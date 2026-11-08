package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ekarpov/tessera/pkg/conformance"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/server"
	"github.com/ekarpov/tessera/pkg/store"
	"github.com/ekarpov/tessera/pkg/store/memory"
)

func fixture(t *testing.T, reads ...string) (*memory.Store, http.Handler, string) {
	t.Helper()
	limits := p.DevelopmentLimits()
	limits.MaxRequestBytes = 1024
	s, err := memory.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.EnableReads(reads...); err != nil {
		t.Fatal(err)
	}
	for token, g := range map[string]store.Grants{"owner": {Read: true, Write: true, Schema: true}, "refused": {Write: true}} {
		if err = s.SetAuthorization(t.Context(), sole(t, s), store.Access{Principal: "synthetic", Authorization: token}, store.Authorization{Grants: g, View: store.FullView()}); err != nil {
			t.Fatal(err)
		}
	}
	h, err := server.NewWithOptions(s, func(_ context.Context, token string) (store.Access, error) {
		if token != "owner" && token != "refused" {
			return store.Access{}, errors.New("secret must not leak")
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
func TestCoreConformance(t *testing.T) {
	_, h, _ := fixture(t)
	s := httptest.NewServer(h)
	defer s.Close()
	if err := conformance.CheckCore(t.Context(), conformance.CoreFixture{URL: s.URL, Token: "owner", RefusedToken: "refused", Client: s.Client()}); err != nil {
		t.Fatal(err)
	}
}
func TestSuiteDetectsSeededFalseDeletion(t *testing.T) {
	_, h, _ := fixture(t)
	broken := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.Contains(r.URL.Path, "/meta/") && r.Header.Get("Authorization") == "Bearer refused" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `{"error":{"code":"not_found","message":"absent"}}`)
			return
		}
		h.ServeHTTP(w, r)
	})
	s := httptest.NewServer(broken)
	defer s.Close()
	err := conformance.CheckCore(t.Context(), conformance.CoreFixture{URL: s.URL, Token: "owner", RefusedToken: "refused", Client: s.Client()})
	if err == nil || !strings.Contains(err.Error(), "expected HTTP 403, received 404") {
		t.Fatal("suite missed seeded defect", err)
	}
}
func TestRequestRejections(t *testing.T) {
	_, h, base := fixture(t)
	for _, tc := range []struct {
		name, body, token, encoding, key string
		status                           int
		code                             string
	}{
		{name: "duplicate", body: `{"type":{},"type":{}}`, token: "owner", status: 400, code: "invalid_field"},
		{name: "unknown", body: `{"typo":1}`, token: "owner", status: 400, code: "invalid_request"},
		{name: "missing", body: `{}`, token: "owner", status: 400, code: "invalid_field"},
		{name: "oversize", body: strings.Repeat(" ", 1025), token: "owner", status: 413, code: "query_result_limit_exceeded"},
		{name: "encoding", body: `{}`, token: "owner", encoding: "gzip", status: 400, code: "invalid_field"},
		{name: "unauthenticated", body: `{}`, token: "bad", status: 401, code: "authentication_failed"},
		{name: "idempotency", body: `{}`, token: "owner", key: "unsupported", status: 501, code: "not_implemented"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", base+"/meta", strings.NewReader(tc.body))
			r.Header.Set("Authorization", "Bearer "+tc.token)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Content-Encoding", tc.encoding)
			if tc.key != "" {
				r.Header.Set("Idempotency-Key", tc.key)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			var body struct {
				Error p.Error `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if w.Code != tc.status || body.Error.Code != tc.code {
				t.Fatalf("got %d %s", w.Code, body.Error.Code)
			}
			if strings.Contains(w.Body.String(), "secret") {
				t.Fatal("credential detail leaked")
			}
		})
	}
}

// A request body is network I/O. Reading it while holding the store transaction
// would deadlock this probe, just as a slow client would block other requests.
type probeBody struct {
	io.Reader
	s       *memory.Store
	dataset string
	ctx     context.Context
	err     error
}

func (b *probeBody) Read(data []byte) (int, error) {
	_, b.err = b.s.Info(b.ctx, store.Access{Principal: "synthetic", Authorization: "owner"}, b.dataset)
	if b.err != nil {
		return 0, b.err
	}
	return b.Reader.Read(data)
}
func (b *probeBody) Close() error { return nil }
func TestBodyReadOutsideTransaction(t *testing.T) {
	s, h, base := fixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	body := &probeBody{Reader: bytes.NewBufferString(`{}`), s: s, dataset: strings.TrimPrefix(base, "/v1/datasets/"), ctx: ctx}
	r := httptest.NewRequest("POST", base+"/meta", nil)
	r.Body = body
	r.Header.Set("Authorization", "Bearer owner")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if body.err != nil || w.Code != 400 {
		t.Fatal(body.err, w.Code)
	}
}

func TestWithdrawalNeverLooksDeleted(t *testing.T) {
	s, h, base := fixture(t)
	owner := store.Access{Principal: "synthetic", Authorization: "owner"}
	const typeID = "test.withdrawal"
	var id string
	err := s.Transact(t.Context(), owner, strings.TrimPrefix(base, "/v1/datasets/"), func(tx store.Tx) error {
		if _, e := tx.CreateMetadata(p.Meta{ID: typeID}); e != nil {
			return e
		}
		r, e := tx.Insert(typeID, nil)
		if e == nil {
			id = r.Records[0].ID.ID
		}
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetAuthorization(t.Context(), sole(t, s), owner, store.Authorization{Grants: store.Grants{Read: true}, View: store.ObjectView()}); err != nil {
		t.Fatal(err)
	}
	var previous string
	for _, objectID := range []string{id, "never-created", "encoded%2Fid"} {
		r := httptest.NewRequest("GET", base+"/objects/"+typeID+"/"+objectID, nil)
		r.Header.Set("Authorization", "Bearer owner")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 404 || !strings.Contains(w.Body.String(), `"code":"unavailable"`) {
			t.Fatalf("withdrawal response: %d %s", w.Code, w.Body.String())
		}
		if previous != "" && previous != w.Body.String() {
			t.Fatal("hidden existence affects response")
		}
		previous = w.Body.String()
	}
	if err = s.Revoke(t.Context(), sole(t, s), owner); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/v1/datasets", nil)
	r.Header.Set("Authorization", "Bearer owner")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	// The listing now also reports server_access, so this asserts what it means
	// rather than the whole body: a revoked caller holds no datasets, and is
	// told that by absence rather than by a refusal that would look like loss.
	var listing struct {
		Datasets []json.RawMessage `json:"datasets"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &listing) != nil || len(listing.Datasets) != 0 {
		t.Fatal("revoked storage still listed", w.Code, w.Body.String())
	}
}

func TestDiscoveryDoesNotPromiseRecovery(t *testing.T) {
	_, h, _ := fixture(t)
	r := httptest.NewRequest("GET", "/v1/datasets", nil)
	r.Header.Set("Authorization", "Bearer owner")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || strings.Contains(w.Body.String(), "recoverable_insert") || !strings.Contains(w.Body.String(), `"max_request_bytes":1024`) {
		t.Fatal("incorrect capabilities or bounds")
	}
}

func TestConformanceAcceptsDifferentProjectionOrder(t *testing.T) {
	_, h, _ := fixture(t)
	reordered := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		for k, vs := range rec.Header() {
			w.Header()[k] = vs
		}
		w.WriteHeader(rec.Code)
		if rec.Code == 200 && r.Method == "GET" && strings.Contains(r.URL.Path, "/objects/") {
			var rs p.RecordSet
			if err := json.Unmarshal(rec.Body.Bytes(), &rs); err != nil {
				t.Error(err)
				return
			}
			for i, j := 0, len(rs.Meta.Fields)-1; i < j; i, j = i+1, j-1 {
				rs.Meta.Fields[i], rs.Meta.Fields[j] = rs.Meta.Fields[j], rs.Meta.Fields[i]
				for k := range rs.Records {
					v := rs.Records[k].Values
					v[i], v[j] = v[j], v[i]
				}
			}
			_ = json.NewEncoder(w).Encode(rs)
			return
		}
		_, _ = w.Write(rec.Body.Bytes())
	})
	s := httptest.NewServer(reordered)
	defer s.Close()
	if err := conformance.CheckCore(t.Context(), conformance.CoreFixture{URL: s.URL, Token: "owner", RefusedToken: "refused", Client: s.Client()}); err != nil {
		t.Fatal(err)
	}
}

func TestSuiteDetectsAnnotationRewrite(t *testing.T) {
	_, h, _ := fixture(t)
	broken := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		for k, vs := range rec.Header() {
			w.Header()[k] = vs
		}
		w.WriteHeader(rec.Code)
		// This common encoding/json round trip silently compacts nested raw values.
		var body any
		if json.Unmarshal(rec.Body.Bytes(), &body) == nil {
			_ = json.NewEncoder(w).Encode(body)
		} else {
			_, _ = w.Write(rec.Body.Bytes())
		}
	})
	s := httptest.NewServer(broken)
	defer s.Close()
	if err := conformance.CheckCore(t.Context(), conformance.CoreFixture{URL: s.URL, Token: "owner", RefusedToken: "refused", Client: s.Client()}); err == nil || !strings.Contains(err.Error(), "metadata round trip") {
		t.Fatal("suite missed rewritten annotation bytes", err)
	}
}

