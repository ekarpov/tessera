package admin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ekarpov/tessera/internal/acquisition"
	"github.com/ekarpov/tessera/pkg/store"
)

func (h *Handler) SetAcquisitions(m *acquisition.Manager) { h.acquisitions = m }
func (h *Handler) acquisitionRoutes(mux *http.ServeMux) {
	h.backupSettingsRoutes(mux)
	mux.HandleFunc("GET /admin/api/v1/backup-operations", h.acquisitionRoute(h.apiAcquisitionCatalog, true))
	mux.HandleFunc("GET /admin/api/v1/backup-collectors", h.api(h.apiCollectors))
	for _, method := range []string{"PUT", "DELETE"} {
		mux.HandleFunc(method+" /admin/api/v1/backup-collectors/{collector}", h.api(h.apiCollectors))
	}
	for _, method := range []string{"PUT", "GET", "DELETE"} {
		mux.HandleFunc(method+" /admin/api/v1/backup-operations/{id}", h.acquisitionRoute(h.apiAcquisition, method == "GET"))
	}
	for _, method := range []string{"PUT", "DELETE"} {
		mux.HandleFunc(method+" /admin/api/v1/backup-operations/{id}/leases/{lease}", h.acquisitionRoute(h.apiAcquisitionLease, true))
	}
	mux.HandleFunc("GET /admin/api/v1/backup-operations/{id}/artifacts/{artifact}", h.acquisitionRoute(h.apiAcquisitionArtifact, true))
}
func (h *Handler) acquisitionStanding(administrator string) error {
	if !h.authority.AdministersServer(administrator) {
		return store.Refuse("permission_denied")
	}
	if h.acquisitions == nil {
		return store.Refuse("not_supported")
	}
	return nil
}
func (h *Handler) apiAcquisition(w http.ResponseWriter, r *http.Request, administrator string, body []byte) (int, any, error) {
	if err := h.acquisitionReadStanding(r, administrator); err != nil {
		return 0, nil, err
	}
	id := r.PathValue("id")
	switch r.Method {
	case "PUT":
		if isCollector(r) {
			return 0, nil, store.Refuse("permission_denied")
		}
		var request struct {
			Kind string `json:"kind"`
		}
		if err := decode(body, &request); err != nil {
			return 0, nil, err
		}
		op, err := h.acquisitions.Start(id, request.Kind)
		return 202, op, err
	case "DELETE":
		if isCollector(r) {
			return 0, nil, store.Refuse("permission_denied")
		}
		op, err := h.acquisitions.Cancel(id)
		return 200, op, err
	default:
		if isCollector(r) {
			op, err := h.acquisitions.CompletedStatus(id)
			return 200, op, err
		}
		op, err := h.acquisitions.Status(id)
		return 200, op, err
	}
}
func (h *Handler) apiAcquisitionLease(w http.ResponseWriter, r *http.Request, administrator string, body []byte) (int, any, error) {
	if err := h.acquisitionReadStanding(r, administrator); err != nil {
		return 0, nil, err
	}
	if len(body) != 0 {
		return 0, nil, store.Refuse("invalid_request")
	}
	expiry, err := h.acquisitions.Lease(r.PathValue("id"), r.PathValue("lease"), r.Method == "DELETE")
	return 200, map[string]any{"id": r.PathValue("lease"), "expires_at": expiry}, err
}
func (h *Handler) apiAcquisitionArtifact(w http.ResponseWriter, r *http.Request, administrator string, body []byte) (int, any, error) {
	if err := h.acquisitionReadStanding(r, administrator); err != nil {
		return 0, nil, err
	}
	f, a, err := h.acquisitions.Read(r.PathValue("id"), r.PathValue("artifact"), r.Header.Get("Tessera-Backup-Lease"))
	if err != nil {
		return 0, nil, err
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(a.Size, 10))
	w.Header().Set("Content-Disposition", `attachment; filename="`+a.ID+`"`)
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(2 * time.Minute))
	w.WriteHeader(200)
	_, _ = io.Copy(w, io.LimitReader(f, a.Size))
	return 0, nil, nil
}

