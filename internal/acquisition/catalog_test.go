package acquisition

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	p "github.com/ekarpov/tessera/pkg/protocol"
)

func TestCatalogCompletionPaginationRotationAndRestart(t *testing.T) {
	dir := t.TempDir()
	key := bytes.Repeat([]byte{2}, 32)
	var calls atomic.Int32
	m, err := openManager(t, dir, key, 3, testCapture(&calls))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { m.Close() }()
	// Ordered caller IDs make page membership deterministic without time sleeps.
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	ids := []string{stamp + "-" + strings.Repeat("1", 32), stamp + "-" + strings.Repeat("2", 32), stamp + "-" + strings.Repeat("3", 32)}
	for i, id := range ids {
		kind := "backup"
		if i == 1 {
			kind = "evidence"
		}
		if _, err = m.Start(id, kind); err != nil {
			t.Fatal(err)
		}
		if waitTerminal(t, m, id).State != "complete" {
			t.Fatal("capture failed")
		}
	}
	first, err := m.Catalog(1, "")
	if err != nil || len(first.Results) != 1 || first.Results[0].ID != ids[0] || first.Next == "" {
		t.Fatal("first page", err)
	}
	// Restart preserves both IDs and signed cursor validity.
	m.Close()
	m, err = openManager(t, dir, key, 3, testCapture(&calls))
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Catalog(0, first.Next)
	if err != nil || len(second.Results) != 1 || second.Results[0].ID != ids[1] || second.Results[0].Kind != "evidence" {
		t.Fatal("restart continuation", err)
	}
	if _, err = m.Catalog(2, first.Next); p.ErrorCode(err) != "invalid_cursor" {
		t.Fatal("cursor limit changed", err)
	}
	if _, err = m.Catalog(0, first.Next+"x"); p.ErrorCode(err) != "invalid_cursor" {
		t.Fatal("tampered cursor", err)
	}
	if _, err = m.Catalog(101, ""); p.ErrorCode(err) != "invalid_field" {
		t.Fatal("unbounded catalog", err)
	}
	// Publication after the fixed upper ID bound waits for a rescan. Rotation
	// removes the first result; its ID cannot resolve to the replacement artifact.
	next := stamp + "-" + strings.Repeat("4", 32)
	if _, err = m.Start(next, "backup"); err != nil {
		t.Fatal(err)
	}
	waitTerminal(t, m, next)
	if _, err = m.Lease(ids[0], testLease, false); p.ErrorCode(err) != "expired" {
		t.Fatal("rotated result substituted", err)
	}
	if _, err = m.CompletedStatus(ids[0]); p.ErrorCode(err) != "expired" {
		t.Fatal("rotated metadata", err)
	}
	third, err := m.Catalog(0, second.Next)
	if err != nil || len(third.Results) != 1 || third.Results[0].ID != ids[2] || third.Next != "" {
		t.Fatal("concurrent publication entered scan", err)
	}
	all, err := m.Catalog(100, "")
	if err != nil || len(all.Results) != 3 || all.Results[2].ID != next {
		t.Fatal("rescan", err)
	}
	// Rotate an unseen entry; continuation skips it, preserving the other IDs.
	fifth := stamp + "-" + strings.Repeat("5", 32)
	if _, err = m.Start(fifth, "evidence"); err != nil {
		t.Fatal(err)
	}
	waitTerminal(t, m, fifth)
	page, err := m.Catalog(0, first.Next)
	if err != nil || len(page.Results) != 1 || page.Results[0].ID != ids[2] {
		t.Fatal("unseen rotation", err)
	}
	m.mu.Lock()
	m.now = func() time.Time { return time.Now().Add(CatalogLifetime + time.Second) }
	m.mu.Unlock()
	if _, err = m.Catalog(0, first.Next); p.ErrorCode(err) != "cursor_expired" {
		t.Fatal("expired cursor", err)
	}
}

func TestCatalogHidesStagingAndDoesNotLease(t *testing.T) {
	var calls atomic.Int32
	entered, resume := make(chan struct{}), make(chan struct{})
	m, err := openManager(t, t.TempDir(), bytes.Repeat([]byte{3}, 32), 1, testCapture(&calls))
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
	page, err := m.Catalog(10, "")
	if err != nil || len(page.Results) != 0 {
		t.Fatal("staging visible", err)
	}
	if _, err = m.CompletedStatus(id); p.ErrorCode(err) != "unavailable" {
		t.Fatal("partial metadata visible", err)
	}
	close(resume)
	op := waitTerminal(t, m, id)
	m.mu.Lock()
	m.beforePublish = nil
	m.mu.Unlock()
	if _, _, err = m.Read(id, op.Artifacts[0].ID, testLease); p.ErrorCode(err) != "expired" {
		t.Fatal("listing implicitly pinned", err)
	}
	next := NewID()
	if _, err = m.Start(next, "backup"); err != nil {
		t.Fatal(err)
	}
	waitTerminal(t, m, next)
	if _, err = m.CompletedStatus(id); p.ErrorCode(err) != "expired" {
		t.Fatal(err)
	}
}