// The optional read capabilities are checked twice: withheld, where every one of
// them must be refused, and advertised, where each must actually behave. A
// composition may not answer an unadvertised capability approximately.
// Aggregation, distinct references and text matching are required operations,
// so there is one case rather than two: every server behaves, and none may
// advertise them as a choice it has.
func TestRequiredReadsBehave(t *testing.T) {
	_, h, _ := fixture(t)
	s := httptest.NewServer(h)
	defer s.Close()
	if err := conformance.CheckOptionalReads(t.Context(), conformance.CoreFixture{URL: s.URL, Token: "owner", RefusedToken: "refused", Client: s.Client()}); err != nil {
		t.Fatal(err)
	}
}

// An unknown or repeated capability name is a composition mistake, not a
// silently ignored one: a server must never advertise what it cannot do.
func TestUnknownReadCapabilityRefused(t *testing.T) {
	s, err := memory.New(p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	for _, names := range [][]string{{"locales"}, {"content"}, {"sharing"}, {""}} {
		if err = s.EnableReads(names...); p.ErrorCode(err) != "invalid_field" {
			t.Fatal("accepted a capability it does not implement", names)
		}
	}
	// A name that used to be a capability is refused rather than ignored: a
	// deployment naming one is asking to withhold something this protocol no
	// longer lets it withhold, and accepting it silently would leave the
	// configuration claiming a choice nobody has.
	for _, retired := range p.RetiredReadCapabilities {
		if err = s.EnableReads(retired); err == nil {
			t.Fatal("accepted a retired capability name as if it were a choice", retired)
		}
	}
	if err = s.EnableReads(); err != nil {
		t.Fatal(err)
	}
	if got := s.ReadCapabilities(); len(got) != 0 {
		t.Fatal("advertised a capability when there are none to advertise", got)
	}
}

// The defects this suite exists to catch are the ones an implementation
// reaches for when the edge case is inconvenient: dropping the null group,
// truncating a list that exceeded its bound, and reporting zero where the
// specification requires null. A suite that passes against them proves nothing.
//
// The last two are about grouping by a reference, and they are the plausible
// mistakes rather than invented ones. Comparing object_id alone is what an
// implementation does when it reaches for a map key and the object identifier
// is the part that looks like one — and it gives a wrong answer that looks
// entirely well formed, because every group it returns is a real group with a
// real total. Answering with the object identifier alone instead of the
// qualified triple is the same reach one layer out, and a client that assumed
// its own dataset would decode it without complaint.
func TestSuiteDetectsSeededOptionalReadDefects(t *testing.T) {
	for _, tc := range []struct{ name, expect string }{
		{"dropped_null_group", "null group"},
		{"truncated_references", "expected HTTP 422"},
		{"zero_for_null", "null statistics"},
		{"grouped_by_object_id", "whole qualified identity"},
		{"group_without_qualification", "all three members"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, h, _ := fixture(t)
			broken := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				recorder := httptest.NewRecorder()
				h.ServeHTTP(recorder, r)
				body := recorder.Body.Bytes()
				switch {
				case tc.name == "dropped_null_group" && recorder.Code == 200 && strings.HasSuffix(r.URL.Path, ":aggregate"):
					body = seedDroppedNullGroup(body)
				case tc.name == "truncated_references" && strings.HasSuffix(r.URL.Path, ":distinct-references"):
					if recorder.Code == 422 {
						recorder.Code = 200
						body = []byte(`{"references":[],"watermark":"w:seeded"}`)
					}
				case tc.name == "zero_for_null" && recorder.Code == 200 && strings.HasSuffix(r.URL.Path, ":aggregate"):
					body = bytes.ReplaceAll(body, []byte(`"sum":null`), []byte(`"sum":0`))
				case tc.name == "grouped_by_object_id" && recorder.Code == 200 && strings.HasSuffix(r.URL.Path, ":aggregate"):
					body = seedGroupsFoldedByObjectID(body)
				case tc.name == "group_without_qualification" && recorder.Code == 200 && strings.HasSuffix(r.URL.Path, ":aggregate"):
					body = seedGroupsWithoutQualification(body)
				}
				for key, values := range recorder.Header() {
					w.Header()[key] = values
				}
				w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				w.WriteHeader(recorder.Code)
				_, _ = w.Write(body)
			})
			s := httptest.NewServer(broken)
			defer s.Close()
			err := conformance.CheckOptionalReads(t.Context(), conformance.CoreFixture{URL: s.URL, Token: "owner", RefusedToken: "refused", Client: s.Client()})
			if err == nil || !strings.Contains(err.Error(), tc.expect) {
				t.Fatal("suite missed seeded defect", err)
			}
		})
	}
}

