package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ekarpov/tessera/pkg/conformance"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/server"
	"github.com/ekarpov/tessera/pkg/store"
	"github.com/ekarpov/tessera/pkg/store/sqlite"
)

func sqliteFixture(t *testing.T, reads ...string) (*sqlite.Store, http.Handler, string) {
	t.Helper()
	s, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "dataset.db"), p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.EnableReads(reads...); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	for name, grant := range map[string]store.Grants{"owner": {Read: true, Write: true, Schema: true}, "refused": {Write: true}} {
		if err = s.SetAuthorization(t.Context(), sole(t, s), store.Access{Principal: "synthetic", Authorization: name}, store.Authorization{Grants: grant, View: store.FullView()}); err != nil {
			t.Fatal(err)
		}
	}
	h, err := server.NewWithOptions(s, func(_ context.Context, token string) (store.Access, error) {
		if token == "guest" || token == "contributor" {
			return store.Access{Principal: token, Authorization: token}, nil
		}
		if token != "owner" && token != "refused" {
			return store.Access{}, store.Refuse("authentication_failed")
		}
		return store.Access{Principal: "synthetic", Authorization: token}, nil
	}, server.Options{DisplayName: "Synthetic Home", AuthMethods: []string{"bearer_token"}})
	if err != nil {
		t.Fatal(err)
	}
	held, err := s.Datasets(t.Context(), store.Access{Principal: "synthetic", Authorization: "owner"})
	if err != nil || len(held) != 1 {
		t.Fatal("expected exactly one dataset", err)
	}
	return s, h, "/v1/datasets/" + held[0].ID
}

// shareWith installs a read-only guest grant the way a host would, and returns
// the token that authenticates as it. Grant administration is outside the
// protocol, so the suite cannot do this for itself.
func shareWith(t *testing.T, s *sqlite.Store) func(context.Context, []string) (string, error) {
	t.Helper()
	return func(ctx context.Context, types []string) (string, error) {
		access := store.Access{Principal: "guest", Authorization: "guest"}
		auth := store.Authorization{Grants: store.Grants{Read: true}, View: store.SharedView(types)}
		if err := s.SetAuthorization(ctx, sole(t, s), access, auth); err != nil {
			return "", err
		}
		return "guest", nil
	}
}

func contributeWith(t *testing.T, s *sqlite.Store) func(context.Context, []string) (string, error) {
	t.Helper()
	return func(ctx context.Context, types []string) (string, error) {
		access := store.Access{Principal: "contributor", Authorization: "contributor"}
		auth := store.Authorization{Grants: store.Grants{Insert: true}, View: store.SharedView(nil).WithInserts(types)}
		if err := s.SetAuthorization(ctx, sole(t, s), access, auth); err != nil {
			return "", err
		}
		return "contributor", nil
	}
}

