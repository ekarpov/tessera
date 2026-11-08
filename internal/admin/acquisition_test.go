package admin

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ekarpov/tessera/internal/acquisition"
	"github.com/ekarpov/tessera/internal/identity"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
	"github.com/ekarpov/tessera/pkg/store/memory"
)

func TestAcquisitionRequiresServerAdministratorOnEveryAction(t *testing.T) {
	engine, err := memory.New(p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	d, err := engine.SoleDataset(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	authority, err := identity.Open(t.Context(), filepath.Join(t.TempDir(), "identity.json"), "https://tessera.example", engine)
	if err != nil {
		t.Fatal(err)
	}
	if err = authority.AddPerson("dataset-admin", "synthetic-password-only", d.ID, store.Grants{Read: true, Write: true, Schema: true, Erase: true}, false); err != nil {
		t.Fatal(err)
	}
	backupDir := t.TempDir()
	if err = os.Chmod(backupDir, 0700); err != nil {
		t.Fatal(err)
	}
	m, err := acquisition.Open(backupDir, bytes.Repeat([]byte{1}, 32), 1, func(context.Context, string, string) (string, string, []byte, error) { return "", "R", nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	h := &Handler{authority: authority, acquisitions: m}
	for _, handle := range []apiHandler{h.apiAcquisition, h.apiAcquisitionLease, h.apiAcquisitionArtifact, h.apiAcquisitionCatalog, h.apiCollectors, h.apiBackupScheduler, h.apiBackupSettings} {
		for _, method := range []string{"PUT", "GET", "DELETE"} {
			request := httptest.NewRequest(method, "/", nil)
			_, _, err := handle(httptest.NewRecorder(), request, "dataset-admin", nil)
			if p.ErrorCode(err) != "permission_denied" {
				t.Fatal("dataset admin acquired server artifacts", method, err)
			}
		}
	}
	mux := http.NewServeMux()
	h.acquisitionRoutes(mux)
	for _, path := range []string{"/admin/api/v1/backup-operations/unknown", "/admin/api/v1/backup-operations/unknown/leases/00000000000000000000000000000001", "/admin/api/v1/backup-operations/unknown/artifacts/unknown"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != http.StatusUnauthorized && w.Code != http.StatusMethodNotAllowed {
			t.Fatal("unauthenticated acquisition", w.Code)
		}
	}
}

// Revocation gates new requests, but cannot retract an already admitted stream.
// That stream retains the existing two-minute write bound and exact open file.
func TestCollectorRevocationDuringAdmittedArtifactTransfer(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	payload := []byte("synthetic encrypted archive bytes")
	m, err := acquisition.Open(dir, bytes.Repeat([]byte{7}, 32), 1, func(ctx context.Context, kind, path string) (string, string, []byte, error) {
		return "S", "R", nil, os.WriteFile(path, payload, 0600)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	credential, err := m.ProvisionCollector(strings.Repeat("a", 32), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	id := acquisition.NewID()
	if _, err = m.Start(id, "backup"); err != nil {
		t.Fatal(err)
	}
	var op acquisition.Operation
	deadline := time.Now().Add(5 * time.Second)
	for {
		op, err = m.Status(id)
		if err != nil {
			t.Fatal(err)
		}
		if op.State == "complete" {
			break
		}
		if op.State != "running" || time.Now().After(deadline) {
			t.Fatal("publication")
		}
		time.Sleep(time.Millisecond)
	}
	lease := strings.Repeat("b", 32)
	if _, err = m.Lease(id, lease, false); err != nil {
		t.Fatal(err)
	}
	h := &Handler{acquisitions: m}
	mux := http.NewServeMux()
	h.acquisitionRoutes(mux)
	request := func(path string) *http.Request {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer "+credential.Token)
		r.Header.Set("Tessera-Backup-Lease", lease)
		return r
	}
	wrong := httptest.NewRecorder()
	mux.ServeHTTP(wrong, request("/admin/api/v1/backup-operations/"+id+"/artifacts/"+strings.Repeat("c", 32)))
	if wrong.Code != 404 {
		t.Fatal("wrong artifact did not fail", wrong.Code)
	}
	writer := &pausedArtifactWriter{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), resume: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		mux.ServeHTTP(writer, request("/admin/api/v1/backup-operations/"+id+"/artifacts/"+op.Artifacts[0].ID))
		close(done)
	}()
	<-writer.entered
	if err = m.RevokeCollector(credential.ID); err != nil {
		t.Fatal(err)
	}
	denied := httptest.NewRecorder()
	mux.ServeHTTP(denied, request("/admin/api/v1/backup-operations"))
	if denied.Code != 401 {
		t.Fatal("revoked collector read catalog", denied.Code)
	}
	close(writer.resume)
	<-done
	if writer.Code != 200 || !bytes.Equal(writer.Body.Bytes(), payload) {
		t.Fatal("admitted artifact changed")
	}
}

type pausedArtifactWriter struct {
	*httptest.ResponseRecorder
	entered, resume chan struct{}
	once            sync.Once
}

func (w *pausedArtifactWriter) Write(b []byte) (int, error) {
	w.once.Do(func() { close(w.entered); <-w.resume })
	return w.ResponseRecorder.Write(b)
}