func seedDroppedNullGroup(body []byte) []byte {
	var response struct {
		Aggregates []json.RawMessage `json:"aggregates"`
		Watermark  string            `json:"watermark"`
	}
	if json.Unmarshal(body, &response) != nil {
		return body
	}
	kept := []json.RawMessage{}
	for _, row := range response.Aggregates {
		// An absent group member is an ungrouped answer, which this defect
		// leaves alone; a present null one is the group being dropped.
		var members map[string]json.RawMessage
		if json.Unmarshal(row, &members) == nil && string(members["group"]) == "null" {
			continue
		}
		kept = append(kept, row)
	}
	response.Aggregates = kept
	seeded, err := json.Marshal(response)
	if err != nil {
		return body
	}
	return seeded
}

// A server that keyed its groups on object_id alone would return one group
// where three identities share one object identifier, with their rows summed
// together. Nothing about the answer looks wrong from the outside.
func seedGroupsFoldedByObjectID(body []byte) []byte {
	return reseedAggregates(body, func(rows []map[string]json.RawMessage) []map[string]json.RawMessage {
		folded := []map[string]json.RawMessage{}
		byObject := map[string]int{}
		for _, row := range rows {
			var identity p.Reference
			if json.Unmarshal(row["group"], &identity) != nil || identity.ObjectID == "" {
				folded = append(folded, row)
				continue
			}
			if at, seen := byObject[identity.ObjectID]; seen {
				folded[at]["row_count"] = addCounts(folded[at]["row_count"], row["row_count"])
				continue
			}
			byObject[identity.ObjectID] = len(folded)
			folded = append(folded, row)
		}
		return folded
	})
}

