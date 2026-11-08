package acquisition

import (
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	p "github.com/ekarpov/tessera/pkg/protocol"
)

func publishSet(t *testing.T, m *Manager, kind string) Operation {
	t.Helper()
	id := NewID()
	if _, err := m.Start(id, kind); err != nil {
		t.Fatal(err)
	}
	return waitTerminal(t, m, id)
}
func TestRetentionReductionProtectsLeasesStreamsAndFinalArchive(t *testing.T) {
	var calls atomic.Int32
	m := nativeFixture(t, 0, 3, testCapture(&calls))
	archive := publishSet(t, m, "backup")
	first := publishSet(t, m, "evidence")
	second := publishSet(t, m, "evidence")
	if _, err := m.Lease(first.ID, testLease, false); err != nil {
		t.Fatal(err)
	}
	descriptor := first.Artifacts[len(first.Artifacts)-1]
	read, _, err := m.Read(first.ID, descriptor.ID, testLease)
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	if _, err = m.Lease(first.ID, testLease, true); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Lease(second.ID, testLease, false); err != nil {
		t.Fatal(err)
	}
	changed := changeSettings(t, m, 0, 1)
	if changed.State != "effective" {
		t.Fatal(changed)
	}
	eventually(t, func() bool { return m.SchedulerObservation().Retention.State == "blocked" })
	observed := m.SchedulerObservation()
	if observed.Settings.Keep != 1 || observed.Retention.Retained != 3 || observed.Retention.Reason != "transfers" {
		t.Fatal(observed)
	}
	if _, err = m.Start(NewID(), "backup"); p.ErrorCode(err) != "limit_exceeded" {
		t.Fatal("over-target inventory admitted new creation", err)
	}
	if page, err := m.Catalog(100, ""); err != nil || len(page.Results) != 3 {
		t.Fatal("collection unavailable during deferral", err)
	}
	if _, err = m.Lease(second.ID, testLease, true); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return m.SchedulerObservation().Retention.Retained == 2 })
	raw, err := io.ReadAll(read)
	if err != nil || int64(len(raw)) != descriptor.Size {
		t.Fatal("admitted stream lost bytes", err)
	}
	read.Close()
	eventually(t, func() bool { return m.SchedulerObservation().Retention.State == "converged" })
	page, err := m.Catalog(100, "")
	if err != nil || len(page.Results) != 1 || page.Results[0].ID != archive.ID {
		t.Fatal("newer evidence replaced final archive", err)
	}
	if _, err = m.Start(NewID(), "evidence"); p.ErrorCode(err) != "limit_exceeded" {
		t.Fatal("evidence evicted final recovery pair", err)
	}
	// A replacement archive may retire the final pair only after publication.
	entered, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	m.mu.Lock()
	m.beforePublish = func() { close(entered); <-resume }
	m.mu.Unlock()
	t.Cleanup(func() { once.Do(func() { close(resume) }) })
	path, err := m.ArchivePath(archive.ID)
	if err != nil {
		t.Fatal(err)
	}
	next := NewID()
	if _, err = m.Start(next, "backup"); err != nil {
		t.Fatal(err)
	}
	<-entered
	if _, err = os.Stat(path); err != nil {
		t.Fatal("last recovery pair deleted before replacement", err)
	}
	once.Do(func() { close(resume) })
	waitTerminal(t, m, next)
}
func TestRetentionCleanupStallsAndFailuresDoNotRollbackPolicyOrBlockReaders(t *testing.T) {
	var calls atomic.Int32
	m := nativeFixture(t, 0, 3, testCapture(&calls))
	publishSet(t, m, "backup")
	publishSet(t, m, "backup")
	latest := publishSet(t, m, "backup")
	entered, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	m.mu.Lock()
	m.beforePrune = func() { once.Do(func() { close(entered); <-resume }) }
	m.removeSet = func(string) error { return os.ErrPermission }
	m.mu.Unlock()
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(resume) }) })
	changed := changeSettings(t, m, 0, 1)
	<-entered
	if op, err := m.SettingsStatus(changed.ID); err != nil || op.State != "effective" {
		t.Fatal("cleanup delayed effectiveness", err)
	}
	if _, err := m.Catalog(100, ""); err != nil {
		t.Fatal("catalog blocked during deletion", err)
	}
	if _, err := m.Lease(latest.ID, testLease, false); err != nil {
		t.Fatal(err)
	}
	read, _, err := m.Read(latest.ID, latest.Artifacts[0].ID, testLease)
	if err != nil {
		t.Fatal(err)
	}
	if raw, err := io.ReadAll(read); err != nil || len(raw) == 0 {
		t.Fatal(err)
	}
	read.Close()
	// Deletion intent is durable while the filesystem operation is paused.
	var disk map[string]*entry
	if err = readJSON(filepath.Join(m.root, "operations.json"), &disk); err != nil {
		t.Fatal(err)
	}
	intent := false
	for _, e := range disk {
		if e.DeletePending && e.State == "expired" {
			intent = true
		}
	}
	if !intent {
		t.Fatal("deletion preceded durable intent")
	}
	release.Do(func() { close(resume) })
	eventually(t, func() bool { return m.SchedulerObservation().Retention.Reason == "cleanup_failed" })
	observed := m.SchedulerObservation()
	if observed.Settings.Revision != changed.Revision || observed.Settings.Keep != 1 {
		t.Fatal("cleanup failure rolled back effective settings")
	}
	m.mu.Lock()
	m.removeSet = nil
	m.mu.Unlock()
	m.signalControl()
	if _, err = m.Lease(latest.ID, testLease, true); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return m.SchedulerObservation().Retention.State == "converged" })
}

