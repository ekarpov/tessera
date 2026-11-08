package standalone

import (
	"bytes"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ekarpov/tessera/pkg/store"
)

func TestDeviceTransfersProgressDuringCollectorRetrieval(t *testing.T) {
	dir := t.TempDir()
	backups := filepath.Join(dir, "backups")
	if err := os.Mkdir(backups, 0700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, bytes.Repeat([]byte{3}, 32), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := Open(t.Context(), Config{Issuer: "https://tessera.example", DataDir: filepath.Join(dir, "data"), Engine: "sqlite", Content: true, EncryptedStorage: true, Backup: &BackupConfig{Directory: backups, KeyFile: key, Keep: 2}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	dataset := sole(t, r.Engine)
	device := store.Access{Principal: "synthetic-device", Authorization: "synthetic-device"}
	if err = r.Engine.SetAuthorization(t.Context(), dataset, device, store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true}, View: store.FullView()}); err != nil {
		t.Fatal(err)
	}
	info, err := r.Engine.Info(t.Context(), device, dataset)
	if err != nil {
		t.Fatal(err)
	}
	content := info.ContentStoreID
	begin := func(id string) map[string]any {
		v, e := r.Engine.ContentControl(t.Context(), device, "begin", content, "", []byte(`{"operation_id":"`+id+`","content_type":"application/octet-stream","maximum_bytes":1024}`))
		if e != nil {
			t.Fatal(e)
		}
		return v.(map[string]any)
	}
	first := begin("before-backup")
	if _, err = r.Engine.ContentTransfer(t.Context(), strings.TrimPrefix(first["url"].(string), "/v1/transfers/"), "PATCH", "bytes 0-2/3", strings.NewReader("abc")); err != nil {
		t.Fatal(err)
	}
	v, err := r.Engine.ContentControl(t.Context(), device, "commit", content, first["upload_id"].(string), nil)
	if err != nil {
		t.Fatal(err)
	}
	item := v.(map[string]any)["item_id"].(string)
	if _, err = r.BackupNow(t.Context()); err != nil {
		t.Fatal(err)
	}
	page, err := r.Acquisitions.Catalog(1, "")
	if err != nil || len(page.Results) != 1 {
		t.Fatal("catalog", err)
	}
	op := page.Results[0]
	credential, err := r.Acquisitions.ProvisionCollector(strings.Repeat("a", 32), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	lease := strings.Repeat("b", 32)
	if _, err = r.Acquisitions.Lease(op.ID, lease, false); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "https://tessera.example/admin/api/v1/backup-operations/"+op.ID+"/artifacts/"+op.Artifacts[0].ID, nil)
	request.Header.Set("Authorization", "Bearer "+credential.Token)
	request.Header.Set("Tessera-Backup-Lease", lease)
	writer := &collectorPausedWriter{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), resume: make(chan struct{})}
	finished := make(chan struct{})
	go func() { r.Handler.ServeHTTP(writer, request); close(finished) }()
	select {
	case <-writer.entered:
	case <-finished:
		t.Fatal("artifact did not start", writer.Code)
	case <-time.After(5 * time.Second):
		t.Fatal("artifact did not start")
	}
	defer func() { close(writer.resume); <-finished }()
	second := begin("during-retrieval")
	progress := make(chan error, 1)
	go func() {
		resolved, e := r.Engine.ContentControl(t.Context(), device, "resolve", content, item, []byte(`{"representation":"original"}`))
		if e != nil {
			progress <- e
			return
		}
		read, e := r.Engine.ContentTransfer(t.Context(), strings.TrimPrefix(resolved.(map[string]any)["url"].(string), "/v1/transfers/"), "GET", "", nil)
		if e != nil {
			progress <- e
			return
		}
		raw, e := io.ReadAll(read.Body)
		read.Body.Close()
		if e != nil || string(raw) != "abc" {
			progress <- store.Refuse("internal_error")
			return
		}
		_, e = r.Engine.ContentTransfer(t.Context(), strings.TrimPrefix(second["url"].(string), "/v1/transfers/"), "PATCH", "bytes 0-2/3", strings.NewReader("new"))
		progress <- e
	}()
	select {
	case e := <-progress:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("collector retrieval blocked device traffic")
	}
}

type collectorPausedWriter struct {
	*httptest.ResponseRecorder
	entered, resume chan struct{}
	once            sync.Once
}

func (w *collectorPausedWriter) Write(b []byte) (int, error) {
	w.once.Do(func() { close(w.entered); <-w.resume })
	return w.ResponseRecorder.Write(b)
}
