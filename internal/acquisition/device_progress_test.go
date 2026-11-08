package acquisition

import (
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
	"github.com/ekarpov/tessera/pkg/store/sqlite"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDeviceReadsAndUploadsFinishWhileSettingsApplicationIsStalled(t *testing.T) {
	engine, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "data.db"), p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	if err = engine.EnableContent(t.Context(), filepath.Join(t.TempDir(), "content")); err != nil {
		t.Fatal(err)
	}
	datasetInfo, err := engine.SoleDataset(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	dataset := datasetInfo.ID
	device := store.Access{Principal: "synthetic-device", Authorization: "synthetic-device"}
	if err = engine.SetAuthorization(t.Context(), dataset, device, store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true}, View: store.FullView()}); err != nil {
		t.Fatal(err)
	}
	info, err := engine.Info(t.Context(), device, dataset)
	if err != nil {
		t.Fatal(err)
	}
	content := info.ContentStoreID
	begin := func(id string) map[string]any {
		v, e := engine.ContentControl(t.Context(), device, "begin", content, "", []byte(`{"operation_id":"`+id+`","content_type":"application/octet-stream","maximum_bytes":1024}`))
		if e != nil {
			t.Fatal(e)
		}
		return v.(map[string]any)
	}
	first := begin("before-backup")
	if _, err = engine.ContentTransfer(t.Context(), strings.TrimPrefix(first["url"].(string), "/v1/transfers/"), "PATCH", "bytes 0-2/3", strings.NewReader("abc")); err != nil {
		t.Fatal(err)
	}
	v, err := engine.ContentControl(t.Context(), device, "commit", content, first["upload_id"].(string), nil)
	if err != nil {
		t.Fatal(err)
	}
	item := v.(map[string]any)["item_id"].(string)

	var calls atomic.Int32
	m := nativeFixture(t, 0, 2, testCapture(&calls))
	entered, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	m.mu.Lock()
	m.beforeSettingsEffective = func() { close(entered); <-resume }
	m.mu.Unlock()
	t.Cleanup(func() { once.Do(func() { close(resume) }) })
	id, payload := settingsRequest(m, 12, 1)
	eventually(t, func() bool { _, err := m.SubmitSettings(id, payload); return err == nil })
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("application barrier")
	}
	if op, err := m.SubmitSettings(id, payload); err != nil || op.State != "pending" {
		t.Fatal("identical retry blocked behind application", op, err)
	}
	second := begin("during-settings-application")
	progress := make(chan error, 1)
	go func() {
		resolved, e := engine.ContentControl(t.Context(), device, "resolve", content, item, []byte(`{"representation":"original"}`))
		if e != nil {
			progress <- e
			return
		}
		read, e := engine.ContentTransfer(t.Context(), strings.TrimPrefix(resolved.(map[string]any)["url"].(string), "/v1/transfers/"), "GET", "", nil)
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
		_, e = engine.ContentTransfer(t.Context(), strings.TrimPrefix(second["url"].(string), "/v1/transfers/"), "PATCH", "bytes 0-2/3", strings.NewReader("new"))
		progress <- e
	}()
	select {
	case e := <-progress:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("settings application blocked device traffic")
	}
	// Both transfer operations completed before the application boundary resumed.
	select {
	case <-resume:
		t.Fatal("barrier unexpectedly resumed")
	default:
	}
	once.Do(func() { close(resume) })
	waitEffective(t, m, id)
}