// A server answering the group as a bare object identifier rather than the
// qualified triple. A client that only ever references its own dataset would
// read it without noticing what it had stopped being told.
func seedGroupsWithoutQualification(body []byte) []byte {
	return reseedAggregates(body, func(rows []map[string]json.RawMessage) []map[string]json.RawMessage {
		for _, row := range rows {
			var identity p.Reference
			if json.Unmarshal(row["group"], &identity) == nil && identity.ObjectID != "" {
				row["group"], _ = json.Marshal(identity.ObjectID)
			}
		}
		return rows
	})
}

func addCounts(a, b json.RawMessage) json.RawMessage {
	var first, second int
	if json.Unmarshal(a, &first) != nil || json.Unmarshal(b, &second) != nil {
		return a
	}
	sum, _ := json.Marshal(first + second)
	return sum
}

func reseedAggregates(body []byte, change func([]map[string]json.RawMessage) []map[string]json.RawMessage) []byte {
	var response struct {
		Aggregates []map[string]json.RawMessage `json:"aggregates"`
		Watermark  string                       `json:"watermark"`
	}
	if json.Unmarshal(body, &response) != nil || len(response.Aggregates) == 0 {
		return body
	}
	response.Aggregates = change(response.Aggregates)
	seeded, err := json.Marshal(response)
	if err != nil {
		return body
	}
	return seeded
}

// The suite must be able to tell a bounded server from an unbounded one. A
// server that answers every query happily, whatever bound it exceeds, has to
// fail — otherwise the bound assertions are decoration.
func TestSuiteDetectsAnUnboundedServer(t *testing.T) {
	_, h, _ := fixture(t)
	unbounded := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := httptest.NewRecorder()
		h.ServeHTTP(recorder, r)
		body := recorder.Body.Bytes()
		code := recorder.Code
		// Pretend every refused query was acceptable after all.
		if strings.HasSuffix(r.URL.Path, ":query") && (code == 400 || code == 413 || code == 422) {
			code = 200
			body = []byte(`{"meta":{"id":"x","fields":[]},"records":[],"watermark":"w:seeded"}`)
		}
		for key, values := range recorder.Header() {
			w.Header()[key] = values
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(code)
		_, _ = w.Write(body)
	})
	s := httptest.NewServer(unbounded)
	defer s.Close()
	err := conformance.CheckCore(t.Context(), conformance.CoreFixture{URL: s.URL, Token: "owner", RefusedToken: "refused", Client: s.Client()})
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatal("the suite accepted a server that enforces no bounds", err)
	}
}