// Only explicitly registered retrieval routes invoke this alternative authenticator.
// All other routes continue using identity.Authority and cannot accept these tokens.
type collectorContextKey struct{}

func isCollector(r *http.Request) bool {
	allowed, _ := r.Context().Value(collectorContextKey{}).(bool)
	return allowed
}
func (h *Handler) acquisitionReadStanding(r *http.Request, administrator string) error {
	if isCollector(r) && h.acquisitions != nil {
		return nil
	}
	return h.acquisitionStanding(administrator)
}
func (h *Handler) acquisitionRoute(handle apiHandler, retrieval bool) http.HandlerFunc {
	ordinary := h.api(handle)
	return func(w http.ResponseWriter, r *http.Request) {
		fields := strings.Fields(r.Header.Get("Authorization"))
		if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") || !strings.HasPrefix(fields[1], acquisition.CollectorPrefix) {
			ordinary(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if h.acquisitions == nil {
			apiFailure(w, store.Refuse("authentication_failed"))
			return
		}
		if err := h.acquisitions.AuthenticateCollector(fields[1]); err != nil {
			apiFailure(w, err)
			return
		}
		if !retrieval {
			apiFailure(w, store.Refuse("permission_denied"))
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil || len(body) != 0 {
			apiFailure(w, store.Refuse("invalid_request"))
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), collectorContextKey{}, true))
		status, result, err := handle(w, r, "", body)
		if err != nil {
			apiFailure(w, err)
			return
		}
		if status != 0 {
			w.WriteHeader(status)
			if result != nil {
				_ = json.NewEncoder(w).Encode(result)
			}
		}
	}
}
func (h *Handler) apiAcquisitionCatalog(w http.ResponseWriter, r *http.Request, administrator string, body []byte) (int, any, error) {
	if err := h.acquisitionReadStanding(r, administrator); err != nil {
		return 0, nil, err
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(body) != 0 {
		return 0, nil, store.Refuse("invalid_request")
	}
	for key, values := range q {
		if (key != "limit" && key != "cursor") || len(values) != 1 {
			return 0, nil, store.Refuse("invalid_request")
		}
	}
	limit := 0
	if values, ok := q["limit"]; ok {
		limit, err = strconv.Atoi(values[0])
		if err != nil || limit < 1 || limit > 100 {
			return 0, nil, store.Refuse("invalid_field")
		}
	}
	page, err := h.acquisitions.Catalog(limit, q.Get("cursor"))
	return 200, page, err
}
func (h *Handler) apiCollectors(w http.ResponseWriter, r *http.Request, administrator string, body []byte) (int, any, error) {
	if err := h.acquisitionStanding(administrator); err != nil {
		return 0, nil, err
	}
	switch r.Method {
	case "PUT":
		var request struct {
			Lifetime int64 `json:"expires_in_seconds"`
		}
		if err := decode(body, &request); err != nil {
			return 0, nil, err
		}
		if request.Lifetime < 60 || request.Lifetime > int64(acquisition.MaxCollectorLifetime/time.Second) {
			return 0, nil, store.Refuse("invalid_field")
		}
		credential, err := h.acquisitions.ProvisionCollector(r.PathValue("collector"), time.Duration(request.Lifetime)*time.Second)
		return 201, credential, err
	case "DELETE":
		if len(body) != 0 {
			return 0, nil, store.Refuse("invalid_request")
		}
		return 204, nil, h.acquisitions.RevokeCollector(r.PathValue("collector"))
	default:
		if len(body) != 0 {
			return 0, nil, store.Refuse("invalid_request")
		}
		collectors, err := h.acquisitions.Collectors()
		return 200, map[string]any{"collectors": collectors}, err
	}
}
