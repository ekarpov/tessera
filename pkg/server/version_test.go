package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/ekarpov/tessera/pkg/conformance"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/server"
	"github.com/ekarpov/tessera/pkg/store"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func versionHandler(t *testing.T, deprecated []p.DeprecatedVersion) (http.Handler, error) {
	t.Helper()
	s, _, _ := fixture(t)
	return server.NewWithOptions(s, func(_ context.Context, token string) (store.Access, error) {
		if token != "owner" && token != "refused" {
			return store.Access{}, errors.New("refused")
		}
		return store.Access{Principal: "synthetic", Authorization: token}, nil
	}, server.Options{AuthMethods: []string{"bearer_token"}, DeprecatedVersions: deprecated})
}

func TestDeprecationDiscoveryAndCore(t *testing.T) {
	for _, sunset := range []string{"", "2030-01-01T00:00:00Z", "2000-01-01T00:00:00Z"} {
		t.Run(sunset, func(t *testing.T) {
			var entries []p.DeprecatedVersion
			if sunset != "" {
				entries = []p.DeprecatedVersion{{Version: "1", Sunset: sunset}}
			}
			h, err := versionHandler(t, entries)
			if err != nil {
				t.Fatal(err)
			}
			// Mutating host-owned options after construction must not change discovery.
			if len(entries) > 0 {
				entries[0].Version = "2"
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", "/.well-known/tessera", nil))
			if w.Code != 200 {
				t.Fatal("discovery requires authentication")
			}
			if err := conformance.CheckVersionMetadata(w.Body.Bytes()); err != nil {
				t.Fatal(err)
			}
			var doc map[string]json.RawMessage
			if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
				t.Fatal(err)
			}
			raw, present := doc["deprecated_versions"]
			if present != (sunset != "") {
				t.Fatal("wrong deprecation presence")
			}
			if present {
				var got []p.DeprecatedVersion
				if json.Unmarshal(raw, &got) != nil || len(got) != 1 || got[0].Version != "1" || got[0].Sunset != sunset {
					t.Fatal("wrong announcement")
				}
			}
			live := httptest.NewServer(h)
			defer live.Close()
			if err := conformance.CheckCore(t.Context(), conformance.CoreFixture{URL: live.URL, Client: live.Client(), Token: "owner", RefusedToken: "refused"}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInvalidDeprecationStopsConstruction(t *testing.T) {
	for _, entries := range [][]p.DeprecatedVersion{
		{{Version: "2", Sunset: "2030-01-01T00:00:00Z"}},
		{{Version: "1", Sunset: "2030-02-30T00:00:00Z"}},
		{{Version: "1", Sunset: "2030-01-01T00:00:00.1Z"}},
		{{Version: "1", Sunset: "2030-01-01T00:00:00Z"}, {Version: "1", Sunset: "2031-01-01T00:00:00Z"}},
	} {
		if h, err := versionHandler(t, entries); err == nil || h != nil {
			t.Fatal("invalid deprecation accepted")
		}
	}
}

func TestCoreDetectsSeededVersionMetadataDefects(t *testing.T) {
	for _, metadata := range []string{`null`, `[{"version":"2","sunset":"2030-01-01T00:00:00Z"}]`, `[{"version":"1","sunset":"2030-02-30T00:00:00Z"}]`} {
		t.Run(metadata, func(t *testing.T) {
			_, h, _ := fixture(t)
			broken := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/.well-known/tessera" {
					h.ServeHTTP(w, r)
					return
				}
				recorder := httptest.NewRecorder()
				h.ServeHTTP(recorder, r)
				var doc map[string]json.RawMessage
				if err := json.Unmarshal(recorder.Body.Bytes(), &doc); err != nil {
					t.Fatal(err)
				}
				doc["deprecated_versions"] = json.RawMessage(metadata)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(doc)
			})
			live := httptest.NewServer(broken)
			defer live.Close()
			err := conformance.CheckCore(t.Context(), conformance.CoreFixture{URL: live.URL, Client: live.Client(), Token: "owner", RefusedToken: "refused"})
			if err == nil || !strings.Contains(err.Error(), "deprecated_versions") {
				t.Fatal("suite missed served defect", err)
			}
		})
	}
}

func TestCoreAllowsAdditiveDiscoveryFields(t *testing.T) {
	_, h, _ := fixture(t)
	extended := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/tessera" {
			h.ServeHTTP(w, r)
			return
		}
		recorder := httptest.NewRecorder()
		h.ServeHTTP(recorder, r)
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(recorder.Body.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		doc["future_response_field"] = json.RawMessage(`true`)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	})
	live := httptest.NewServer(extended)
	defer live.Close()
	if err := conformance.CheckCore(t.Context(), conformance.CoreFixture{URL: live.URL, Client: live.Client(), Token: "owner", RefusedToken: "refused"}); err != nil {
		t.Fatal(err)
	}
}

func TestCoreRefusesNoCommonVersionBeforeDataRequests(t *testing.T) {
	requests := 0
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/tessera" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"protocol_versions":["2"]}`))
			return
		}
		requests++
		w.WriteHeader(http.StatusInternalServerError)
	})
	live := httptest.NewServer(h)
	defer live.Close()
	err := conformance.CheckCore(t.Context(), conformance.CoreFixture{URL: live.URL, Client: live.Client(), Token: "owner", RefusedToken: "refused"})
	if err == nil || !strings.Contains(err.Error(), "requires advertised protocol 1") || requests != 0 {
		t.Fatal("suite requested data without a common version", err, requests)
	}
}
