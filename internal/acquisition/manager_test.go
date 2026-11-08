package acquisition

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	p "github.com/ekarpov/tessera/pkg/protocol"
)

const testLease = "00000000000000000000000000000001"

func waitTerminal(t *testing.T, m *Manager, id string) Operation {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		op, err := m.Status(id)
		if err != nil {
			t.Fatal(err)
		}
		m.mu.Lock()
		settling := m.busy || m.maintenanceActive
		if op.State == "complete" && m.retentionFailure == "" {
			for _, e := range m.entries {
				if e.DeletePending {
					settling = true
				}
			}
		}
		m.mu.Unlock()
		if op.State != "running" && !settling {
			return op
		}
		select {
		case <-deadline:
			t.Fatal("worker did not finish")
		case <-time.After(time.Millisecond):
		}
	}
}
func testCapture(calls *atomic.Int32) Capture {
	return func(ctx context.Context, kind, path string) (string, string, []byte, error) {
		calls.Add(1)
		if kind == "backup" {
			if err := writeFile(path, []byte("encrypted fixture")); err != nil {
				return "", "", nil, err
			}
		}
		return "S", "R", nil, ctx.Err()
	}
}
func TestReconcileAuthenticateLeaseAndRotation(t *testing.T) {
	dir := t.TempDir()
	key := bytes.Repeat([]byte{7}, 32)
	var calls atomic.Int32
	entered, resume := make(chan struct{}), make(chan struct{})
	m, err := openManager(t, dir, key, 1, func(ctx context.Context, kind, path string) (string, string, []byte, error) {
		close(entered)
		<-resume
		return testCapture(&calls)(ctx, kind, path)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	id := NewID()
	first, err := m.Start(id, "backup")
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	// Simulate lost acceptance: no new job, despite an identical retry.
	replay, err := m.Start(id, "backup")
	if err != nil || replay.ID != first.ID {
		t.Fatal(replay, err)
	}
	if _, err = m.Start(id, "evidence"); p.ErrorCode(err) != "conflict" {
		t.Fatal(err)
	}
	if _, err = m.Start(NewID(), "backup"); p.ErrorCode(err) != "limit_exceeded" {
		t.Fatal(err)
	}
	close(resume)
	op := waitTerminal(t, m, id)
	if op.State != "complete" || calls.Load() != 1 {
		t.Fatal(op, calls.Load())
	}
	replay, err = m.Start(id, "backup")
	if err != nil || replay.State != "complete" || calls.Load() != 1 {
		t.Fatal("lost completion duplicated work", err)
	}
	if _, _, err = m.Read(id, op.Artifacts[0].ID, testLease); p.ErrorCode(err) != "expired" {
		t.Fatal("read without lease", err)
	}
	expiry, err := m.Lease(id, testLease, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Start(NewID(), "evidence"); p.ErrorCode(err) != "limit_exceeded" {
		t.Fatal("rotated protected pair", err)
	}
	var read *Read
	for _, a := range op.Artifacts {
		f, _, err := m.Read(id, a.ID, testLease)
		if err != nil {
			t.Fatal(err)
		}
		if a.Kind == "archive" {
			read = f
			continue
		}
		raw, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		if a.Kind == "metadata" {
			pair, err := Verify(raw, key, op.Pair.Deployment)
			if err != nil || pair.R != "R" {
				t.Fatal(err)
			}
			if _, err = Verify(raw, key, "another deployment"); err == nil {
				t.Fatal("wrong provenance accepted")
			}
			var env Envelope
			_ = json.Unmarshal(raw, &env)
			env.Payload[0] ^= 1
			bad, _ := json.Marshal(env)
			if _, err = Verify(bad, key, op.Pair.Deployment); err == nil {
				t.Fatal("tampered metadata accepted")
			}
		}
	}
	// Expired and released leases still cannot evict an admitted reader.
	m.mu.Lock()
	m.now = func() time.Time { return expiry.Add(time.Second) }
	m.capture = testCapture(&calls)
	m.mu.Unlock()
	if _, err = m.Lease(id, testLease, true); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Start(strconv.FormatInt(expiry.Add(time.Second).Unix(), 10)+"-"+token(), "backup"); p.ErrorCode(err) != "limit_exceeded" {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(read)
	if err != nil || string(raw) != "encrypted fixture" {
		t.Fatal(err)
	}
	read.Close()
	// Reset clock to issue a new ID in its admission window.
	m.mu.Lock()
	m.now = time.Now
	m.mu.Unlock()
	next := NewID()
	if _, err = m.Start(next, "backup"); err != nil {
		t.Fatal(err)
	}
	if got := waitTerminal(t, m, next); got.State != "complete" {
		t.Fatal(got)
	}
	old, err := m.Status(id)
	if err != nil || old.State != "expired" {
		t.Fatal(old, err)
	}
	if _, err = m.Lease(id, testLease, false); p.ErrorCode(err) != "expired" {
		t.Fatal(err)
	}
}
func TestRestartInterruptAndRetention(t *testing.T) {
	dir := t.TempDir()
	key := bytes.Repeat([]byte{3}, 32)
	var calls atomic.Int32
	m, err := openManager(t, dir, key, 1, testCapture(&calls))
	if err != nil {
		t.Fatal(err)
	}
	id := NewID()
	if _, err = m.Start(id, "evidence"); err != nil {
		t.Fatal(err)
	}
	op := waitTerminal(t, m, id)
	m.Close()
	m, err = openManager(t, dir, key, 1, testCapture(&calls))
	if err != nil {
		t.Fatal(err)
	}
	replay, err := m.Start(id, "evidence")
	if err != nil || replay.State != "complete" || calls.Load() != 1 {
		t.Fatal("completed operation lost across restart", err)
	}
	m.Close()
	// Reconstruct an interrupted publication whose directory was already renamed.
	journal := filepath.Join(dir, ".acquisitions", "operations.json")
	var entries map[string]*entry
	if err = readJSON(journal, &entries); err != nil {
		t.Fatal(err)
	}
	entries[id].State = "running"
	if err = atomicJSON(filepath.Dir(journal), "operations.json", entries); err != nil {
		t.Fatal(err)
	}
	m, err = openManager(t, dir, key, 1, testCapture(&calls))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	got, err := m.Start(id, "evidence")
	if err != nil || got.State != "failed" || calls.Load() != 1 {
		t.Fatal("interrupted operation restarted", got, err)
	}
	m.mu.Lock()
	m.now = func() time.Time { return op.ExpiresAt.Add(time.Second) }
	m.mu.Unlock()
	if _, err = m.Start(id, "evidence"); p.ErrorCode(err) != "expired" {
		t.Fatal("expired ID admitted", err)
	}
}
func TestCancellationAndPublicationAreNeverPartial(t *testing.T) {
	dir := t.TempDir()
	key := bytes.Repeat([]byte{1}, 32)
	var calls atomic.Int32
	entered, resume := make(chan struct{}), make(chan struct{})
	m, err := openManager(t, dir, key, 2, testCapture(&calls))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.beforePublish = func() { close(entered); <-resume }
	id := NewID()
	if _, err = m.Start(id, "backup"); err != nil {
		t.Fatal(err)
	}
	<-entered
	if _, err = m.Lease(id, testLease, false); err == nil {
		t.Fatal("leased partial output")
	}
	if _, err = m.Cancel(id); err != nil {
		t.Fatal(err)
	}
	close(resume)
	if op := waitTerminal(t, m, id); op.State != "cancelled" || len(op.Artifacts) != 0 {
		t.Fatal(op)
	}
	contents, err := os.ReadDir(m.root)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range contents {
		if f.IsDir() {
			t.Fatal("partial directory survived", f.Name())
		}
	}
}

func TestRenewalAndStreamAndStatusBounds(t *testing.T) {
	var calls atomic.Int32
	m, err := openManager(t, t.TempDir(), bytes.Repeat([]byte{2}, 32), 1, testCapture(&calls))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	id := NewID()
	if _, err = m.Start(id, "evidence"); err != nil {
		t.Fatal(err)
	}
	op := waitTerminal(t, m, id)
	first, err := m.Lease(id, testLease, false)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(time.Minute)
	m.mu.Lock()
	m.now = func() time.Time { return now }
	m.mu.Unlock()
	renewed, err := m.Lease(id, testLease, false)
	if err != nil || !renewed.After(first) {
		t.Fatal("lease not renewed", err)
	}
	var readers []*Read
	for i := 0; i < 8; i++ {
		r, _, err := m.Read(id, op.Artifacts[0].ID, testLease)
		if err != nil {
			t.Fatal(err)
		}
		readers = append(readers, r)
	}
	if _, _, err = m.Read(id, op.Artifacts[0].ID, testLease); p.ErrorCode(err) != "limit_exceeded" {
		t.Fatal("stream bound not enforced", err)
	}
	for _, r := range readers {
		r.Close()
	}
	m.mu.Lock()
	m.now = time.Now
	for i := len(m.entries); i < maxStatuses; i++ {
		m.entries[NewID()] = &entry{Operation: Operation{State: "failed", ExpiresAt: time.Now().Add(Retention)}}
	}
	m.mu.Unlock()
	if _, err = m.Start(NewID(), "evidence"); p.ErrorCode(err) != "limit_exceeded" {
		t.Fatal("status bound not enforced", err)
	}
}

func TestIndependentLeasesSurviveAnotherRelease(t *testing.T) {
	var calls atomic.Int32
	m, err := openManager(t, t.TempDir(), bytes.Repeat([]byte{6}, 32), 1, testCapture(&calls))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	id := NewID()
	if _, err = m.Start(id, "evidence"); err != nil {
		t.Fatal(err)
	}
	op := waitTerminal(t, m, id)
	another := "00000000000000000000000000000002"
	if _, err = m.Lease(id, testLease, false); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Lease(id, another, false); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Lease(id, testLease, true); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Start(NewID(), "backup"); p.ErrorCode(err) != "limit_exceeded" {
		t.Fatal("released another consumer's protection", err)
	}
	r, _, err := m.Read(id, op.Artifacts[0].ID, another)
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
}

func openManager(t *testing.T, dir string, key []byte, keep int, capture Capture) (*Manager, error) {
	t.Helper()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return Open(dir, key, keep, capture)
}

func TestFailedExportKeepsPreviousPairAndPublishesNothing(t *testing.T) {
	var calls atomic.Int32
	m, err := openManager(t, t.TempDir(), bytes.Repeat([]byte{4}, 32), 1, testCapture(&calls))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	id := NewID()
	if _, err = m.Start(id, "backup"); err != nil {
		t.Fatal(err)
	}
	old := waitTerminal(t, m, id)
	m.mu.Lock()
	m.capture = func(ctx context.Context, kind, path string) (string, string, []byte, error) {
		_ = writeFile(path, []byte("partial encrypted archive"))
		return "", "", nil, context.DeadlineExceeded
	}
	m.mu.Unlock()
	failed := NewID()
	if _, err = m.Start(failed, "backup"); err != nil {
		t.Fatal(err)
	}
	op := waitTerminal(t, m, failed)
	if op.State != "failed" || len(op.Artifacts) != 0 || op.Pair != nil {
		t.Fatal("partial pair appeared published", op.State)
	}
	retained, err := m.Status(id)
	if err != nil || retained.State != "complete" {
		t.Fatal("failed export rotated previous pair", err)
	}
	if _, err = m.Lease(id, testLease, false); err != nil {
		t.Fatal(err)
	}
	f, _, err := m.Read(id, old.Artifacts[0].ID, testLease)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
}

func TestShutdownCancelsAndJoinsAcceptedWork(t *testing.T) {
	dir := t.TempDir()
	key := bytes.Repeat([]byte{8}, 32)
	entered := make(chan struct{})
	m, err := openManager(t, dir, key, 1, func(ctx context.Context, kind, path string) (string, string, []byte, error) {
		close(entered)
		<-ctx.Done()
		return "", "", nil, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	id := NewID()
	if _, err = m.Start(id, "evidence"); err != nil {
		t.Fatal(err)
	}
	<-entered
	done := make(chan struct{})
	go func() { m.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not join its worker")
	}
	var calls atomic.Int32
	m, err = openManager(t, dir, key, 1, testCapture(&calls))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	op, err := m.Status(id)
	if err != nil || op.State != "cancelled" || len(op.Artifacts) != 0 {
		t.Fatal("shutdown outcome was not durable", err)
	}
	if _, err = m.Start(id, "evidence"); err != nil || calls.Load() != 0 {
		t.Fatal("shutdown operation restarted", err)
	}
}

func TestRestartReconcilesExcessCompletedSets(t *testing.T) {
	for _, tc := range []struct {
		name          string
		sets          int
		expiredIntent bool
	}{
		{"interrupted rotation after publication", 2, false},
		{"lowered keep", 3, false},
		{"interrupted rotation after expiry intent", 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			key := bytes.Repeat([]byte{3}, 32)
			var calls atomic.Int32
			m, err := openManager(t, dir, key, tc.sets, testCapture(&calls))
			if err != nil {
				t.Fatal(err)
			}
			ids := make([]string, tc.sets)
			paths := make([]string, tc.sets)
			for i := range ids {
				ids[i] = NewID()
				if _, err = m.Start(ids[i], "backup"); err != nil {
					m.Close()
					t.Fatal(err)
				}
				if op := waitTerminal(t, m, ids[i]); op.State != "complete" {
					m.Close()
					t.Fatal(op.State)
				}
				paths[i], err = m.ArchivePath(ids[i])
				if err != nil {
					m.Close()
					t.Fatal(err)
				}
			}
			// Reconstruct the durable state between publishing the new set and
			// expiring/removing the victim. It is also a valid keep reduction input.
			m.mu.Lock()
			for i, id := range ids {
				m.entries[id].AcceptedAt = time.Now().Add(time.Duration(i-tc.sets) * time.Minute)
				if tc.expiredIntent && i == 0 {
					m.entries[id].State = "expired"
					m.entries[id].Pair = nil
					m.entries[id].Artifacts = nil
				}
			}
			err = m.save()
			m.mu.Unlock()
			// This fixture installs a durable reduced policy; static keep is bootstrap only.
			m.settingsMu.Lock()
			m.mu.Lock()
			policy := cloneSettings(m.settings)
			policy.Effective.Keep = 1
			m.mu.Unlock()
			if saveErr := m.writeSettings(policy); err == nil {
				err = saveErr
			}
			m.settingsMu.Unlock()
			m.Close()
			if err != nil {
				t.Fatal(err)
			}
			for attempt := 0; attempt < 2; attempt++ {
				m, err = openManager(t, dir, key, 1, testCapture(&calls))
				if err != nil {
					t.Fatal(err)
				}
				for i, id := range ids {
					op, e := m.Status(id)
					if e != nil {
						m.Close()
						t.Fatal(e)
					}
					if i < len(ids)-1 {
						if op.State != "expired" {
							m.Close()
							t.Fatal("old completed set was not rotated on restart", op.State)
						}
						if _, e = os.Stat(paths[i]); !os.IsNotExist(e) {
							m.Close()
							t.Fatal("old artifact survived rotation", e)
						}
					} else if op.State != "complete" {
						m.Close()
						t.Fatal("newest set was lost", op.State)
					}
				}
				if calls.Load() != int32(tc.sets) {
					m.Close()
					t.Fatal("recovery recaptured a backup")
				}
				if attempt == 0 {
					m.Close()
				}
			}
			defer m.Close()
			next := NewID()
			if _, err = m.Start(next, "backup"); err != nil {
				t.Fatal("recovered rotation still blocks admission", err)
			}
			if op := waitTerminal(t, m, next); op.State != "complete" {
				t.Fatal(op.State)
			}
		})
	}
}
