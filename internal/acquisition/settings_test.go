package acquisition

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	p "github.com/ekarpov/tessera/pkg/protocol"
)

func nativeFixture(t *testing.T, hours, keep int, capture Capture) *Manager {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	m, err := OpenNative(dir, bytes.Repeat([]byte{9}, 32), hours, keep, nil, capture)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m
}
func eventually(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		if f() {
			return
		}
		select {
		case <-deadline:
			t.Fatal("condition did not become true")
		case <-time.After(time.Millisecond):
		}
	}
}
func settingsRequest(m *Manager, hours, keep int) (string, SettingsPayload) {
	observed := m.SchedulerObservation()
	return *observed.SettingsScope + "." + fmt.Sprintf("%d-%s", observed.ObservedAt.Unix(), token()), SettingsPayload{observed.Settings.Revision, hours, keep}
}
func waitEffective(t *testing.T, m *Manager, id string) SettingsOperation {
	t.Helper()
	var op SettingsOperation
	eventually(t, func() bool {
		var err error
		op, err = m.SettingsStatus(id)
		if err != nil {
			t.Fatal(err)
		}
		return op.State == "effective"
	})
	return op
}
func changeSettings(t *testing.T, m *Manager, hours, keep int) SettingsOperation {
	t.Helper()
	id, payload := settingsRequest(m, hours, keep)
	eventually(t, func() bool {
		op, err := m.SubmitSettings(id, payload)
		if err != nil && op.ID != "" {
			t.Fatal(err)
		}
		return err == nil
	})
	return waitEffective(t, m, id)
}
func TestSettingsBindingPendingRevisionsAndAcquisitionPriority(t *testing.T) {
	for _, kind := range []string{"backup", "evidence"} {
		t.Run(kind, func(t *testing.T) {
			var calls atomic.Int32
			entered, resume := make(chan struct{}), make(chan struct{})
			var once sync.Once
			m := nativeFixture(t, 0, 4, func(ctx context.Context, kind, path string) (string, string, []byte, error) {
				close(entered)
				<-resume
				return testCapture(&calls)(ctx, kind, path)
			})
			t.Cleanup(func() { once.Do(func() { close(resume) }) })
			acquisitionID := NewID()
			if _, err := m.Start(acquisitionID, kind); err != nil {
				t.Fatal(err)
			}
			<-entered
			id, payload := settingsRequest(m, 12, 2)
			op, err := m.SubmitSettings(id, payload)
			if err != nil || op.State != "pending" {
				t.Fatal(op, err)
			}
			if observed := m.SchedulerObservation(); observed.Settings.Keep != 4 || observed.PendingRequestID == nil {
				t.Fatal("policy became effective during acquisition")
			}
			if retry, err := m.SubmitSettings(id, payload); err != nil || retry.State != "pending" {
				t.Fatal("lost acceptance", err)
			}
			changed := payload
			changed.Keep = 3
			if _, err = m.SubmitSettings(id, changed); p.ErrorCode(err) != "conflict" {
				t.Fatal("conflicting ID reuse", err)
			}
			if _, err = m.Start(NewID(), "backup"); p.ErrorCode(err) != "limit_exceeded" {
				t.Fatal("pending command starved by new creation", err)
			}
			rejectedID := *m.SchedulerObservation().SettingsScope + "." + NewID()
			var rejected SettingsOperation
			eventually(t, func() bool { rejected, err = m.SubmitSettings(rejectedID, payload); return rejected.ID != "" })
			if p.ErrorCode(err) != "conflict" || rejected.State != "failed" || rejected.Failure != "settings_pending" {
				t.Fatal("second pending admission", rejected, err)
			}
			once.Do(func() { close(resume) })
			effective := waitEffective(t, m, id)
			retry, err := m.SubmitSettings(id, payload)
			if err != nil || retry.Revision != effective.Revision {
				t.Fatal("effective retry rechecked current revision", err)
			}
			staleID := *m.SchedulerObservation().SettingsScope + "." + NewID()
			stale, err := m.SubmitSettings(staleID, payload)
			if p.ErrorCode(err) != "conflict" || stale.Failure != "stale_revision" {
				t.Fatal(stale, err)
			}
			stored, err := m.SettingsStatus(staleID)
			if err != nil || stored.State != "failed" {
				t.Fatal("rejection was not retained", err)
			}
		})
	}
}
func TestSettingsPersistenceUncertaintyAndRestart(t *testing.T) {
	for _, boundary := range []string{"accept_after_rename", "effective_before_write", "effective_after_rename"} {
		t.Run(boundary, func(t *testing.T) {
			var calls atomic.Int32
			dir := t.TempDir()
			key := bytes.Repeat([]byte{7}, 32)
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			m, err := OpenNative(dir, key, 0, 4, nil, testCapture(&calls))
			if err != nil {
				t.Fatal(err)
			}
			id, payload := settingsRequest(m, 8, 2)
			var durableRevision string
			m.settingsMu.Lock()
			m.settingsWrite = func(d *settingsDisk) error {
				op, ok := d.Requests[id]
				trigger := ok && (boundary == "accept_after_rename" && op.State == "pending" || strings.HasPrefix(boundary, "effective_") && op.State == "effective")
				if trigger && boundary == "effective_before_write" {
					return errors.New("synthetic persistence failure")
				}
				err := atomicJSON(m.root, "settings.json", d)
				if trigger && err == nil {
					durableRevision = d.Effective.Revision
					return errors.New("synthetic uncertain directory sync")
				}
				return err
			}
			m.settingsMu.Unlock()
			_, err = m.SubmitSettings(id, payload)
			if boundary == "accept_after_rename" && p.ErrorCode(err) != "dataset_unavailable" {
				m.Close()
				t.Fatal(err)
			}
			eventually(t, func() bool { return m.SchedulerObservation().Reason == "settings_unavailable" })
			if _, err = m.SettingsStatus(id); p.ErrorCode(err) != "dataset_unavailable" {
				m.Close()
				t.Fatal("uncertainty reported as definite outcome", err)
			}
			if _, err = m.Start(NewID(), "backup"); p.ErrorCode(err) != "dataset_unavailable" {
				m.Close()
				t.Fatal(err)
			}
			m.Close()
			m, err = OpenNative(dir, key, 99, 99, nil, testCapture(&calls))
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			op, err := m.SettingsStatus(id)
			if err != nil || op.State != "effective" || m.SchedulerObservation().Settings.Keep != 2 {
				t.Fatal(op, err)
			}
			if boundary == "effective_after_rename" && op.Revision != durableRevision {
				t.Fatal("recovered effective command applied twice")
			}
			retry, err := m.SubmitSettings(id, payload)
			if err != nil || retry.Revision != op.Revision || calls.Load() != 0 {
				t.Fatal("recovery reran command or export", err)
			}
		})
	}
}
func TestSettingsDurablePrecedenceExportAndCorruption(t *testing.T) {
	var calls atomic.Int32
	dir := t.TempDir()
	key := bytes.Repeat([]byte{8}, 32)
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	m, err := OpenNative(dir, key, 0, 5, nil, testCapture(&calls))
	if err != nil {
		t.Fatal(err)
	}
	changeSettings(t, m, 4, 2)
	observed := m.SchedulerObservation()
	export := *observed.SettingsExport
	oldScope := *observed.SettingsScope
	m.Close()
	m, err = OpenNative(dir, key, 0, 1, nil, testCapture(&calls))
	if err != nil {
		t.Fatal(err)
	}
	if got := m.SchedulerObservation(); got.Settings.EveryHours != 4 || got.Settings.Keep != 2 || got.Settings.Revision != observed.Settings.Revision || got.InstanceID == observed.InstanceID || got.LastScheduledAttempt != nil {
		m.Close()
		t.Fatal("bootstrap overrode durable policy or observations survived restart")
	}
	m.Close()
	path := filepath.Join(dir, ".acquisitions", "settings.json")
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	m, err = OpenNative(dir, key, 0, 1, &export, testCapture(&calls))
	if err != nil {
		t.Fatal(err)
	}
	if got := m.SchedulerObservation(); got.Settings.Keep != 2 || *got.SettingsScope == oldScope || got.Settings.Revision == export.Revision {
		m.Close()
		t.Fatal("recovery preserved old revision scope")
	}
	if _, err = m.SettingsStatus(oldScope + "." + NewID()); p.ErrorCode(err) != "conflict" {
		m.Close()
		t.Fatal("old request revived after recovery", err)
	}
	m.Close()
	if err = os.WriteFile(path, []byte("malformed"), 0600); err != nil {
		t.Fatal(err)
	}
	if restored, err := OpenNative(dir, key, 0, 1, nil, testCapture(&calls)); err == nil {
		restored.Close()
		t.Fatal("corrupt policy fell back to static config")
	}
}
func TestSettingsBoundsExpiryAndClockFloor(t *testing.T) {
	var calls atomic.Int32
	m := nativeFixture(t, 0, 4, testCapture(&calls))
	id, payload := settingsRequest(m, 0, 4)
	if _, err := m.SubmitSettings(id, payload); err != nil {
		t.Fatal(err)
	}
	waitEffective(t, m, id)
	future := time.Now().Add(8 * 24 * time.Hour)
	m.mu.Lock()
	m.now = func() time.Time { return future }
	m.mu.Unlock()
	current := m.SchedulerObservation()
	newID := *current.SettingsScope + "." + fmt.Sprintf("%d-%s", future.Unix(), token())
	if _, err := m.SubmitSettings(newID, SettingsPayload{current.Settings.Revision, 0, 3}); err != nil {
		t.Fatal(err)
	}
	waitEffective(t, m, newID)
	m.mu.Lock()
	m.now = time.Now
	m.mu.Unlock()
	if _, err := m.SubmitSettings(id, payload); p.ErrorCode(err) != "expired" {
		t.Fatal("forgotten ID revived after clock rollback", err)
	}
	for _, invalid := range []SettingsPayload{{current.Settings.Revision, -1, 1}, {current.Settings.Revision, 87601, 1}, {current.Settings.Revision, 0, 0}, {current.Settings.Revision, 0, 1025}} {
		if _, err := m.SubmitSettings(*current.SettingsScope+"."+NewID(), invalid); p.ErrorCode(err) != "invalid_field" {
			t.Fatal("invalid bounds admitted", err)
		}
	}
}

