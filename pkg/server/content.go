package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

func (h *handler) content(w http.ResponseWriter, r *http.Request) {
	backend, ok := h.engine.(store.Content)
	if !ok {
		failure(w, store.Refuse("not_supported"))
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v1/transfers/") {
		if r.URL.RawQuery != "" {
			failure(w, store.Refuse("invalid_field"))
			return
		}
		token := strings.TrimPrefix(r.URL.Path, "/v1/transfers/")
		read, err := backend.ContentTransfer(r.Context(), token, r.Method, r.Header.Get("Content-Range"), r.Body)
		if err != nil {
			failure(w, err)
			return
		}
		if read.Body != nil {
			defer read.Body.Close()
			w.Header().Set("Content-Type", read.ContentType)
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Content-Disposition", "attachment")
			w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
			_, _ = io.Copy(w, read.Body)
		} else {
			w.WriteHeader(204)
		}
		return
	}
	a, err := h.access(r)
	if err != nil {
		failure(w, err)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.EscapedPath(), "/v1/stores"), "/")
	for i := range parts {
		parts[i], err = url.PathUnescape(parts[i])
		if err != nil {
			failure(w, store.Refuse("invalid_identifier"))
			return
		}
	}
	action, storeID, id := "", "", ""
	if r.URL.Path == "/v1/stores" && r.Method == "GET" {
		action = "list"
	} else if len(parts) >= 2 {
		storeID = parts[1]
		if len(parts) == 2 && r.Method == "POST" && strings.HasSuffix(storeID, ":erase") {
			action = "erase"
			storeID = strings.TrimSuffix(storeID, ":erase")
			// Store erasure follows the record protocol's rule: no idempotency
			// key, refused before anything is retired rather than silently
			// ignored by a caller that believed it had recovery. Imports carry
			// their own recovery in operation_id, so the header is only
			// meaningless there, not dangerous.
			if len(r.Header.Values("Idempotency-Key")) > 0 {
				failure(w, store.Refuse("invalid_request"))
				return
			}
		}
		if len(parts) == 3 && parts[2] == "imports" && r.Method == "POST" {
			action = "begin"
		}
		if len(parts) == 4 {
			id = parts[3]
			if parts[2] == "imports" {
				if r.Method == "DELETE" {
					action = "abort"
				} else if r.Method == "POST" && strings.HasSuffix(id, ":commit") {
					action = "commit"
					id = strings.TrimSuffix(id, ":commit")
				}
			}
			if parts[2] == "items" {
				if r.Method == "GET" {
					action = "inspect"
				} else if r.Method == "DELETE" {
					action = "delete"
				} else if r.Method == "POST" && strings.HasSuffix(id, ":resolve") {
					action = "resolve"
					id = strings.TrimSuffix(id, ":resolve")
				}
			}
		}
	}
	if action == "" {
		failure(w, store.Refuse("not_found"))
		return
	}
	if action != "list" && p.ValidateIdentifier(p.DatasetIdentifier, storeID) != nil {
		failure(w, store.Refuse("invalid_identifier"))
		return
	}
	if id != "" && p.ValidateIdentifier(p.ContentIdentifier, id) != nil {
		failure(w, store.Refuse("invalid_identifier"))
		return
	}
	var body []byte
	if action == "inspect" {
		query, e := url.ParseQuery(r.URL.RawQuery)
		if e != nil || len(query) != 1 || len(query["representation"]) != 1 {
			failure(w, store.Refuse("invalid_field"))
			return
		}
		body, _ = json.Marshal(map[string]string{"representation": query.Get("representation")})
	} else {
		if r.URL.RawQuery != "" {
			failure(w, store.Refuse("invalid_field"))
			return
		}
		if action == "begin" || action == "resolve" || action == "erase" {
			body, err = h.readBody(w, r, 4096)
		} else {
			err = emptyBody(w, r)
		}
		if err != nil {
			failure(w, err)
			return
		}
	}
	result, err := backend.ContentControl(r.Context(), a, action, storeID, id, body)
	if err != nil {
		failure(w, err)
		return
	}
	if response, ok := result.(map[string]any); ok {
		if path, ok := response["url"].(string); ok {
			if h.contentOrigin == "" {
				failure(w, store.Refuse("store_unavailable"))
				return
			}
			response["url"] = strings.TrimRight(h.contentOrigin, "/") + path
		}
	}
	if action == "delete" || action == "abort" {
		w.WriteHeader(204)
		return
	}
	send(w, 200, result)
}
