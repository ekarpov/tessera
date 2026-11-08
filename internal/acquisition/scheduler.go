package acquisition

import (
	"context"
	"errors"
	"strconv"
	"time"

	p "github.com/ekarpov/tessera/pkg/protocol"
)

type ScheduledAttempt struct {
	StartedAt   time.Time  `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at"`
	Outcome     string     `json:"outcome"`
	OperationID string     `json:"operation_id,omitempty"`
}
type ScheduledOperation struct {
	ID        string    `json:"id"`
	StartedAt time.Time `json:"started_at"`
}
type SchedulerObservation struct {
	InstanceID               string                `json:"instance_id"`
	ObservedAt               time.Time             `json:"observed_at"`
	HistoryScope             string                `json:"history_scope"`
	Configured               bool                  `json:"configured"`
	Available                bool                  `json:"available"`
	Reason                   string                `json:"reason"`
	Enabled                  bool                  `json:"enabled"`
	Settings                 *Settings             `json:"settings"`
	SettingsScope            *string               `json:"settings_scope"`
	PendingRequestID         *string               `json:"pending_request_id"`
	LastScheduledAttempt     *ScheduledAttempt     `json:"last_scheduled_attempt"`
	ActiveScheduledOperation *ScheduledOperation   `json:"active_scheduled_operation"`
	NextAttemptAt            *time.Time            `json:"next_attempt_at"`
	NextReason               string                `json:"next_reason"`
	Retention                *RetentionObservation `json:"retention"`
	SettingsExport           *SettingsExport       `json:"settings_export"`
}

func NewInstanceID() string { return token() }
func UnconfiguredScheduler(instance string, now time.Time) SchedulerObservation {
	return SchedulerObservation{InstanceID: instance, ObservedAt: now.UTC(), HistoryScope: "process_instance", Reason: "not_configured", NextReason: "not_configured"}
}
func (m *Manager) SchedulerObservation() SchedulerObservation {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := SchedulerObservation{InstanceID: m.instance, ObservedAt: m.now().UTC(), HistoryScope: "process_instance", Configured: true, Available: !m.settingsUnavailable && !m.closed, Reason: "ready"}
	if m.settingsUnavailable {
		out.Reason = "settings_unavailable"
		out.NextReason = "settings_unavailable"
		return out
	}
	s := m.settings.Effective
	scope := m.settings.Scope
	out.Settings = &s
	out.SettingsScope = &scope
	out.Enabled = s.EveryHours > 0
	if id := pendingSettings(m.settings); id != "" {
		out.PendingRequestID = &id
	}
	out.SettingsExport = &SettingsExport{Version: 1, Deployment: m.deployment, EveryHours: s.EveryHours, Keep: s.Keep, Revision: s.Revision, ExportedAt: out.ObservedAt}
	retention := m.retentionLocked()
	out.Retention = &retention
	if m.lastScheduled != nil {
		attempt := *m.lastScheduled
		out.LastScheduledAttempt = &attempt
		if attempt.Outcome == "in_progress" && attempt.OperationID != "" {
			out.ActiveScheduledOperation = &ScheduledOperation{attempt.OperationID, attempt.StartedAt}
		}
	}
	switch {
	case m.closed:
		out.Reason = "acquisition_unavailable"
		out.NextReason = "acquisition_unavailable"
	case out.ActiveScheduledOperation != nil:
		out.NextReason = "in_progress"
	case out.PendingRequestID != nil || m.settingsAdmission:
		out.NextReason = "settings_pending"
	case !m.schedulerStarted:
		if m.schedulerEverStarted {
			out.NextReason = "stopped"
		} else {
			out.NextReason = "not_started"
		}
	case !out.Enabled:
		out.NextReason = "disabled"
	default:
		out.NextReason = "scheduled"
		if m.nextScheduled != nil {
			at := *m.nextScheduled
			out.NextAttemptAt = &at
		}
	}
	return out
}
func (m *Manager) settingsChangedLocked(old, next Settings, at time.Time) {
	if !m.schedulerStarted {
		return
	}
	if next.EveryHours == 0 {
		m.nextScheduled = nil
		return
	}
	if old.EveryHours == 0 {
		m.nextScheduled = &at
	} else if old.EveryHours != next.EveryHours {
		due := at.Add(time.Duration(next.EveryHours) * time.Hour)
		m.nextScheduled = &due
	}
}
func (m *Manager) StartScheduler(ctx context.Context) {
	m.mu.Lock()
	if m.schedulerEverStarted || m.closed {
		m.mu.Unlock()
		return
	}
	m.schedulerStarted = true
	m.schedulerEverStarted = true
	if m.settings.Effective.EveryHours > 0 {
		at := m.now().UTC()
		m.nextScheduled = &at
	}
	m.wg.Add(1)
	m.mu.Unlock()
	m.signalControl()
	go func() {
		defer m.wg.Done()
		select {
		case <-ctx.Done():
		case <-m.controlCtx.Done():
		}
		m.mu.Lock()
		m.schedulerStarted = false
		m.nextScheduled = nil
		m.mu.Unlock()
		m.signalControl()
	}()
}
func (m *Manager) signalControl() {
	if m.controlWake != nil {
		select {
		case m.controlWake <- struct{}{}:
		default:
		}
	}
}
func (m *Manager) startControl() {
	m.instance = token()
	m.controlWake = make(chan struct{}, 1)
	m.controlCtx, m.stopControl = context.WithCancel(context.Background())
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		timer := time.NewTimer(0)
		defer timer.Stop()
		for {
			select {
			case <-m.controlCtx.Done():
				return
			case <-m.controlWake:
			case <-timer.C:
			}
			m.stepScheduled()
			if m.applySettings() {
				m.signalControl()
			}
			m.stepScheduled()
			if m.pruneOne() {
				m.signalControl()
			}
			m.mu.Lock()
			delay := 30 * time.Second
			if m.schedulerStarted && m.nextScheduled != nil && !m.settingsAdmission && pendingSettings(m.settings) == "" && !m.settingsUnavailable && !m.closed {
				until := m.nextScheduled.Sub(m.now())
				if until < 0 {
					until = 0
				}
				if until < delay {
					delay = until
				}
			}
			m.mu.Unlock()
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(delay)
		}
	}()
}

