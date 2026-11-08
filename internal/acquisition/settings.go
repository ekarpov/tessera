package acquisition

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ekarpov/tessera/pkg/store"
)

const SettingsRetention = 7 * 24 * time.Hour

type Settings struct {
	EveryHours int    `json:"every_hours"`
	Keep       int    `json:"keep"`
	Revision   string `json:"revision"`
}
type SettingsPayload struct {
	ExpectedRevision string `json:"expected_revision"`
	EveryHours       int    `json:"every_hours"`
	Keep             int    `json:"keep"`
}
type SettingsOperation struct {
	ID string `json:"id"`
	SettingsPayload
	State       string     `json:"state"`
	AcceptedAt  time.Time  `json:"accepted_at"`
	ExpiresAt   time.Time  `json:"expires_at"`
	EffectiveAt *time.Time `json:"effective_at,omitempty"`
	Revision    string     `json:"revision,omitempty"`
	Failure     string     `json:"failure,omitempty"`
}
type SettingsExport struct {
	Version    int       `json:"version"`
	Deployment string    `json:"deployment"`
	EveryHours int       `json:"every_hours"`
	Keep       int       `json:"keep"`
	Revision   string    `json:"revision"`
	ExportedAt time.Time `json:"exported_at"`
}
type settingsDisk struct {
	Version     int                          `json:"version"`
	Deployment  string                       `json:"deployment"`
	Scope       string                       `json:"scope"`
	Effective   Settings                     `json:"effective"`
	EffectiveAt time.Time                    `json:"effective_at"`
	ClockFloor  time.Time                    `json:"clock_floor"`
	Requests    map[string]SettingsOperation `json:"requests"`
}

func validSettings(hours, keep int) bool {
	return hours >= 0 && hours <= 87600 && keep >= 1 && keep <= 1024
}
func validRevision(revision, scope string) bool {
	return strings.HasPrefix(revision, scope+".") && validToken(strings.TrimPrefix(revision, scope+"."))
}
func validExportRevision(revision, deployment string) bool {
	parts := strings.Split(revision, ".")
	return len(parts) == 3 && parts[0] == deployment && validToken(parts[1]) && validToken(parts[2])
}
func (m *Manager) writeSettings(d *settingsDisk) error {
	if m.settingsWrite != nil {
		return m.settingsWrite(d)
	}
	return atomicJSON(m.root, "settings.json", d)
}
func cloneSettings(d *settingsDisk) *settingsDisk {
	raw, _ := json.Marshal(d)
	var copy settingsDisk
	_ = json.Unmarshal(raw, &copy)
	return &copy
}
func pendingSettings(d *settingsDisk) string {
	for id, op := range d.Requests {
		if op.State == "pending" {
			return id
		}
	}
	return ""
}
func (m *Manager) loadSettings(hours, keep int, recovery *SettingsExport) error {
	d := &settingsDisk{}
	err := readJSON(filepath.Join(m.root, "settings.json"), d)
	if os.IsNotExist(err) {
		if recovery != nil {
			if !validSettingsExport(recovery) || recovery.Deployment != m.deployment {
				return store.Refuse("invalid_field")
			}
			hours, keep = recovery.EveryHours, recovery.Keep
		}
		now := m.now().UTC()
		scope := m.deployment + "." + token()
		d = &settingsDisk{Version: 1, Deployment: m.deployment, Scope: scope, Effective: Settings{hours, keep, scope + "." + token()}, EffectiveAt: now, ClockFloor: now, Requests: map[string]SettingsOperation{}}
		if err = m.writeSettings(d); err != nil {
			return err
		}
	} else if err != nil {
		return store.Refuse("dataset_unavailable")
	} else if recovery != nil {
		return store.Refuse("conflict")
	}
	parts := strings.Split(d.Scope, ".")
	if d.Version != 1 || d.Deployment != m.deployment || len(parts) != 2 || parts[0] != m.deployment || !validToken(parts[1]) || !validSettings(d.Effective.EveryHours, d.Effective.Keep) || !validRevision(d.Effective.Revision, d.Scope) || d.Requests == nil || len(d.Requests) > maxStatuses || d.ClockFloor.IsZero() || d.EffectiveAt.IsZero() {
		return store.Refuse("dataset_unavailable")
	}
	pending := 0
	for id, op := range d.Requests {
		issued, ok := settingsID(id, d.Scope)
		if !ok || issued.IsZero() || op.ID != id || !validSettings(op.EveryHours, op.Keep) || !validRevision(op.ExpectedRevision, d.Scope) || op.AcceptedAt.IsZero() || op.ExpiresAt.IsZero() {
			return store.Refuse("dataset_unavailable")
		}
		switch op.State {
		case "pending":
			pending++
		case "effective":
			if op.EffectiveAt == nil || !validRevision(op.Revision, d.Scope) {
				return store.Refuse("dataset_unavailable")
			}
		case "failed":
			if op.Failure != "stale_revision" && op.Failure != "settings_pending" {
				return store.Refuse("dataset_unavailable")
			}
		default:
			return store.Refuse("dataset_unavailable")
		}
	}
	if pending > 1 {
		return store.Refuse("dataset_unavailable")
	}
	// The owner lock proves the previous acquisition executor has stopped.
	// Complete accepted commands before the first startup rotation.
	if id := pendingSettings(d); id != "" {
		now := m.now().UTC()
		if now.Before(d.ClockFloor) {
			now = d.ClockFloor
		}
		installSettings(d, id, now)
		if err = m.writeSettings(d); err != nil {
			return err
		}
	}
	m.settings = d
	m.keep = d.Effective.Keep
	return nil
}
func settingsID(id, scope string) (time.Time, bool) {
	prefix := scope + "."
	if !strings.HasPrefix(id, prefix) {
		return time.Time{}, false
	}
	return validID(strings.TrimPrefix(id, prefix))
}
func installSettings(d *settingsDisk, id string, at time.Time) {
	op := d.Requests[id]
	d.Effective = Settings{op.EveryHours, op.Keep, d.Scope + "." + token()}
	d.EffectiveAt = at
	d.ClockFloor = at
	op.State = "effective"
	op.Revision = d.Effective.Revision
	op.EffectiveAt = &at
	op.ExpiresAt = at.Add(SettingsRetention)
	d.Requests[id] = op
}
func settingsFailure(reason string) error { return store.RefuseWith("conflict", reason, nil) }
func rejectedSettings(op SettingsOperation) error {
	return store.RefuseWith("conflict", op.Failure, map[string]any{"operation": op})
}
func (m *Manager) settingsTimeLocked() time.Time {
	now := m.now().UTC()
	if now.Before(m.settings.ClockFloor) {
		return m.settings.ClockFloor
	}
	return now
}