func TestSQLiteConformance(t *testing.T) {
	s, h, _ := sqliteFixture(t)
	server := httptest.NewServer(h)
	defer server.Close()
	f := conformance.CoreFixture{URL: server.URL, Token: "owner", RefusedToken: "refused", Client: server.Client(), Share: shareWith(t, s), Contribute: contributeWith(t, s)}
	if err := conformance.CheckCore(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	if err := conformance.CheckPaging(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	if err := conformance.CheckRecovery(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	if err := conformance.CheckSharing(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	if err := s.EnableContent(t.Context(), filepath.Join(t.TempDir(), "content")); err != nil {
		t.Fatal(err)
	}
	contentServer := httptest.NewUnstartedServer(nil)
	contentHandler, err := newContentHandler(s, "http://"+contentServer.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	contentServer.Config.Handler = contentHandler
	contentServer.Start()
	defer contentServer.Close()
	f.URL = contentServer.URL
	f.Client = contentServer.Client()
	if err := conformance.CheckContent(t.Context(), f); err != nil {
		t.Fatal(err)
	}

}
func keyedCall(t *testing.T, h http.Handler, method, path, body, key string, status int) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer owner")
	r.Header.Set("Idempotency-Key", key)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != status {
		t.Fatalf("expected %d got %d: %s", status, w.Code, w.Body.String())
	}
	return w
}
func TestHTTPRecoveryAfterRevocation(t *testing.T) {
	s, h, base := sqliteFixture(t)
	const key = "10000000-0000-0000-0000-000000000001"
	const body = `{"operations":[{"op":"define_type","meta":{"id":"test.revoke","fields":[]}},{"op":"insert","type":"test.revoke","fields":{}}]}`
	keyedCall(t, h, "POST", base+":transact", body, key, 200)
	if err := s.Revoke(t.Context(), sole(t, s), store.Access{Principal: "synthetic", Authorization: "owner"}); err != nil {
		t.Fatal(err)
	}
	got := wireCall(t, h, "POST", base+"/operations/"+key+":status", "owner", "", 200)
	if !strings.Contains(string(got), `"result_withheld":true`) {
		t.Fatal("revoked owner cannot recover")
	}
	replay := keyedCall(t, h, "POST", base+":transact", body, key, 200)
	if replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("receipt wasn't replayed")
	}
	// A revoked caller holds no grant, so it is told unavailable rather than
	// permission_denied — it may not learn the dataset is still there. Its
	// receipt is recoverable all the same, which is what this test is about.
	wireCall(t, h, "GET", base+"/meta", "owner", "", 404)
	var poll struct {
		Datasets []struct {
			ViewVersion string                     `json:"view_version"`
			Access      map[string]json.RawMessage `json:"access"`
		} `json:"datasets"`
	}
	if err := json.Unmarshal(wireCall(t, h, "GET", "/v1/datasets", "refused", "", 200), &poll); err != nil {
		t.Fatal(err)
	}
	if len(poll.Datasets) != 1 || poll.Datasets[0].ViewVersion == "" || string(poll.Datasets[0].Access["changes_at"]) != "null" {
		t.Fatal("discovery not aligned with current spec")
	}
}

// Simulate a lost response after a successful commit. The body is never delivered
// to the client; a retry must return the same creation instead of doing it again.
func TestDroppedCommittedResponseIsRecoverable(t *testing.T) {
	_, h, base := sqliteFixture(t)
	wireCall(t, h, "POST", base+"/meta", "owner", `{"type":{"id":"test.lost","fields":[]}}`, 201)
	const key = "10000000-0000-0000-0000-000000000002"
	drop := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(httptest.NewRecorder(), r)
		w.WriteHeader(503)
	})
	keyedCall(t, drop, "POST", base+"/objects/test.lost", `{"fields":{}}`, key, 503)
	reply := keyedCall(t, h, "POST", base+"/objects/test.lost", `{"fields":{}}`, key, 201)
	if reply.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("lost response reexecuted write")
	}
	count := wireCall(t, h, "POST", base+"/objects/test.lost:count", "owner", `{}`, 200)
	if !strings.Contains(string(count), `"count":1`) {
		t.Fatal("duplicate insert after lost response")
	}
}

func newContentHandler(s store.Store, origin string) (http.Handler, error) {
	return server.NewWithOptions(s, func(_ context.Context, token string) (store.Access, error) {
		if token == "guest" || token == "contributor" {
			return store.Access{Principal: token, Authorization: token}, nil
		}
		if token != "owner" && token != "refused" {
			return store.Access{}, store.Refuse("authentication_failed")
		}
		return store.Access{Principal: "synthetic", Authorization: token}, nil
	}, server.Options{AuthMethods: []string{"bearer_token"}, ContentOrigin: origin})
}

type replaceBetweenInfoAndRead struct {
	store.Store
	replace func()
}