// Finish at the producer boundary, before another producer can retire this set.
// The entry fallback also handles a capture that finishes before Start returns.
func (m *Manager) finishScheduledLocked(e *entry) {
	if e == nil || e.finishedAt.IsZero() || m.lastScheduled == nil || m.lastScheduled.OperationID != e.ID || m.lastScheduled.Outcome != "in_progress" {
		return
	}
	attempt := *m.lastScheduled
	finished := e.finishedAt
	attempt.FinishedAt = &finished
	switch e.finishedState {
	case "complete":
		attempt.Outcome = "succeeded"
	case "cancelled":
		attempt.Outcome = "cancelled"
	default:
		attempt.Outcome = "failed"
	}
	m.lastScheduled = &attempt
	if m.schedulerStarted && m.settings.Effective.EveryHours > 0 {
		due := finished.Add(time.Duration(m.settings.Effective.EveryHours) * time.Hour)
		m.nextScheduled = &due
	}
}
func (m *Manager) stepScheduled() {
	m.mu.Lock()
	now := m.now().UTC()
	if m.lastScheduled != nil {
		m.finishScheduledLocked(m.entries[m.lastScheduled.OperationID])
	}

	if !m.schedulerStarted || m.closed || m.settingsUnavailable || m.settingsAdmission || pendingSettings(m.settings) != "" || m.nextScheduled == nil || now.Before(*m.nextScheduled) {
		m.mu.Unlock()
		return
	}
	id := strconv.FormatInt(now.Unix(), 10) + "-" + token()
	m.mu.Unlock()
	op, err := m.Start(id, "backup")
	m.mu.Lock()
	defer m.mu.Unlock()
	attempt := &ScheduledAttempt{StartedAt: now, Outcome: "in_progress", OperationID: op.ID}
	m.nextScheduled = nil
	if err != nil {
		attempt.OperationID = ""
		finished := m.now().UTC()
		attempt.Outcome = "refused_capacity"
		attempt.FinishedAt = &finished
		var refusal *p.Error
		if errors.As(err, &refusal) && refusal.Details["reason"] == "busy" {
			attempt.Outcome = "refused_busy"
		}
		if p.ErrorCode(err) != "limit_exceeded" {
			attempt.Outcome = "refused_unavailable"
		}
		if errors.As(err, &refusal) && refusal.Details["reason"] == "settings_pending" {
			attempt.Outcome = "refused_settings_pending"
		}
		due := finished.Add(time.Duration(m.settings.Effective.EveryHours) * time.Hour)
		m.nextScheduled = &due
	}
	m.lastScheduled = attempt
	if err == nil {
		m.finishScheduledLocked(m.entries[op.ID])
	}
}
