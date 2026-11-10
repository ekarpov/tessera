package server_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekarpov/tessera/internal/dbtest"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/server"
	"github.com/ekarpov/tessera/pkg/store"
	"github.com/ekarpov/tessera/pkg/store/postgres"
	"github.com/ekarpov/tessera/pkg/store/sqlite"
)

type profileEngine interface {
	store.Store
	store.Provisioning
	EnableReads(...string) error
	EnableTypedFields(context.Context, store.Access, string) error
	SetAuthorization(context.Context, string, store.Access, store.Authorization) error
	Close() error
}

// The same wire requests run against each engine. Only dataset provisioning
// and grants use host methods. The scenarios use neither a catalog API nor SQL.
func TestManagedStorageCompatibilityV1(t *testing.T) {
	engines := []struct {
		name string
		open func(*testing.T) profileEngine
	}{
		{"sqlite", func(t *testing.T) profileEngine {
			engine, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "profiles.db"), p.DevelopmentLimits())
			if err != nil {
				t.Fatal(err)
			}
			return engine
		}},
		{"postgres", openProfilePostgres},
	}
	for _, backend := range engines {
		t.Run(backend.name, func(t *testing.T) {
			engine := backend.open(t)
			t.Cleanup(func() {
				if err := engine.Close(); err != nil {
					t.Error(err)
				}
			})
			if err := engine.EnableReads(p.OptionalReadCapabilities...); err != nil {
				t.Fatal(err)
			}
			handler, err := server.New(engine, func(_ context.Context, token string) (store.Access, error) {
				return profileAccess(token), nil
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, mode := range []string{"json", "typed"} {
				t.Run(mode, func(t *testing.T) {
					for _, scenario := range []struct {
						name string
						run  func(*testing.T, profileWire)
					}{
						{"inherited_sort", checkInheritedSort},
						{"exact_aggregate", checkExactAggregate},
						{"reference_pagination", checkReferencePagination},
						{"scoped_access", checkScopedAccess},
					} {
						t.Run(scenario.name, func(t *testing.T) {
							made, err := engine.CreateDataset(t.Context(), profileAccess("owner"), scenario.name, p.Limits{}, nil, "", [32]byte{})
							if err != nil {
								t.Fatal(err)
							}
							if mode == "typed" {
								if err = engine.EnableTypedFields(t.Context(), profileAccess("owner"), made.DatasetID); err != nil {
									t.Fatal(err)
								}
							}
							scenario.run(t, profileWire{engine, handler, made.DatasetID})
						})
					}
				})
			}

		})
	}
}

func openProfilePostgres(t *testing.T) profileEngine {
	t.Helper()
	engine, _, _ := openProfilePostgresSchema(t)
	return engine
}

func openProfilePostgresSchema(t *testing.T) (profileEngine, *sql.DB, string) {
	t.Helper()
	dsn := dbtest.Postgres(t)
	admin, err := sql.Open("tessera-postgres", dsn)
	if err != nil {
		t.Fatal("open disposable PostgreSQL", err)
	}
	t.Cleanup(func() { admin.Close() })
	var suffix [8]byte
	rand.Read(suffix[:])
	schema := fmt.Sprintf("tessera_profiles_%x", suffix)
	if _, err = admin.ExecContext(t.Context(), `CREATE SCHEMA "`+schema+`"`); err != nil {
		t.Fatal("create isolated profile schema", err)
	}
	t.Cleanup(func() {
		if _, err := admin.ExecContext(context.Background(), `DROP SCHEMA "`+schema+`" CASCADE`); err != nil {
			t.Error(err)
		}
	})
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		parsed, err := url.Parse(dsn)
		if err != nil {
			t.Fatal("invalid disposable PostgreSQL URL")
		}
		values := parsed.Query()
		values.Set("search_path", schema)
		parsed.RawQuery = values.Encode()
		dsn = parsed.String()
	} else {
		dsn += " search_path=" + schema
	}
	engine, err := postgres.Open(t.Context(), dsn, filepath.Join(t.TempDir(), "retired"), p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	return engine, admin, schema
}

func profileAccess(token string) store.Access {
	return store.Access{Principal: token, Authorization: token}
}

type profileWire struct {
	engine  profileEngine
	handler http.Handler
	dataset string
}

func (f profileWire) call(t *testing.T, method, path, token, body string, status int) []byte {
	t.Helper()
	return wireCall(t, f.handler, method, "/v1/datasets/"+f.dataset+path, token, body, status)
}

func (f profileWire) grant(t *testing.T, token string, grants store.Grants, view store.View) {
	t.Helper()
	if err := f.engine.SetAuthorization(t.Context(), f.dataset, profileAccess(token), store.Authorization{Grants: grants, View: view}); err != nil {
		t.Fatal(err)
	}
}

func (f profileWire) transact(t *testing.T, body string) p.TransactionResult {
	t.Helper()
	return decode[p.TransactionResult](t, f.call(t, "POST", ":transact", "owner", body, 200))
}

func (f profileWire) refusal(t *testing.T, method, path, token, body string, status int, code string) {
	t.Helper()
	envelope := decode[struct{ Error p.Error }](t, f.call(t, method, path, token, body, status))
	if envelope.Error.Code != code {
		t.Fatalf("error code %q, want %q", envelope.Error.Code, code)
	}
}

type profilePage struct {
	Records    []p.Record
	NextCursor string `json:"next_cursor"`
}

func (f profileWire) firstPage(t *testing.T, path, token, body string) profilePage {
	t.Helper()
	cursor := decode[store.PageStart](t, f.call(t, "POST", path+":page", token, body, 200)).Cursor
	if cursor == "" {
		t.Fatal("missing first cursor")
	}
	return decode[profilePage](t, f.call(t, "POST", "/pages/"+cursor+":next", token, "", 200))
}

func checkInheritedSort(t *testing.T, f profileWire) {
	result := f.transact(t, `{"operations":[
	  {"op":"define_type","meta":{"id":"profile.root","fields":[{"id":"stamp","meta_id":"profile.root","type":"date_time"}]}},
	  {"op":"define_type","meta":{"id":"profile.base","parent_id":"profile.root","fields":[{"id":"score","meta_id":"profile.base","type":"integer"}]}},
	  {"op":"define_type","meta":{"id":"profile.extension","parent_id":"profile.base","fields":[{"id":"label","meta_id":"profile.extension","type":"text"}]}},
	  {"op":"insert","type":"profile.base","fields":{"stamp":{"type":"date_time","value":"2026-01-01T10:00:00.001Z"}}},
	  {"op":"insert","type":"profile.extension","fields":{"stamp":{"type":"date_time","value":"2026-01-01T10:00:00.002Z"},"score":{"type":"integer","value":7},"label":{"type":"text","value":"extension value"}}}
	]}`)
	baseRecord, extensionRecord := result.Results[3].Objects[0].ID, result.Results[4].Objects[0].ID
	query := `{"query":{"type_scope":{"kind":"%s"},"fields":["stamp","score"],"result_sort":[{"field_id":"stamp","ascending":false}]},"limit":10}`
	for _, tc := range []struct {
		scope string
		first string
		count int
	}{{"exact", baseRecord, 1}, {"descendants", extensionRecord, 2}} {
		set := decode[p.RecordSet](t, f.call(t, "POST", "/objects/profile.base:query", "owner", fmt.Sprintf(query, tc.scope), 200))
		if len(set.Records) != tc.count || set.Records[0].ID.ID != tc.first {
			t.Fatalf("%s: unexpected baseRecord result", tc.scope)
		}
	}
	set := decode[p.RecordSet](t, f.call(t, "GET", "/objects/profile.extension/"+extensionRecord+"?fields=label", "owner", "", 200))
	if len(set.Records) != 1 || set.Records[0].Values[0].Payload() != "extension value" {
		t.Fatal("custom field did not survive inherited record read")
	}
	filtered := decode[p.RecordSet](t, f.call(t, "POST", "/objects/profile.base:query", "owner",
		`{"query":{"type_scope":{"kind":"descendants"},"filter":{"and":[
          {"field":{"id":"score","range":{"lower":{"type":"integer","value":6}}}},
          {"field":{"id":"score","exact":{"type":"integer","value":7}}}
        ]}},"limit":10}`, 200))
	if len(filtered.Records) != 1 || filtered.Records[0].ID.ID != extensionRecord {
		t.Fatal("filter lost a matching descendant or included a missing value")
	}
	f.call(t, "PATCH", "/objects/profile.extension/"+extensionRecord, "owner",
		`{"fields":{"score":{"type":"null"}}}`, 200)
	filtered = decode[p.RecordSet](t, f.call(t, "POST", "/objects/profile.base:query", "owner",
		`{"query":{"type_scope":{"kind":"descendants"},"filter":{"field":{"id":"score","exact":{"type":"integer","value":7}}}},"limit":10}`, 200))
	if len(filtered.Records) != 0 {
		t.Fatal("cleared inherited field remained visible to the filter")
	}
}

func checkExactAggregate(t *testing.T, f profileWire) {
	f.transact(t, `{"operations":[
	  {"op":"define_type","meta":{"id":"profile.value","fields":[{"id":"bucket","meta_id":"profile.value","type":"text"},{"id":"amount","meta_id":"profile.value","type":"fixed","scale":2}]}},
	  {"op":"insert","type":"profile.value","fields":{"bucket":{"type":"text","value":"alpha"},"amount":{"type":"fixed","value":"9007199254740993.01"}}},
	  {"op":"insert","type":"profile.value","fields":{"bucket":{"type":"text","value":"alpha"},"amount":{"type":"fixed","value":"0.02"}}},
	  {"op":"insert","type":"profile.value","fields":{"bucket":{"type":"text","value":"beta"},"amount":{"type":"fixed","value":"-9007199254740993.03"}}}
	]}`)
	f.refusal(t, "POST", ":transact", "owner", `{"operations":[
	  {"op":"insert","type":"profile.value","fields":{"bucket":{"type":"text","value":"alpha"},"amount":{"type":"fixed","value":"1"}}},
	  {"op":"insert","type":"profile.value","fields":{"unknown":{"type":"text","value":"bad"}}}
	]}`, 400, "invalid_field")
	f.grant(t, "reader", store.Grants{Read: true}, store.SharedView([]string{"profile.value"}))
	result := decode[struct {
		Aggregates []struct {
			Group    *string         `json:"group"`
			RowCount int             `json:"row_count"`
			Sum      json.RawMessage `json:"sum"`
		} `json:"aggregates"`
	}](t, f.call(t, "POST", "/objects/profile.value:aggregate", "reader",
		`{"field":"amount","group_by":"bucket"}`, 200))
	if len(result.Aggregates) != 2 {
		t.Fatalf("group count = %d", len(result.Aggregates))
	}
	for i, want := range []struct {
		group, sum string
		count      int
	}{{"alpha", "9007199254740993.03", 2}, {"beta", "-9007199254740993.03", 1}} {
		row := result.Aggregates[i]
		if row.Group == nil || *row.Group != want.group || string(row.Sum) != fmt.Sprintf("%q", want.sum) || row.RowCount != want.count {
			t.Fatalf("group %d lost exact values or transaction atomicity: %+v", i, row)
		}
	}
}

func checkReferencePagination(t *testing.T, f profileWire) {
	result := f.transact(t, `{"operations":[
	  {"op":"define_type","meta":{"id":"profile.parent","fields":[{"id":"body","meta_id":"profile.parent","type":"text"}]}},
	  {"op":"define_type","meta":{"id":"profile.dependent","fields":[{"id":"post","meta_id":"profile.dependent","type":"reference"},{"id":"body","meta_id":"profile.dependent","type":"text"},{"id":"position","meta_id":"profile.dependent","type":"integer"}]}},
	  {"op":"insert","type":"profile.parent","as":"thread","fields":{"body":{"type":"text","value":"original"}}},
	  {"op":"insert","type":"profile.parent","as":"other","fields":{"body":{"type":"text","value":"other parent"}}},
	  {"op":"insert","type":"profile.dependent","fields":{"post":{"type":"reference","value":"$thread"},"body":{"type":"text","value":"first"},"position":{"type":"integer","value":1}}},
	  {"op":"insert","type":"profile.dependent","fields":{"post":{"type":"reference","value":"$thread"},"body":{"type":"text","value":"second"},"position":{"type":"integer","value":2}}},
	  {"op":"insert","type":"profile.dependent","fields":{"post":{"type":"reference","value":"$other"},"position":{"type":"integer","value":0}}}
	]}`)
	thread := result.Results[2].Objects[0].ID
	f.grant(t, "member", store.Grants{Read: true}, store.FullView())
	f.refusal(t, "POST", "/meta", "member", `{"type":{"id":"profile.member_type","fields":[]}}`, 403, "permission_denied")
	f.call(t, "PATCH", "/objects/profile.parent/"+thread, "owner", `{"fields":{"body":{"type":"text","value":"Removed"}}}`, 200)
	post := decode[p.RecordSet](t, f.call(t, "GET", "/objects/profile.parent/"+thread+"?fields=body", "member", "", 200))
	if len(post.Records) != 1 || post.Records[0].ID.ID != thread || post.Records[0].Values[0].Payload() != "Removed" {
		t.Fatal("placeholder did not preserve the parent")
	}
	body := fmt.Sprintf(`{"query":{"fields":["post","body"],"filter":{"field":{"id":"post","exact":{"type":"reference","value":{"dataset_id":%q,"meta_id":"profile.parent","object_id":%q}}}},"result_sort":[{"field_id":"position","ascending":true}]},"page_size":1}`, f.dataset, thread)
	page := f.firstPage(t, "/objects/profile.dependent", "member", body)
	if len(page.Records) != 1 || page.Records[0].ID.ID != result.Results[4].Objects[0].ID || page.NextCursor == "" {
		t.Fatal("wrong first dependent")
	}
	page = decode[profilePage](t, f.call(t, "POST", "/pages/"+page.NextCursor+":next", "member", "", 200))
	if len(page.Records) != 1 || page.Records[0].ID.ID != result.Results[5].Objects[0].ID || page.NextCursor != "" {
		t.Fatal("wrong second dependent or cross-parent record")
	}
}

func checkScopedAccess(t *testing.T, f profileWire) {
	result := f.transact(t, `{"operations":[
	  {"op":"define_type","meta":{"id":"profile.measurement","fields":[{"id":"amount","meta_id":"profile.measurement","type":"integer"}]}},
	  {"op":"define_type","meta":{"id":"profile.private_note","parent_id":"profile.measurement","fields":[]}},
	  {"op":"insert","type":"profile.measurement","fields":{"amount":{"type":"integer","value":1}}},
	  {"op":"insert","type":"profile.private_note","fields":{"amount":{"type":"integer","value":99}}}
	]}`)
	f.grant(t, "contributor", store.Grants{Insert: true}, store.SharedView(nil).WithInserts([]string{"profile.measurement"}))
	f.call(t, "POST", "/objects/profile.measurement", "contributor", `{"fields":{"amount":{"type":"integer","value":2}}}`, 201)
	f.refusal(t, "POST", "/objects/profile.private_note", "contributor", `{"fields":{}}`, 403, "permission_denied")
	f.refusal(t, "GET", "/objects/profile.measurement/"+result.Results[2].Objects[0].ID, "contributor", "", 403, "permission_denied")
	expiry := time.Now().Add(2 * time.Second)
	f.grant(t, "guest", store.Grants{Read: true, ExpiresAt: &expiry}, store.SharedView([]string{"profile.measurement"}))
	f.refusal(t, "GET", "/meta/profile.private_note", "guest", "", 404, "unavailable")
	page := f.firstPage(t, "/objects/profile.measurement", "guest",
		`{"query":{"type_scope":{"kind":"descendants"},"result_sort":[{"field_id":"amount","ascending":false}]},"page_size":1}`)
	if len(page.Records) != 1 || page.Records[0].MetaID != "profile.measurement" || page.Records[0].Values[0].Payload() != int64(2) || page.NextCursor == "" {
		t.Fatal("authorized first page included a private child or missed a contributed entry")
	}
	time.Sleep(time.Until(expiry) + time.Millisecond)
	f.refusal(t, "POST", "/pages/"+page.NextCursor+":next", "guest", "", 410, "view_changed")
}
