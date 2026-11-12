package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/server"
	"github.com/ekarpov/tessera/pkg/store"
	"github.com/ekarpov/tessera/pkg/store/memory"
)

func tesseraFixture(t *testing.T) (*memory.Store, http.Handler, string) {
	t.Helper()
	s, err := memory.New(p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	a := store.Access{Principal: "person", Authorization: "owner"}
	if err = s.SetAuthorization(t.Context(), sole(t, s), a, store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true}, View: store.FullView()}); err != nil {
		t.Fatal(err)
	}
	h, err := server.New(s, func(_ context.Context, token string) (store.Access, error) {
		return store.Access{Principal: "person", Authorization: token}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	held, err := s.Datasets(t.Context(), a)
	if err != nil || len(held) != 1 {
		t.Fatal("expected exactly one dataset", err)
	}
	return s, h, "/v1/datasets/" + held[0].ID
}
func wireCall(t testing.TB, h http.Handler, method, path, token, body string, status int) []byte {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != status {
		t.Fatalf("%s expected %d got %d: %s", method, status, w.Code, w.Body.String())
	}
	return w.Body.Bytes()
}
func decode[T any](t *testing.T, b []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}
func readObject(t *testing.T, h http.Handler, base, typ, id string) p.RecordSet {
	return decode[p.RecordSet](t, wireCall(t, h, "GET", base+"/objects/"+typ+"/"+id, "owner", "", 200))
}

func TestTesseraAnnotationsAliasesAndVersions(t *testing.T) {
	_, h, base := tesseraFixture(t)
	const opaque = `{ "z": 1.00, "a": [true, false], "escaped": "\u0061" }`
	body := `{"operations":[
 {"op":"define_type","meta":{"id":"test.parent","roles":["unknown.role"],"annotations":{"opaque":` + opaque + `},"fields":[{"id":"amount","meta_id":"test.parent","name":"Amount","type":"integer","annotations":{"bytes":` + opaque + `}}]}},
 {"op":"define_type","meta":{"id":"test.child","parent_id":"test.parent","roles":["unknown.role","child"],"annotations":{"own":1e3},"fields":[{"id":"link","meta_id":"test.child","type":"reference"},{"id":"hidden","meta_id":"test.child","type":"text"}]}},
 {"op":"insert","type":"test.parent","fields":{"amount":{"type":"integer","value":1}},"as":"parent"},
 {"op":"insert","type":"test.child","fields":{"link":{"type":"reference","value":"$parent"},"hidden":{"type":"text","value":"whole row"}},"as":"child"},
 {"op":"update","target":{"type":"test.child","id":"$child"},"fields":{"amount":{"type":"integer","value":2}}}]}`
	result := decode[p.TransactionResult](t, wireCall(t, h, "POST", base+":transact", "owner", body, 200))
	if len(result.Results) != 5 || result.Watermark == "" {
		t.Fatal("transaction envelope")
	}
	id := result.Results[3].Objects[0].ID
	row := readObject(t, h, base, "test.child", id)
	if row.Watermark != result.Watermark || row.Records[0].Version == "" || row.Meta.SchemaVersion == "" {
		t.Fatal("missing or torn versions")
	}
	if string(row.Meta.Annotations["opaque"]) != opaque || string(row.Meta.Fields[0].Annotations["bytes"]) != opaque || len(row.Meta.Roles) != 2 {
		t.Fatal("opaque metadata rewritten or role union incorrect")
	}
	ref := row.Records[0].Values[1].Payload().(p.Reference)
	if ref.DatasetID != strings.TrimPrefix(base, "/v1/datasets/") || ref.ObjectID != result.Results[2].Objects[0].ID || ref.MetaID != "test.parent" {
		t.Fatal("alias reference is not qualified")
	}
	version := row.Records[0].Version
	schema := row.Meta.SchemaVersion
	projected := decode[p.RecordSet](t, wireCall(t, h, "GET", base+"/objects/test.child/"+id+"?fields=amount", "owner", "", 200))
	if projected.Records[0].Version != version {
		t.Fatal("projection changed version")
	}
	query := decode[p.RecordSet](t, wireCall(t, h, "POST", base+"/objects/test.parent:query", "owner", `{"query":{"type_scope":{"kind":"descendants"},"fields":[],"include_versions":true}}`, 200))
	if len(query.Records) != 2 || len(query.Meta.Fields) != 0 {
		t.Fatal("ancestor projection")
	}
	for _, r := range query.Records {
		if r.ID.ID == id && r.Version != version {
			t.Fatal("ancestor version omits child fields")
		}
	}
	wireCall(t, h, "PUT", base+"/meta/test.parent", "owner", `{"type":{"id":"test.parent","roles":["changed"],"annotations":{"opaque":`+opaque+`},"fields":[{"id":"amount","meta_id":"test.parent","name":"New name","type":"integer"}]}}`, 200)
	after := readObject(t, h, base, "test.child", id)
	if after.Watermark == row.Watermark || after.Meta.SchemaVersion == schema || after.Records[0].Version != version {
		t.Fatal("schema change affected wrong version")
	}
	failed := wireCall(t, h, "POST", base+":transact", "owner", `{"operations":[{"op":"expect_type","type":"test.child","schema_version":"`+schema+`"}]}`, 409)
	var envelope struct {
		Error p.Error `json:"error"`
	}
	_ = json.Unmarshal(failed, &envelope)
	if envelope.Error.OperationIndex == nil || *envelope.Error.OperationIndex != 0 {
		t.Fatal("missing first operation index")
	}
	wireCall(t, h, "POST", base+":transact", "owner", `{"operations":[{"op":"expect_type","type":"test.child","schema_version":"`+after.Meta.SchemaVersion+`"},{"op":"expect","target":{"type":"test.child","id":"`+id+`"},"version":"`+version+`"},{"op":"update","target":{"type":"test.child","id":"`+id+`"},"fields":{"hidden":{"type":"text","value":"new"}}}]}`, 200)
	changed := readObject(t, h, base, "test.child", id)
	if changed.Records[0].Version == version {
		t.Fatal("hidden field did not change version")
	}
	noVersions := wireCall(t, h, "POST", base+"/objects/test.child:query", "owner", `{"query":{"fields":[]}}`, 200)
	var no p.RecordSet
	_ = json.Unmarshal(noVersions, &no)
	if no.Records[0].Version != "" {
		t.Fatal("unsolicited query versions")
	}
}

func TestTesseraRollbackAbsenceAndCascade(t *testing.T) {
	_, h, base := tesseraFixture(t)
	wireCall(t, h, "POST", base+"/meta", "owner", `{"type":{"id":"test.kind","fields":[{"id":"n","meta_id":"test.kind","type":"integer"}]}}`, 201)
	filter := `{"field":{"id":"n","exact":{"type":"integer","value":7}}}`
	operations := `{"operations":[{"op":"expect_absent","type":"test.kind","filter":` + filter + `,"guards":["entry"]},{"op":"insert","type":"test.kind","fields":{"n":{"type":"integer","value":7}},"as":"entry"},{"op":"insert","type":"test.kind","fields":{"n":{"type":"integer","value":8}}}]}`
	wireCall(t, h, "POST", base+":transact", "owner", operations, 200)
	wireCall(t, h, "POST", base+":transact", "owner", operations, 409)
	before := wireCall(t, h, "POST", base+"/objects/test.kind:count", "owner", `{}`, 200)
	wireCall(t, h, "POST", base+":transact", "owner", `{"operations":[{"op":"delete_where","type":"test.kind"},{"op":"expect_absent","type":"test.kind","filter":`+filter+`,"guards":["bad"]},{"op":"insert","type":"test.kind","fields":{"n":{"type":"integer","value":9}},"as":"bad"}]}`, 400)
	after := wireCall(t, h, "POST", base+"/objects/test.kind:count", "owner", `{}`, 200)
	if string(before) != string(after) {
		t.Fatal("late guard failure committed deletions or watermark")
	}
	wireCall(t, h, "POST", base+":transact", "owner", `{"operations":[{"op":"delete_type","type":"test.kind"}]}`, 409)
	wireCall(t, h, "POST", base+"/meta", "owner", `{"type":{"id":"test.child","parent_id":"test.kind","fields":[]}}`, 201)
	wireCall(t, h, "POST", base+"/objects/test.child", "owner", `{"fields":{}}`, 201)
	wireCall(t, h, "POST", base+":transact", "owner", `{"operations":[{"op":"update_where","type":"test.kind","fields":{"n":{"type":"integer","value":10}}}]}`, 200)
	count := decode[map[string]any](t, wireCall(t, h, "POST", base+"/objects/test.kind:count", "owner", `{"filter":{"field":{"id":"n","exact":{"type":"integer","value":10}}}}`, 200))
	if count["count"] != float64(3) {
		t.Fatal("filtered update omitted descendant")
	}
	deleted := decode[map[string]any](t, wireCall(t, h, "DELETE", base+"/meta/test.kind?confirm=test.kind", "owner", "", 200))
	if deleted["deleted_objects"] != float64(3) {
		t.Fatal("cascade omitted descendants")
	}
	wireCall(t, h, "GET", base+"/meta/test.child", "owner", "", 404)
}

func TestTesseraGrammarAndGrantUnion(t *testing.T) {
	s, h, base := tesseraFixture(t)
	if err := s.SetAuthorization(t.Context(), sole(t, s), store.Access{Principal: "person", Authorization: "schema"}, store.Authorization{Grants: store.Grants{Schema: true}, View: store.FullView()}); err != nil {
		t.Fatal(err)
	}
	wireCall(t, h, "POST", base+":transact", "schema", `{"operations":[{"op":"define_type","meta":{"id":"test.schema","fields":[]}}]}`, 200)
	wireCall(t, h, "POST", base+":transact", "schema", `{"operations":[{"op":"define_type","meta":{"id":"test.rollback","fields":[]}},{"op":"insert","type":"test.rollback","fields":{}}]}`, 403)
	wireCall(t, h, "GET", base+"/meta/test.rollback", "owner", "", 404)
	for _, body := range []string{
		`{"operations":[]}`,
		`{"operations":[{"op":"delete","target":{"type":"test.schema","id":"$later"}},{"op":"insert","type":"test.schema","fields":{},"as":"later"}]}`,
		`{"operations":[{"op":"insert","type":"test.schema","fields":{},"as":"same"},{"op":"insert","type":"test.schema","fields":{},"as":"same"}]}`,
		`{"operations":[{"op":"expect_absent","type":"test.schema","filter":{"and":[]}}]}`,
		`{"operations":[{"op":"delete","target":{"type":"test.schema","id":"1"},"fields":{}}]}`,
		`{"operations":[{"op":"insert","type":"test.schema","fields":{},"absence_fields":{}}]}`,
	} {
		wireCall(t, h, "POST", base+":transact", "owner", body, 400)
	}
	poll := decode[struct {
		Datasets []struct {
			Watermark     string `json:"watermark"`
			SchemaVersion string `json:"schema_version"`
			Access        struct {
				ExpiresAt *string `json:"expires_at"`
			} `json:"access"`
		} `json:"datasets"`
	}](t, wireCall(t, h, "GET", "/v1/datasets", "owner", "", 200))
	if len(poll.Datasets) != 1 || poll.Datasets[0].Watermark == "" || poll.Datasets[0].SchemaVersion == "" {
		t.Fatal("cheap poll unavailable")
	}
	expiry := time.Now().Add(-time.Second)
	if err := s.SetAuthorization(t.Context(), sole(t, s), store.Access{Principal: "person", Authorization: "expired"}, store.Authorization{Grants: store.Grants{Read: true, ExpiresAt: &expiry}, View: store.FullView()}); err != nil {
		t.Fatal(err)
	}
	// The listing now also carries server_access, so this asserts the datasets
	// member rather than the whole body.
	var expiredListing struct {
		Datasets []json.RawMessage `json:"datasets"`
	}
	if json.Unmarshal(wireCall(t, h, "GET", "/v1/datasets", "expired", "", 200), &expiredListing) != nil || len(expiredListing.Datasets) != 0 {
		t.Fatal("expired access remains listed")
	}
	// An expired grant is no grant, so the dataset is concealed rather than
	// refused: 404 unavailable, not 403.
	wireCall(t, h, "GET", base+"/meta", "expired", "", 404)
	// Routes this server used to have. A withdrawn route must answer 404
	// rather than resolve to whatever replaced it, and the well-known path is
	// the one a client probes before it knows anything else. The former
	// spelling was a client's own; per AGENTS.md that vocabulary lives with
	// the client, so a neutral withdrawn path stands for it here.
	for _, route := range []string{"/.well-known/tessera-storage", "/v1/storages"} {
		wireCall(t, h, "GET", route, "owner", "", 404)
	}
}

func TestTesseraSchemaValidationAndFieldRemoval(t *testing.T) {
	_, h, base := tesseraFixture(t)
	wireCall(t, h, "POST", base+"/meta", "owner", `{"type":{"id":"test.root","roles":["anything"],"annotations":{"reserved":null},"fields":[{"id":"n","meta_id":"test.root","type":"integer"}]}}`, 201)
	for _, body := range []string{
		`{"type":{"id":"test.bad","roles":["a","a"],"fields":[]}}`,
		`{"type":{"id":"test.bad","roles":["no spaces"],"fields":[]}}`,
		`{"type":{"id":"test.bad","parent_id":"test.root","annotations":{"reserved":7},"fields":[]}}`,
		`{"type":{"id":"test.bad","annotations":[],"fields":[]}}`,
		`{"type":{"id":"test.bad","annotations":{"nested":{"a":1,"a":2}},"fields":[]}}`,
	} {
		wireCall(t, h, "POST", base+"/meta", "owner", body, 400)
	}
	wireCall(t, h, "POST", base+"/meta", "owner", `{"type":{"id":"test.leaf","parent_id":"test.root","fields":[]}}`, 201)
	insert := decode[struct {
		Objects []p.Identity `json:"objects"`
	}](t, wireCall(t, h, "POST", base+"/objects/test.leaf", "owner", `{"fields":{"n":{"type":"integer","value":1}}}`, 201))
	id := insert.Objects[0].ID
	before := readObject(t, h, base, "test.leaf", id)
	wireCall(t, h, "PUT", base+"/meta/test.root", "owner", `{"type":{"id":"test.root","fields":[]}}`, 400)
	wireCall(t, h, "PUT", base+"/meta/test.root", "owner", `{"type":{"id":"test.root","fields":[{"id":"n","meta_id":"test.root","type":"text"}]}}`, 400)
	after := readObject(t, h, base, "test.leaf", id)
	if before.Watermark != after.Watermark {
		t.Fatal("failed schema update moved watermark")
	}
	wireCall(t, h, "POST", base+":transact", "owner", `{"operations":[{"op":"update_type","meta":{"id":"test.root","fields":[]},"confirm":"test.root"}]}`, 200)
	after = readObject(t, h, base, "test.leaf", id)
	if len(after.Records[0].Values) != 0 || after.Records[0].Version == before.Records[0].Version {
		t.Fatal("field removal did not remove descendant values")
	}
}

func TestServiceCredentialDiscoveryAndIsolation(t *testing.T) {
	s, _, _ := tesseraFixture(t)
	if err := s.SetAuthorization(t.Context(), sole(t, s), store.Access{Principal: "service", Authorization: "job"}, store.Authorization{Grants: store.Grants{Read: true}, View: store.FullView()}); err != nil {
		t.Fatal(err)
	}
	h, err := server.NewWithOptions(s, func(_ context.Context, token string) (store.Access, error) {
		return store.Access{Principal: "service", Authorization: token}, nil
	}, server.Options{AuthMethods: []string{"client_credentials"}, AuthorizationServers: []string{"https://auth.example.test"}})
	if err != nil {
		t.Fatal(err)
	}
	discovery := wireCall(t, h, "GET", "/.well-known/tessera", "job", "", 200)
	if !strings.Contains(string(discovery), `"client_credentials"`) {
		t.Fatal("service auth method missing")
	}
	// An authorization name belonging to a different principal gives no access.
	var isolated struct {
		Datasets []json.RawMessage `json:"datasets"`
	}
	if json.Unmarshal(wireCall(t, h, "GET", "/v1/datasets", "owner", "", 200), &isolated) != nil || len(isolated.Datasets) != 0 {
		t.Fatal("service inherited another principal's access")
	}
}

func TestReferenceAliasesInPredicatesAndOpaqueJSON(t *testing.T) {
	_, h, base := tesseraFixture(t)
	wireCall(t, h, "POST", base+"/meta", "owner", `{"type":{"id":"test.refs","fields":[{"id":"ref","meta_id":"test.refs","type":"reference"},{"id":"payload","meta_id":"test.refs","type":"json"}]}}`, 201)
	response := decode[p.TransactionResult](t, wireCall(t, h, "POST", base+":transact", "owner", `{"operations":[
 {"op":"insert","type":"test.refs","fields":{},"as":"parent"},
 {"op":"expect_absent","type":"test.refs","filter":{"field":{"id":"ref","exact":{"type":"reference","value":"$parent"}}},"guards":["child"]},
 {"op":"insert","type":"test.refs","fields":{"ref":{"type":"reference","value":"$parent"},"payload":{"type":"json","value":"\"$not_an_alias\""}},"as":"child"},
 {"op":"delete_where","type":"test.refs","filter":{"field":{"id":"ref","exact":{"type":"reference","value":"$parent"}}}}]}`, 200))
	if response.Results[3].Affected == nil || *response.Results[3].Affected != 1 {
		t.Fatal("predicate alias did not resolve")
	}
	wireCall(t, h, "POST", base+":transact", "owner", `{"operations":[{"op":"delete_where","type":"test.refs","filter":{"field":{"id":"ref","exact":{"type":"reference","value":"$missing"}}}}]}`, 400)
}

func TestConvenienceLimitUsesReceivedBytes(t *testing.T) {
	_, h, base := fixture(t)
	wireCall(t, h, "POST", base+"/meta", "owner", `{"type":{"id":"test.limit","fields":[{"id":"note","meta_id":"test.limit","type":"text"}]}}`, 201)
	prefix := `{"fields":{"note":{"type":"text","value":"`
	suffix := `"}}}`
	body := prefix + strings.Repeat("x", 1024-len(prefix)-len(suffix)) + suffix
	wireCall(t, h, "POST", base+"/objects/test.limit", "owner", body, 201)
	wireCall(t, h, "POST", base+"/objects/test.limit", "owner", body+" ", 413)
}