func TestStartupLoadsPolicyBeforeRotationAndRecoversDeletionIntent(t *testing.T) {
	var calls atomic.Int32
	m := nativeFixture(t, 0, 3, testCapture(&calls))
	publishSet(t, m, "backup")
	publishSet(t, m, "backup")
	publishSet(t, m, "backup")
	dir := filepath.Dir(m.root)
	key := append([]byte(nil), m.key...)
	m.Close()
	reopened, err := OpenNative(dir, key, 0, 1, nil, testCapture(&calls))
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.SchedulerObservation(); got.Settings.Keep != 3 || got.Retention.Retained != 3 {
		reopened.Close()
		t.Fatal("static config rotated before durable policy", got)
	}
	// Crash-state fixture: effective policy and one retirement intent are durable;
	// neither physical deletion nor the remaining excess set was processed.
	reopened.Close()
	reopened.settings.Effective.Keep = 1
	if err := atomicJSON(reopened.root, "settings.json", reopened.settings); err != nil {
		t.Fatal(err)
	}
	var retired *entry
	for _, e := range reopened.entries {
		retired = e
		break
	}
	if err := reopened.persistRetirement(retired); err != nil {
		t.Fatal(err)
	}
	recovered, err := OpenNative(dir, key, 0, 3, nil, testCapture(&calls))
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	eventually(t, func() bool { return recovered.SchedulerObservation().Retention.State == "converged" })
	if got := recovered.SchedulerObservation(); got.Settings.Keep != 1 || got.Retention.Retained != 1 {
		t.Fatal(got)
	}
	if _, err := os.Stat(filepath.Join(recovered.root, retired.Set)); !os.IsNotExist(err) {
		t.Fatal("retirement intent did not remove files", err)
	}
}

func TestRestartAcknowledgesDeletionCompletedBeforeOutcomeWrite(t *testing.T) {
	var calls atomic.Int32
	m := nativeFixture(t, 0, 2, testCapture(&calls))
	first := publishSet(t, m, "backup")
	publishSet(t, m, "backup")
	// Crash fixture: retirement intent committed, bytes removed, clearing that
	// intent never reached the journal.
	m.Close()
	e := m.entries[first.ID]
	if err := m.persistRetirement(e); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(m.root, e.Set)); err != nil {
		t.Fatal(err)
	}
	recovered, err := OpenNative(filepath.Dir(m.root), m.key, 0, 2, nil, testCapture(&calls))
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	eventually(t, func() bool {
		got := recovered.SchedulerObservation()
		return got.Retention.State == "converged" && got.Retention.Retained == 1
	})
	publishSet(t, recovered, "backup")
}