// The listing is the engine's answer, not the handler's. A caller with no grant
// gets an empty list rather than a refusal, so the response reveals nothing
// about which datasets the server holds.
func TestDatasetListingRevealsOnlyTheCallersOwn(t *testing.T) {
	_, h, base := fixture(t)
	dataset := strings.TrimPrefix(base, "/v1/datasets/")
	listed := func(token string) []any {
		t.Helper()
		r := httptest.NewRequest("GET", "/v1/datasets", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatal("listing refused", w.Code, w.Body.String())
		}
		var body struct {
			Datasets []any `json:"datasets"`
		}
		if json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Datasets == nil {
			t.Fatal("listing is not a list", w.Body.String())
		}
		return body.Datasets
	}
	held := listed("owner")
	if len(held) != 1 {
		t.Fatal("owner was not offered its dataset", held)
	}
	if id, _ := held[0].(map[string]any)["id"].(string); id != dataset {
		t.Fatal("listing named another dataset", held[0])
	}
	// "refused" authenticates and holds a write grant but no readable dataset
	// description; it is still a caller, and still sees a list.
	if stranger := listed("refused"); len(stranger) != 1 {
		t.Fatal("a granted caller lost its dataset", stranger)
	}
}

// An identity the server does not hold is absence, at the route as much as in
// the engine, and it is absence for every method the route accepts.
func TestUnknownDatasetRouteIsAbsence(t *testing.T) {
	_, h, _ := fixture(t)
	for _, method := range []string{"GET", "POST"} {
		r := httptest.NewRequest(method, "/v1/datasets/d-not-served/meta", strings.NewReader(`{}`))
		r.Header.Set("Authorization", "Bearer owner")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		// Absence, but not *proven* absence: not_found is reserved for a
		// retirement this server can evidence, and an identity it never held
		// has none behind it.
		if w.Code != 404 || !strings.Contains(w.Body.String(), "unavailable") {
			t.Fatal("unknown dataset claimed a retirement", method, w.Code, w.Body.String())
		}
	}
}

// sole names the dataset a store holds, through the composition call rather
// than the listing: a grant has to be installed on a dataset before any listing
// can report one.
func sole(t *testing.T, s interface {
	SoleDataset(context.Context) (store.Dataset, error)
}) string {
	t.Helper()
	dataset, err := s.SoleDataset(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return dataset.ID
}

// An engine that cannot make a dataset refuses the route however willing the
// host is. The promise is the engine's to keep, not the host's to make.
func TestDatasetCreationNeedsAnEngineThatCan(t *testing.T) {
	s, err := memory.New(p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := any(s).(store.Provisioning); ok {
		t.Fatal("the evaluator claims it can provision; this test no longer means anything")
	}
	h, err := server.NewWithOptions(s, func(context.Context, string) (store.Access, error) {
		return store.Access{Principal: "synthetic", Authorization: "owner"}, nil
	}, server.Options{AuthMethods: []string{"bearer_token"}, Provision: func(context.Context, store.Access) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/v1/datasets", strings.NewReader(`{"display_name":"Second quarter"}`))
	r.Header.Set("Authorization", "Bearer owner")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 || !strings.Contains(w.Body.String(), "not_supported") {
		t.Fatal("an engine that cannot provision answered", w.Code, w.Body.String())
	}
}

func TestCoreRejectsInvalidDiscoveryDisplayName(t *testing.T) {
	for _, raw := range []string{`""`, `null`, `17`, `true`, `{}`, `[]`} {
		t.Run(raw, func(t *testing.T) {
			_, h, _ := fixture(t)
			broken := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/.well-known/tessera" {
					h.ServeHTTP(w, r)
					return
				}
				recorded := httptest.NewRecorder()
				h.ServeHTTP(recorded, r)
				var document map[string]json.RawMessage
				if err := json.Unmarshal(recorded.Body.Bytes(), &document); err != nil {
					t.Fatal(err)
				}
				document["display_name"] = json.RawMessage(raw)
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(document); err != nil {
					t.Fatal(err)
				}
			})
			s := httptest.NewServer(broken)
			defer s.Close()
			err := conformance.CheckCore(t.Context(), conformance.CoreFixture{URL: s.URL, Token: "owner", RefusedToken: "refused", Client: s.Client()})
			if err == nil || !strings.Contains(err.Error(), "display_name must be a non-empty string") {
				t.Fatal("suite missed invalid display_name", err)
			}
		})
	}
}
