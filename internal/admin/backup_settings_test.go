package admin

import (
	"fmt"
	"github.com/ekarpov/tessera/internal/acquisition"
	"github.com/ekarpov/tessera/internal/identity"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
	"github.com/ekarpov/tessera/pkg/store/memory"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackupSettingsRejectsMissingUnknownDuplicateNullAndWrongTypes(t *testing.T) {
	revision := strings.Repeat("a", 32) + "." + strings.Repeat("b", 32) + "." + strings.Repeat("c", 32)
	valid := fmt.Sprintf(`{"expected_revision":%q,"every_hours":0,"keep":1}`, revision)
	if _, err := decodeBackupSettings([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{}`, `[]`, `null`, valid + `{}`, strings.Replace(valid, `,"keep":1`, "", 1),
		strings.Replace(valid, `"keep":1`, `"keep":null`, 1),
		strings.Replace(valid, `"keep":1`, `"keep":1,"keep":2`, 1),
		strings.Replace(valid, `"keep":1`, `"keep":1,"path":"synthetic"`, 1),
		strings.Replace(valid, `"keep":1`, `"keep":"1"`, 1),
		strings.Replace(valid, `"every_hours":0`, `"every_hours":1.5`, 1),
		strings.Replace(valid, `"expected_revision":`, `"expected_revision":null,"unused":`, 1),
	} {
		if _, err := decodeBackupSettings([]byte(body)); err == nil {
			t.Fatalf("accepted malformed payload %s", body)
		}
	}
}

func TestUnconfiguredBackupSettingsDoNotProvisionStorage(t *testing.T) {
	engine, err := memory.New(p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	dataset, err := engine.SoleDataset(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	authority, err := identity.Open(t.Context(), filepath.Join(t.TempDir(), "identity.json"), "https://tessera.example", engine)
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.AddServerAdministrator("owner", "synthetic-password", dataset.ID, store.Grants{Read: true, Write: true, Schema: true, Erase: true}, false); err != nil {
		t.Fatal(err)
	}
	h := &Handler{authority: authority, backupInstance: acquisition.NewInstanceID()}
	request := httptest.NewRequest("GET", "/admin/api/v1/backup-scheduler", nil)
	status, value, err := h.apiBackupScheduler(httptest.NewRecorder(), request, "owner", nil)
	observed := value.(acquisition.SchedulerObservation)
	if err != nil || status != 200 || observed.Configured || observed.Reason != "not_configured" || observed.Settings != nil || observed.InstanceID == "" {
		t.Fatal(status, value, err)
	}
	for _, method := range []string{"GET", "PUT"} {
		request = httptest.NewRequest(method, "/", nil)
		_, _, err = h.apiBackupSettings(httptest.NewRecorder(), request, "owner", nil)
		if p.ErrorCode(err) != "not_supported" {
			t.Fatal(method, err)
		}
	}
}