func TestCollectorDigestRestartExpiryRevocationAndProducerIndependence(t *testing.T) {
	dir := t.TempDir()
	key := bytes.Repeat([]byte{4}, 32)
	var calls atomic.Int32
	m, err := openManager(t, dir, key, 2, testCapture(&calls))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { m.Close() }()
	id := strings.Repeat("a", 32)
	credential, err := m.ProvisionCollector(id, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.AuthenticateCollector(credential.Token); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(m.root, "collectors.json"))
	if err != nil || bytes.Contains(raw, []byte(credential.Token)) || bytes.Contains(raw, []byte(strings.Split(credential.Token, ".")[2])) {
		t.Fatal("secret persisted", err)
	}
	if _, err = m.ProvisionCollector(id, time.Hour); p.ErrorCode(err) != "conflict" {
		t.Fatal("uncertain issuance replaced", err)
	}
	if _, err = m.ProvisionCollector(strings.Repeat("b", 32), MaxCollectorLifetime+time.Second); p.ErrorCode(err) != "invalid_field" {
		t.Fatal("unbounded expiry", err)
	}
	m.Close()
	m, err = openManager(t, dir, key, 2, testCapture(&calls))
	if err != nil {
		t.Fatal(err)
	}
	if err = m.AuthenticateCollector(credential.Token); err != nil {
		t.Fatal("restart lost credential", err)
	}
	if err = m.AuthenticateCollector(credential.Token + "x"); p.ErrorCode(err) != "authentication_failed" {
		t.Fatal("bad token", err)
	}
	m.mu.Lock()
	m.now = func() time.Time { return credential.ExpiresAt }
	m.mu.Unlock()
	if err = m.AuthenticateCollector(credential.Token); p.ErrorCode(err) != "authentication_failed" {
		t.Fatal("expiry", err)
	}
	m.mu.Lock()
	m.now = time.Now
	m.mu.Unlock()
	if err = m.RevokeCollector(id); err != nil {
		t.Fatal(err)
	}
	if err = m.AuthenticateCollector(credential.Token); p.ErrorCode(err) != "authentication_failed" {
		t.Fatal("revocation", err)
	}
	m.Close()
	m, err = openManager(t, dir, key, 2, testCapture(&calls))
	if err != nil {
		t.Fatal(err)
	}
	if err = m.AuthenticateCollector(credential.Token); p.ErrorCode(err) != "authentication_failed" {
		t.Fatal("revocation not durable", err)
	}
	op := NewID()
	if _, err = m.Start(op, "backup"); err != nil {
		t.Fatal("revocation stopped producer", err)
	}
	if waitTerminal(t, m, op).State != "complete" {
		t.Fatal("producer failed")
	}
}

func TestCollectorRevocationPersistenceFailureFailsOnlyCollectors(t *testing.T) {
	var calls atomic.Int32
	m, err := openManager(t, t.TempDir(), bytes.Repeat([]byte{5}, 32), 1, testCapture(&calls))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	credential, err := m.ProvisionCollector(strings.Repeat("a", 32), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(m.root, "collectors.json")
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err = m.RevokeCollector(credential.ID); err == nil {
		t.Fatal("failed revocation reported success")
	}
	if err = m.AuthenticateCollector(credential.Token); err == nil {
		t.Fatal("credential accepted after uncertain revocation")
	}
	id := NewID()
	if _, err = m.Start(id, "evidence"); err != nil {
		t.Fatal(err)
	}
	if waitTerminal(t, m, id).State != "complete" {
		t.Fatal("credential failure stopped producer")
	}
}

func TestCollectorBound(t *testing.T) {
	m, err := openManager(t, t.TempDir(), bytes.Repeat([]byte{6}, 32), 1, func(context.Context, string, string) (string, string, []byte, error) { return "", "R", nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	for i := 0; i < maxCollectors; i++ {
		if _, err = m.ProvisionCollector(token(), time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = m.ProvisionCollector(token(), time.Hour); p.ErrorCode(err) != "limit_exceeded" {
		t.Fatal("credential bound", err)
	}
}