// Replays need no journal write and remain available during application.
func (m *Manager) replaySettingsLocked(id string, payload SettingsPayload) (SettingsOperation, error, bool) {
	if m.settingsUnavailable || m.closed {
		return SettingsOperation{}, store.RefuseWith("dataset_unavailable", "settings_unavailable", nil), true
	}
	op, exists := m.settings.Requests[id]
	if !exists {
		return SettingsOperation{}, nil, false
	}
	if op.SettingsPayload != payload {
		return SettingsOperation{}, settingsFailure("request_reused"), true
	}
	if op.State != "pending" && !m.settingsTimeLocked().Before(op.ExpiresAt) {
		return SettingsOperation{}, store.Refuse("expired"), true
	}
	if op.State == "failed" {
		return op, rejectedSettings(op), true
	}
	return op, nil, true
}

// SubmitSettings durably binds a complete decoded tuple. It never waits on
// acquisition work or bulk cleanup, and uncertainty never becomes a failed result.
func (m *Manager) SubmitSettings(id string, payload SettingsPayload) (SettingsOperation, error) {
	parts := strings.Split(payload.ExpectedRevision, ".")
	validExpected := len(parts) == 3
	for _, part := range parts {
		if !validToken(part) {
			validExpected = false
		}
	}
	if !validExpected || !validSettings(payload.EveryHours, payload.Keep) {
		return SettingsOperation{}, store.Refuse("invalid_field")
	}
	m.mu.Lock()
	replay, replayErr, found := m.replaySettingsLocked(id, payload)
	m.mu.Unlock()
	if found {
		return replay, replayErr
	}
	if !m.settingsMu.TryLock() {
		return SettingsOperation{}, settingsFailure("settings_busy")
	}
	defer func() { m.settingsMu.Unlock(); m.signalControl() }()
	m.mu.Lock()
	if replay, err, found := m.replaySettingsLocked(id, payload); found {
		m.mu.Unlock()
		return replay, err
	}

	d := m.settings
	if !validRevision(payload.ExpectedRevision, d.Scope) {
		m.mu.Unlock()
		return SettingsOperation{}, settingsFailure("request_scope_mismatch")
	}
	issued, ok := settingsID(id, d.Scope)
	if !ok {
		m.mu.Unlock()
		return SettingsOperation{}, settingsFailure("request_scope_mismatch")
	}
	now := m.settingsTimeLocked()

	if issued.Before(now.Add(-5*time.Minute)) || issued.After(now.Add(5*time.Minute)) {
		m.mu.Unlock()
		return SettingsOperation{}, store.Refuse("expired")
	}
	next := cloneSettings(d)
	for key, op := range next.Requests {
		if op.State != "pending" && !now.Before(op.ExpiresAt) {
			delete(next.Requests, key)
		}
	}
	if len(next.Requests) >= maxStatuses {
		m.mu.Unlock()
		return SettingsOperation{}, store.Refuse("limit_exceeded")
	}
	op := SettingsOperation{ID: id, SettingsPayload: payload, State: "pending", AcceptedAt: now, ExpiresAt: now.Add(SettingsRetention)}
	if payload.ExpectedRevision != d.Effective.Revision {
		op.State = "failed"
		op.Failure = "stale_revision"
	} else if pendingSettings(d) != "" {
		op.State = "failed"
		op.Failure = "settings_pending"
	}
	next.Requests[id] = op
	next.ClockFloor = now
	// Give this accepted command priority before releasing the lock to persist.
	if op.State == "pending" {
		m.settingsAdmission = true
	}
	m.mu.Unlock()
	err := m.writeSettings(next)
	m.mu.Lock()
	if err != nil {
		m.settingsUnavailable = true
		m.settingsAdmission = true
	} else {
		m.settings = next
	}
	m.mu.Unlock()
	m.signalControl()
	if err != nil {
		return SettingsOperation{}, store.RefuseWith("dataset_unavailable", "settings_unavailable", nil)
	}
	if op.State == "failed" {
		return op, rejectedSettings(op)
	}
	return op, nil
}
func (m *Manager) SettingsStatus(id string) (SettingsOperation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.settingsUnavailable {
		return SettingsOperation{}, store.RefuseWith("dataset_unavailable", "settings_unavailable", nil)
	}
	issued, ok := settingsID(id, m.settings.Scope)
	if !ok {
		return SettingsOperation{}, settingsFailure("request_scope_mismatch")
	}
	now := m.settingsTimeLocked()
	if op, exists := m.settings.Requests[id]; exists {
		if op.State != "pending" && !now.Before(op.ExpiresAt) {
			return SettingsOperation{}, store.Refuse("expired")
		}
		return op, nil
	}
	if now.Sub(issued) > 5*time.Minute {
		return SettingsOperation{}, store.Refuse("expired")
	}
	return SettingsOperation{}, store.Refuse("not_found")
}
func (m *Manager) applySettings() bool {
	m.mu.Lock()
	pending := pendingSettings(m.settings) != "" && !m.busy && !m.maintenanceActive && !m.closed && !m.settingsUnavailable
	m.mu.Unlock()
	if !pending {
		return false
	}
	if !m.settingsMu.TryLock() {
		return false
	}
	defer m.settingsMu.Unlock()
	m.mu.Lock()
	id := pendingSettings(m.settings)
	if id == "" || m.settingsUnavailable || m.closed || m.busy || m.maintenanceActive {
		m.mu.Unlock()
		return false
	}
	next := cloneSettings(m.settings)
	old := next.Effective
	hook := m.beforeSettingsEffective
	m.mu.Unlock()
	if hook != nil {
		hook()
	}
	m.mu.Lock()
	at := m.settingsTimeLocked()
	m.mu.Unlock()
	installSettings(next, id, at)
	err := m.writeSettings(next)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.settingsUnavailable = true
		m.settingsAdmission = true
		return false
	}
	m.settings = next
	m.keep = next.Effective.Keep
	m.settingsAdmission = false
	m.settingsChangedLocked(old, next.Effective, at)
	return true
}

func validSettingsExport(e *SettingsExport) bool {
	return e != nil && e.Version == 1 && validToken(e.Deployment) && validSettings(e.EveryHours, e.Keep) && !e.ExportedAt.IsZero() && validExportRevision(e.Revision, e.Deployment)
}