func TestConcurrentAdministratorsAndPendingRequestSurvivesExpiry(t *testing.T) {
	var calls atomic.Int32
	entered, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	m := nativeFixture(t, 0, 4, func(ctx context.Context, kind, path string) (string, string, []byte, error) {
		close(entered)
		<-resume
		return testCapture(&calls)(ctx, kind, path)
	})
	t.Cleanup(func() { once.Do(func() { close(resume) }) })
	if _, err := m.Start(NewID(), "backup"); err != nil {
		t.Fatal(err)
	}
	<-entered
	ids := make([]string, 2)
	payloads := make([]SettingsPayload, 2)
	for i := range ids {
		ids[i], payloads[i] = settingsRequest(m, i+1, i+2)
	}
	results := make(chan SettingsOperation, 2)
	for i := range ids {
		go func(i int) {
			for {
				op, err := m.SubmitSettings(ids[i], payloads[i])
				if op.ID != "" {
					results <- op
					return
				}
				if p.ErrorCode(err) != "conflict" {
					results <- SettingsOperation{}
					return
				}
				time.Sleep(time.Millisecond)
			}
		}(i)
	}
	var pending SettingsOperation
	deadline := time.After(5 * time.Second)
	for range ids {
		select {
		case op := <-results:
			if op.State == "pending" {
				if pending.ID != "" {
					t.Fatal("two pending commands")
				}
				pending = op
			} else if op.State != "failed" || op.Failure != "settings_pending" {
				t.Fatal(op)
			}
		case <-deadline:
			t.Fatal("concurrent admission did not complete")
		}
	}
	if pending.ID == "" {
		t.Fatal("no accepted command")
	}
	later := time.Now().Add(8 * 24 * time.Hour)
	advanceClock(m, later)
	if op, err := m.SettingsStatus(pending.ID); err != nil || op.State != "pending" {
		t.Fatal("pending request expired", op, err)
	}
	if op, err := m.SubmitSettings(pending.ID, pending.SettingsPayload); err != nil || op.State != "pending" {
		t.Fatal("pending retry expired", op, err)
	}
	once.Do(func() { close(resume) })
	effective := waitEffective(t, m, pending.ID)
	if !effective.ExpiresAt.Equal(later.Add(SettingsRetention)) {
		t.Fatal("resolution did not renew outcome retention")
	}
}

