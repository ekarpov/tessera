package acquisition

import (
	"context"
	"github.com/ekarpov/tessera/pkg/store"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func advanceClock(m *Manager, at time.Time) {
	m.mu.Lock()
	m.now = func() time.Time { return at }
	m.mu.Unlock()
	m.signalControl()
}
func TestSchedulerCadenceSettingsAndInstanceHistory(t *testing.T) {
	var calls atomic.Int32
	entered, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	m := nativeFixture(t, 1, 8, func(ctx context.Context, kind, path string) (string, string, []byte, error) {
		if calls.Load() == 0 {
			close(entered)
			<-resume
		}
		return testCapture(&calls)(ctx, kind, path)
	})
	t.Cleanup(func() { once.Do(func() { close(resume) }) })
	base := time.Now().Add(time.Minute).UTC()
	advanceClock(m, base)
	m.StartScheduler(t.Context())
	<-entered
	observed := m.SchedulerObservation()
	if observed.LastScheduledAttempt == nil || observed.ActiveScheduledOperation == nil || observed.NextAttemptAt != nil || observed.NextReason != "in_progress" {
		t.Fatal("active observation", observed)
	}
	// A long attempt consumes no queued ticks. The interval changes only at the
	// acquisition's safe terminal boundary, then the next attempt is finish+2h.
	end := base.Add(3 * time.Hour)
	advanceClock(m, end)
	id, payload := settingsRequest(m, 2, 8)
	if _, err := m.SubmitSettings(id, payload); err != nil {
		t.Fatal(err)
	}
	if observed = m.SchedulerObservation(); observed.Settings.EveryHours != 1 {
		t.Fatal("settings changed during scheduled creation")
	}
	once.Do(func() { close(resume) })
	waitEffective(t, m, id)
	eventually(t, func() bool { return m.SchedulerObservation().LastScheduledAttempt.Outcome == "succeeded" })
	observed = m.SchedulerObservation()
	due := end.Add(2 * time.Hour)
	if calls.Load() != 1 || observed.NextAttemptAt == nil || !observed.NextAttemptAt.Equal(due) || !observed.LastScheduledAttempt.FinishedAt.Equal(end) {
		t.Fatal("queued ticker or wrong completion cadence", observed, calls.Load())
	}
	// Retention-only settings and manual creation cannot reset the due time.
	changeSettings(t, m, 2, 7)
	manualTime := end.Add(time.Hour)
	advanceClock(m, manualTime)
	manualID := NewID()
	m.mu.Lock()
	issued := m.now().Unix()
	m.mu.Unlock()
	manualID = fmtID(issued)
	if _, err := m.Start(manualID, "evidence"); err != nil {
		t.Fatal(err)
	}
	waitTerminal(t, m, manualID)
	observed = m.SchedulerObservation()
	if observed.LastScheduledAttempt.OperationID == manualID || !observed.NextAttemptAt.Equal(due) {
		t.Fatal("manual evidence reset scheduled history")
	}
	advanceClock(m, due)
	eventually(t, func() bool {
		a := m.SchedulerObservation().LastScheduledAttempt
		return a.StartedAt.Equal(due) && a.Outcome == "succeeded"
	})
	m.StartScheduler(t.Context()) // idempotent start cannot create another attempt
	disable := changeSettings(t, m, 0, 7)
	if disable.State != "effective" {
		t.Fatal(disable)
	}
	disabled := m.SchedulerObservation()
	if disabled.Enabled || disabled.NextAttemptAt != nil || disabled.NextReason != "disabled" {
		t.Fatal(disabled)
	}
	before := calls.Load()
	advanceClock(m, due.Add(8*time.Hour))
	changeSettings(t, m, 2, 7)
	eventually(t, func() bool {
		return calls.Load() == before+1 && m.SchedulerObservation().LastScheduledAttempt.Outcome == "succeeded"
	})
}
func fmtID(seconds int64) string { return strconv.FormatInt(seconds, 10) + "-" + token() }
func TestScheduledBusyRefusalAndFailureHaveBoundedCadence(t *testing.T) {
	var calls atomic.Int32
	entered, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	m := nativeFixture(t, 1, 4, func(ctx context.Context, kind, path string) (string, string, []byte, error) {
		if calls.Load() == 0 {
			close(entered)
			<-resume
			return testCapture(&calls)(ctx, kind, path)
		}
		calls.Add(1)
		return "", "", nil, store.Refuse("store_unavailable")
	})
	t.Cleanup(func() { once.Do(func() { close(resume) }) })
	base := time.Now().Add(time.Minute)
	advanceClock(m, base)
	manual := fmtID(base.Unix())
	if _, err := m.Start(manual, "backup"); err != nil {
		t.Fatal(err)
	}
	<-entered
	m.StartScheduler(t.Context())
	eventually(t, func() bool { return m.SchedulerObservation().LastScheduledAttempt != nil })
	refused := m.SchedulerObservation()
	if refused.LastScheduledAttempt.Outcome != "refused_busy" || refused.LastScheduledAttempt.OperationID != "" || !refused.NextAttemptAt.Equal(base.Add(time.Hour)) {
		t.Fatal(refused)
	}
	advanceClock(m, base.Add(30*time.Minute))
	once.Do(func() { close(resume) })
	waitTerminal(t, m, manual)
	if !m.SchedulerObservation().NextAttemptAt.Equal(base.Add(time.Hour)) {
		t.Fatal("manual completion reset cadence")
	}
	advanceClock(m, base.Add(time.Hour))
	eventually(t, func() bool { return m.SchedulerObservation().LastScheduledAttempt.Outcome == "failed" })
	observed := m.SchedulerObservation()
	if !observed.NextAttemptAt.Equal(base.Add(2*time.Hour)) || observed.LastScheduledAttempt.OperationID == "" {
		t.Fatal(observed)
	}
}

func TestScheduledCompletionUsesOwnBoundaryDuringLaterManualWork(t *testing.T) {
	var calls atomic.Int32
	entered, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	m := nativeFixture(t, 1, 4, func(ctx context.Context, kind, path string) (string, string, []byte, error) {
		if calls.Load() == 1 {
			close(entered)
			<-resume
		}
		return testCapture(&calls)(ctx, kind, path)
	})
	t.Cleanup(func() { once.Do(func() { close(resume) }) })
	// Drive the worker deterministically after its normal goroutine has stopped.
	m.stopControl()
	m.wg.Wait()
	base := time.Now().UTC()
	advanceClock(m, base)
	first := fmtID(base.Unix())
	if _, err := m.Start(first, "backup"); err != nil {
		t.Fatal(err)
	}
	waitTerminal(t, m, first)
	m.mu.Lock()
	m.schedulerStarted = true
	m.lastScheduled = &ScheduledAttempt{StartedAt: base, Outcome: "in_progress", OperationID: first}
	m.mu.Unlock()
	advanceClock(m, base.Add(10*time.Minute))
	second := fmtID(base.Add(10 * time.Minute).Unix())
	if _, err := m.Start(second, "evidence"); err != nil {
		t.Fatal(err)
	}
	<-entered
	// A later publication/retirement can change the entry's inventory state;
	// the scheduled outcome still describes its own terminal publication.
	m.mu.Lock()
	m.entries[first].State = "expired"
	m.mu.Unlock()
	m.stepScheduled()
	got := m.SchedulerObservation()
	if got.LastScheduledAttempt.Outcome != "succeeded" || !got.LastScheduledAttempt.FinishedAt.Equal(base) || got.ActiveScheduledOperation != nil || !got.NextAttemptAt.Equal(base.Add(time.Hour)) {
		t.Fatal("later producer shifted scheduled completion", got)
	}
	once.Do(func() { close(resume) })
	waitTerminal(t, m, second)
}

func TestIdleIntervalChangeAndSchedulerStop(t *testing.T) {
	var calls atomic.Int32
	m := nativeFixture(t, 1, 4, testCapture(&calls))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	base := time.Now().Add(time.Minute).UTC()
	advanceClock(m, base)
	m.StartScheduler(ctx)
	eventually(t, func() bool {
		a := m.SchedulerObservation().LastScheduledAttempt
		return a != nil && a.Outcome == "succeeded"
	})
	changedAt := base.Add(20 * time.Minute)
	advanceClock(m, changedAt)
	changeSettings(t, m, 3, 4)
	if got := m.SchedulerObservation(); got.NextAttemptAt == nil || !got.NextAttemptAt.Equal(changedAt.Add(3*time.Hour)) {
		t.Fatal(got)
	}
	cancel()
	eventually(t, func() bool { return m.SchedulerObservation().NextReason == "stopped" })
	if m.SchedulerObservation().NextAttemptAt != nil {
		t.Fatal("stopped schedule has a due time")
	}
}