func (s *replaceBetweenInfoAndRead) Info(ctx context.Context, a store.Access, dataset string) (store.Info, error) {
	info, err := s.Store.Info(ctx, a, dataset)
	if err == nil && s.replace != nil {
		replace := s.replace
		s.replace = nil
		replace()
	}
	return info, err
}
func TestOldDatasetRouteCannotCrossConcurrentErase(t *testing.T) {
	s, _, base := sqliteFixture(t)
	a := store.Access{Principal: "synthetic", Authorization: "owner"}
	if err := s.SetAuthorization(t.Context(), sole(t, s), a, store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true, Erase: true}, View: store.FullView()}); err != nil {
		t.Fatal(err)
	}
	dataset := strings.TrimPrefix(base, "/v1/datasets/")
	wrapper := &replaceBetweenInfoAndRead{Store: s, replace: func() {
		if _, err := s.Erase(t.Context(), a, dataset, dataset); err != nil {
			t.Fatal(err)
		}
	}}
	h, err := server.New(wrapper, func(context.Context, string) (store.Access, error) { return a, nil })
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", base+"/meta", nil)
	r.Header.Set("Authorization", "Bearer owner")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 404 || !strings.Contains(w.Body.String(), "not_found") {
		t.Fatal("old route crossed dataset generation", w.Code, w.Body.String())
	}
}

// The same optional read contract on the durable engine, withheld and
// advertised. Advertising is composition, so each run gets its own store.
func TestSQLiteOptionalReads(t *testing.T) {
	// One case rather than two. There is no withheld shape any more: these are
	// required operations, so a server either behaves or is not conformant.
	for _, reads := range [][]string{{}} {
		_, h, _ := sqliteFixture(t, reads...)
		server := httptest.NewServer(h)
		f := conformance.CoreFixture{URL: server.URL, Token: "owner", RefusedToken: "refused", Client: server.Client()}
		err := conformance.CheckOptionalReads(t.Context(), f)
		server.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
}

// Erasure is the one mutation that does not take an idempotency key: its
// confirmation and replacement discovery are the recovery mechanism, and a
// receipt would belong to a dataset that no longer exists. Resolved as review
// finding 10; the refusal must come before anything is retired.
func TestErasureRefusesAnIdempotencyKeyBeforeRetiring(t *testing.T) {
	s, h, base := sqliteFixture(t)
	if err := s.SetAuthorization(t.Context(), sole(t, s), store.Access{Principal: "synthetic", Authorization: "owner"},
		store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true, Erase: true}, View: store.FullView()}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	dataset := strings.TrimPrefix(base, "/v1/datasets/")
	call := func(path, key, body string) *http.Response {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), "POST", server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer owner")
		request.Header.Set("Content-Type", "application/json")
		if key != "" {
			request.Header.Set("Idempotency-Key", key)
		}
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	const uuid = "3f2504e0-4f89-11d3-9a0c-0305e82c3301"
	for _, path := range []string{base + ":erase", base + ":erase-preview"} {
		response := call(path, uuid, `{"confirm":"`+dataset+`"}`)
		var envelope struct {
			Error p.Error `json:"error"`
		}
		_ = json.NewDecoder(response.Body).Decode(&envelope)
		response.Body.Close()
		if response.StatusCode != 400 || envelope.Error.Code != "invalid_request" {
			t.Fatal("a key was not refused with invalid_request", path, response.StatusCode, envelope.Error.Code)
		}
	}
	// Nothing was retired by those refusals: the dataset still answers.
	if response := call(base+":erase-preview", "", `{}`); response.StatusCode != 200 {
		t.Fatal("a refused key retired something", response.StatusCode)
	} else {
		response.Body.Close()
	}
	// Without a key it erases, and the retired identity stops answering, so a
	// replayed erase cannot reach the replacement that took its place.
	response := call(base+":erase", "", `{"confirm":"`+dataset+`"}`)
	if response.StatusCode != 200 {
		t.Fatal("unkeyed erase refused", response.StatusCode)
	}
	response.Body.Close()
	replay := call(base+":erase", "", `{"confirm":"`+dataset+`"}`)
	defer replay.Body.Close()
	if replay.StatusCode != 404 {
		t.Fatal("a replayed erase reached the replacement dataset", replay.StatusCode)
	}
}