func TestSettingsRequestCapacityPurgesOnlyExpiredOutcomes(t *testing.T) {
	var calls atomic.Int32
	m := nativeFixture(t, 0, 4, testCapture(&calls))
	m.stopControl()
	m.wg.Wait()
	now := time.Now().UTC()
	advanceClock(m, now)
	m.mu.Lock()
	for range maxStatuses {
		id := m.settings.Scope + "." + fmtID(now.Unix())
		m.settings.Requests[id] = SettingsOperation{ID: id, SettingsPayload: SettingsPayload{m.settings.Effective.Revision, 0, 4}, State: "failed", Failure: "stale_revision", AcceptedAt: now, ExpiresAt: now.Add(SettingsRetention)}
	}
	m.mu.Unlock()
	id, payload := settingsRequest(m, 0, 3)
	if _, err := m.SubmitSettings(id, payload); p.ErrorCode(err) != "limit_exceeded" {
		t.Fatal("unbounded outcomes", err)
	}
	advanceClock(m, now.Add(8*24*time.Hour))
	next, payload := settingsRequest(m, 0, 3)
	if _, err := m.SubmitSettings(next, payload); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	count := len(m.settings.Requests)
	m.mu.Unlock()
	if count != 1 {
		t.Fatal("expired outcomes not purged", count)
	}
	if _, err := m.SubmitSettings(id, payload); p.ErrorCode(err) != "expired" {
		t.Fatal("forgotten ID admitted", err)
	}
}

