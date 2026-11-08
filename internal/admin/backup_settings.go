package admin

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/ekarpov/tessera/internal/acquisition"
	"github.com/ekarpov/tessera/pkg/store"
)

func (h *Handler) backupSettingsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/v1/backup-scheduler", h.api(h.apiBackupScheduler))
	mux.HandleFunc("PUT /admin/api/v1/backup-settings-operations/{request_id}", h.api(h.apiBackupSettings))
	mux.HandleFunc("GET /admin/api/v1/backup-settings-operations/{request_id}", h.api(h.apiBackupSettings))
}
func (h *Handler) apiBackupScheduler(w http.ResponseWriter, r *http.Request, administrator string, body []byte) (int, any, error) {
	if !h.authority.AdministersServer(administrator) {
		return 0, nil, store.Refuse("permission_denied")
	}
	if len(body) != 0 {
		return 0, nil, store.Refuse("invalid_request")
	}
	if h.acquisitions == nil {
		return 200, acquisition.UnconfiguredScheduler(h.backupInstance, time.Now()), nil
	}
	return 200, h.acquisitions.SchedulerObservation(), nil
}
func (h *Handler) apiBackupSettings(w http.ResponseWriter, r *http.Request, administrator string, body []byte) (int, any, error) {
	if !h.authority.AdministersServer(administrator) {
		return 0, nil, store.Refuse("permission_denied")
	}
	if h.acquisitions == nil {
		return 0, nil, store.RefuseWith("not_supported", "not_configured", nil)
	}
	id := r.PathValue("request_id")
	if r.Method == "GET" {
		if len(body) != 0 {
			return 0, nil, store.Refuse("invalid_request")
		}
		op, err := h.acquisitions.SettingsStatus(id)
		return 200, op, err
	}
	payload, err := decodeBackupSettings(body)
	if err != nil {
		return 0, nil, err
	}
	op, err := h.acquisitions.SubmitSettings(id, payload)
	status := 202
	if op.State == "effective" {
		status = 200
	}
	return status, op, err
}
func decodeBackupSettings(body []byte) (acquisition.SettingsPayload, error) {
	var out acquisition.SettingsPayload
	d := json.NewDecoder(bytes.NewReader(body))
	first, err := d.Token()
	if err != nil || first != json.Delim('{') {
		return out, store.Refuse("invalid_request")
	}
	seen := map[string]bool{}
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return out, store.Refuse("invalid_request")
		}
		name, ok := token.(string)
		if !ok || seen[name] || (name != "expected_revision" && name != "every_hours" && name != "keep") {
			return out, store.Refuse("invalid_request")
		}
		seen[name] = true
		var raw json.RawMessage
		if d.Decode(&raw) != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return out, store.Refuse("invalid_request")
		}
		switch name {
		case "expected_revision":
			err = json.Unmarshal(raw, &out.ExpectedRevision)
		case "every_hours":
			err = json.Unmarshal(raw, &out.EveryHours)
		case "keep":
			err = json.Unmarshal(raw, &out.Keep)
		}
		if err != nil {
			return out, store.Refuse("invalid_field")
		}
	}
	if _, err = d.Token(); err != nil || len(seen) != 3 {
		return out, store.Refuse("invalid_request")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return out, store.Refuse("invalid_request")
	}
	return out, nil
}