// A server that advertises sharing but whose fixture cannot install a guest
// must fail the suite rather than skip it. Otherwise an advertised capability
// with no evidence behind it looks identical to one that was checked.
func TestSharingSuiteRefusesToSkipAnAdvertisedCapability(t *testing.T) {
	_, h, _ := sqliteFixture(t)
	server := httptest.NewServer(h)
	defer server.Close()
	f := conformance.CoreFixture{URL: server.URL, Token: "owner", RefusedToken: "refused", Client: server.Client()}
	err := conformance.CheckSharing(t.Context(), f)
	if err == nil || !strings.Contains(err.Error(), "must supply Share") {
		t.Fatal("the suite skipped an advertised capability", err)
	}
}

// Creating a dataset is not in the contract, so the route is refused unless a
// deployment has said otherwise twice: the host supplies a decision, and the
// engine can make one. Recorded as review finding 16.
func TestDatasetCreationIsRefusedUnlessOfferedTwice(t *testing.T) {
	build := func(t *testing.T, provision server.Provision) (*sqlite.Store, http.Handler) {
		t.Helper()
		s, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "dataset.db"), p.DevelopmentLimits())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		h, err := server.NewWithOptions(s, func(_ context.Context, token string) (store.Access, error) {
			if token != "owner" && token != "other" {
				return store.Access{}, store.Refuse("authentication_failed")
			}
			return store.Access{Principal: "synthetic", Authorization: token}, nil
		}, server.Options{AuthMethods: []string{"bearer_token"}, Provision: provision})
		if err != nil {
			t.Fatal(err)
		}
		return s, h
	}
	create := func(h http.Handler, token, body string, headers ...string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/v1/datasets", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		for i := 0; i+1 < len(headers); i += 2 {
			r.Header.Set(headers[i], headers[i+1])
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	// Authentication comes before the host's decision, so an unauthenticated
	// caller is told it did not authenticate rather than that it may not.
	_, guarded := build(t, func(context.Context, store.Access) error { return nil })
	unauthenticated := httptest.NewRequest("POST", "/v1/datasets", strings.NewReader(`{"display_name":"Second quarter"}`))
	unauthenticated.Header.Set("Content-Type", "application/json")
	anonymous := httptest.NewRecorder()
	guarded.ServeHTTP(anonymous, unauthenticated)
	if anonymous.Code != 401 || !strings.Contains(anonymous.Body.String(), "authentication_failed") {
		t.Fatal("an unauthenticated creation was not refused as one", anonymous.Code, anonymous.Body.String())
	}
	if w := create(guarded, "bearer-nobody", `{"display_name":"Second quarter"}`); w.Code != 401 {
		t.Fatal("an unknown credential was not refused as one", w.Code, w.Body.String())
	}
	// An engine that can provision, with no host decision, still refuses.
	_, silent := build(t, nil)
	if w := create(silent, "owner", `{"display_name":"Second quarter"}`); w.Code != 403 || !strings.Contains(w.Body.String(), "not_supported") {
		t.Fatal("a route nobody offered answered", w.Code, w.Body.String())
	}
	// The host's decision is the only thing standing between a caller and a new
	// dataset, so it is asked for every request and its refusal is the answer.
	s, h := build(t, func(_ context.Context, a store.Access) error {
		if a.Authorization != "owner" {
			return store.Refuse("permission_denied")
		}
		return nil
	})
	if w := create(h, "other", `{"display_name":"Second quarter"}`); w.Code != 403 {
		t.Fatal("the host's refusal was not the answer", w.Code, w.Body.String())
	}
	if w := create(h, "owner", `{"display_name":""}`); w.Code != 400 {
		t.Fatal("a nameless dataset was created", w.Code, w.Body.String())
	}
	// A creation has no dataset, so its outcome is retained under the server
	// scope. A malformed key is still refused as one.
	if w := create(h, "owner", `{"display_name":"Second quarter"}`, "Idempotency-Key", "not-a-uuid"); w.Code != 400 {
		t.Fatal("a malformed key was accepted", w.Code, w.Body.String())
	}
	// A key is required on this route, so the keyless call above is refused and
	// this one carries one.
	if keyless := create(h, "owner", `{"display_name":"Second quarter"}`); keyless.Code != 400 {
		t.Fatal("a creation without a key was accepted", keyless.Code, keyless.Body.String())
	}
	w := create(h, "owner", `{"display_name":"Second quarter"}`, "Idempotency-Key", "5c0f1a92-0000-4000-8000-00000000fe01")
	if w.Code != 201 {
		t.Fatal("creation refused", w.Code, w.Body.String())
	}
	// An identity and nothing else: no display name, no bounds, no versions.
	var created struct {
		DatasetID string `json:"dataset_id"`
	}
	if json.Unmarshal(w.Body.Bytes(), &created) != nil || created.DatasetID == "" {
		t.Fatal("creation did not report an identity", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "display_name") || strings.Contains(w.Body.String(), "limits") {
		t.Fatal("creation returned discovery state alongside the identity", w.Body.String())
	}
	// The caller that asked administers it, and nobody else sees it.
	listed := func(token string) []any {
		t.Helper()
		r := httptest.NewRequest("GET", "/v1/datasets", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		out := httptest.NewRecorder()
		h.ServeHTTP(out, r)
		var body struct {
			Datasets []any `json:"datasets"`
		}
		if out.Code != 200 || json.Unmarshal(out.Body.Bytes(), &body) != nil {
			t.Fatal("listing refused", out.Code, out.Body.String())
		}
		return body.Datasets
	}
	if held := listed("owner"); len(held) != 1 || held[0].(map[string]any)["id"] != created.DatasetID {
		t.Fatal("the creator was not made its administrator", held)
	}
	if held := listed("other"); len(held) != 0 {
		t.Fatal("a new dataset was shown to someone else", held)
	}
	// The storage that was opened still holds the dataset it started with, so
	// the new one is an addition rather than a replacement.
	provisioned, err := s.Provisioned(t.Context())
	if err != nil || len(provisioned) != 2 {
		t.Fatal("the storage does not hold both datasets", provisioned, err)
	}
}

// A creation whose response is lost must be answerable. Every other receipt is
// keyed by the dataset the work acted on; a creation has none, which is why
// this route once refused an idempotency key outright — and why a caller whose
// response was dropped could only list and try to recognise a display name,
// which is not an identity and may collide. Guessing is the one thing this
// contract never asks of a caller, so creation is retained under a server scope.
func TestALostCreationIsAnsweredRatherThanGuessed(t *testing.T) {
	s, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "dataset.db"), p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	h, err := server.NewWithOptions(s, func(_ context.Context, token string) (store.Access, error) {
		if token != "owner" && token != "other" {
			return store.Access{}, store.Refuse("authentication_failed")
		}
		return store.Access{Principal: "synthetic", Authorization: token}, nil
	}, server.Options{AuthMethods: []string{"bearer_token"}, Provision: func(context.Context, store.Access) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	const key = "3f1a9c52-0000-4000-8000-00000000abcd"
	create := func(token, body, k string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest("POST", "/v1/datasets", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		if k != "" {
			r.Header.Set("Idempotency-Key", k)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	id := func(w *httptest.ResponseRecorder) string {
		t.Helper()
		var body struct {
			DatasetID string `json:"dataset_id"`
		}
		if json.Unmarshal(w.Body.Bytes(), &body) != nil {
			t.Fatal("unreadable creation response", w.Body.String())
		}
		return body.DatasetID
	}

	first := create("owner", `{"display_name":"Second quarter"}`, key)
	if first.Code != 201 {
		t.Fatal("keyed creation refused", first.Code, first.Body.String())
	}
	made := id(first)

	// The caller never saw that response. Replaying the identical request must
	// name the same dataset rather than make a second one.
	again := create("owner", `{"display_name":"Second quarter"}`, key)
	if again.Code != 201 || id(again) != made {
		t.Fatal("a replayed creation did not name the original dataset", again.Code, again.Body.String())
	}

	// And asking is enough, without replaying the write at all.
	ask := httptest.NewRequest("POST", "/v1/operations/"+key+":status", nil)
	ask.Header.Set("Authorization", "Bearer owner")
	status := httptest.NewRecorder()
	h.ServeHTTP(status, ask)
	if status.Code != 200 || !strings.Contains(status.Body.String(), "committed") || !strings.Contains(status.Body.String(), made) {
		t.Fatal("the creation's outcome was not answerable", status.Code, status.Body.String())
	}

	// A receipt is owner-bound, and the scope does not loosen that.
	stolen := httptest.NewRequest("POST", "/v1/operations/"+key+":status", nil)
	stolen.Header.Set("Authorization", "Bearer other")
	taken := httptest.NewRecorder()
	h.ServeHTTP(taken, stolen)
	if taken.Code != 403 {
		t.Fatal("another principal read the creation's receipt", taken.Code, taken.Body.String())
	}

	// The same key for different work is a conflict, not a second answer.
	reused := create("owner", `{"display_name":"Third quarter"}`, key)
	if reused.Code != 409 || !strings.Contains(reused.Body.String(), "idempotency_key_reused") {
		t.Fatal("a reused key answered different work", reused.Code, reused.Body.String())
	}

	// Exactly one dataset came of all that.
	list := httptest.NewRequest("GET", "/v1/datasets", nil)
	list.Header.Set("Authorization", "Bearer owner")
	held := httptest.NewRecorder()
	h.ServeHTTP(held, list)
	var listing struct {
		Datasets []struct {
			ID string `json:"id"`
		} `json:"datasets"`
	}
	if json.Unmarshal(held.Body.Bytes(), &listing) != nil {
		t.Fatal(held.Body.String())
	}
	seen := 0
	for _, d := range listing.Datasets {
		if d.ID == made {
			seen++
		}
	}
	// Only the created dataset is listed: this caller holds no grant on the one
	// the storage opened with, which is the absence rule doing its job.
	if seen != 1 || len(listing.Datasets) != 1 {
		t.Fatal("the replays made more than one dataset", held.Body.String())
	}
}

// The creation suite runs against this server, which is how the contract's
// cases and this implementation are held to each other.
func TestSQLiteCreationConformance(t *testing.T) {
	s, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "dataset.db"), p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	// Server rights and the capability are deployment state, so the suite gets
	// them the way it gets grant administration: through the host.
	var mu sync.Mutex
	mayCreate, offered := true, true
	handler, err := server.NewWithOptions(s, func(_ context.Context, token string) (store.Access, error) {
		// Two principals that both authenticate. The second holds nothing on
		// anything this one creates, which is what makes a live dataset the
		// caller may not use an observable case rather than a hypothetical.
		if token != "owner" && token != "refused" {
			return store.Access{}, store.Refuse("authentication_failed")
		}
		return store.Access{Principal: "synthetic", Authorization: token}, nil
	}, server.Options{
		AuthMethods: []string{"bearer_token"},
		Provision: func(context.Context, store.Access) error {
			mu.Lock()
			defer mu.Unlock()
			if !mayCreate {
				return store.Refuse("permission_denied")
			}
			return nil
		},
		CreationOffered: func() bool {
			mu.Lock()
			defer mu.Unlock()
			return offered
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	origin := httptest.NewServer(handler)
	t.Cleanup(origin.Close)
	set := func(flag *bool) func(context.Context) (func(), error) {
		return func(context.Context) (func(), error) {
			mu.Lock()
			*flag = false
			mu.Unlock()
			return func() { mu.Lock(); *flag = true; mu.Unlock() }, nil
		}
	}
	if err = conformance.CheckCreation(t.Context(), conformance.CoreFixture{
		URL: origin.URL, Token: "owner", RefusedToken: "refused", Client: origin.Client(),
		WithdrawCreation: set(&mayCreate),
		DisableCreation:  set(&offered),
	}); err != nil {
		t.Fatal(err)
	}
}