func TestSettingsAcceptanceFailureBeforeWriteIsUncertainUntilRestart(t *testing.T) {
	var calls atomic.Int32
	m := nativeFixture(t, 0, 3, testCapture(&calls))
	id, payload := settingsRequest(m, 12, 2)
	m.settingsMu.Lock()
	m.settingsWrite = func(*settingsDisk) error { return errors.New("synthetic pre-write failure") }
	m.settingsMu.Unlock()
	if _, err := m.SubmitSettings(id, payload); p.ErrorCode(err) != "dataset_unavailable" {
		t.Fatal(err)
	}
	dir, key := filepath.Dir(m.root), append([]byte(nil), m.key...)
	m.Close()
	recovered, err := OpenNative(dir, key, 0, 3, nil, testCapture(&calls))
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if _, err := recovered.SettingsStatus(id); p.ErrorCode(err) != "not_found" {
		t.Fatal("absent acceptance invented", err)
	}
	if _, err := recovered.SubmitSettings(id, payload); err != nil {
		t.Fatal(err)
	}
	waitEffective(t, recovered, id)
}

func TestSettingsBootstrapRequiresExistingMatchingDeployment(t *testing.T) {
	var calls atomic.Int32
	m := nativeFixture(t, 0, 3, testCapture(&calls))
	export := *m.SchedulerObservation().SettingsExport
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if other, err := OpenNative(dir, m.key, 0, 1, &export, testCapture(&calls)); err == nil {
		other.Close()
		t.Fatal("bootstrap minted a replacement deployment")
	}
	if _, err := os.Stat(filepath.Join(dir, ".acquisitions", "deployment.json")); !os.IsNotExist(err) {
		t.Fatal("rejected bootstrap wrote a deployment identity", err)
	}
	// A different provisioned deployment also cannot import these settings.
	other := nativeFixture(t, 0, 1, testCapture(&calls))
	other.Close()
	if err := os.Remove(filepath.Join(other.root, "settings.json")); err != nil {
		t.Fatal(err)
	}
	if imported, err := OpenNative(filepath.Dir(other.root), other.key, 0, 1, &export, testCapture(&calls)); err == nil {
		imported.Close()
		t.Fatal("foreign deployment export accepted")
	}
}
