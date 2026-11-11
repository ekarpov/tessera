package trackstoragelab

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/server"
	"github.com/ekarpov/tessera/pkg/store"
	"github.com/ekarpov/tessera/pkg/store/sqlite"
)

// The application export is deliberately external to Tessera.
func TestTrackCatalogueOnReleasedRoutes(t *testing.T) {
	root := os.Getenv("TESSERA_TRACK_CATALOGUE")
	if root == "" {
		t.Skip("set TESSERA_TRACK_CATALOGUE to the app's exported catalog")
	}
	var manifest struct {
		RootTypeID string
		Documents  []struct {
			Path, SHA256, TypeID string
		}
	}
	read := func(name string) []byte {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	parse := func(raw []byte, target any) {
		t.Helper()
		if err := json.Unmarshal(raw, target); err != nil {
			t.Fatal(err)
		}
	}
	parse(read("manifest.json"), &manifest)
	byID := make(map[string]p.Meta, len(manifest.Documents))
	for _, file := range manifest.Documents {
		raw := read(file.Path)
		sum := sha256.Sum256(raw)
		if hex.EncodeToString(sum[:]) != file.SHA256 {
			t.Fatalf("digest mismatch: %s", file.Path)
		}
		var doc struct{ Installation struct{ Type p.Meta } }
		parse(raw, &doc)
		if doc.Installation.Type.ID != file.TypeID {
			t.Fatal("type ID mismatch")
		}
		byID[file.TypeID] = doc.Installation.Type
	}
	// Installation accepts declared fields with parents before children.
	var types []p.Meta
	emitted, visiting := map[string]bool{}, map[string]bool{}
	var emit func(string)
	emit = func(id string) {
		if emitted[id] {
			return
		}
		meta, exists := byID[id]
		if !exists || visiting[id] {
			t.Fatalf("invalid catalog parent graph at %q", id)
		}
		visiting[id] = true
		if meta.ParentID != nil {
			emit(*meta.ParentID)
		}
		types = append(types, meta)
		emitted[id] = true
	}
	for _, file := range manifest.Documents {
		emit(file.TypeID)
	}
	engine, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "lab.db"), p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	access := store.Access{Principal: "synthetic", Authorization: "lab"}
	handler, err := server.NewWithOptions(engine, func(context.Context, string) (store.Access, error) {
		return access, nil
	}, server.Options{AuthMethods: []string{"bearer_token"}, Provision: func(context.Context, store.Access) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path string, body any, status int, result any) {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequestWithContext(t.Context(), method, path, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer synthetic")
		req.Header.Set("Content-Type", "application/json")
		if path == "/v1/datasets" {
			req.Header.Set("Idempotency-Key", "78d72e62-bc84-4df1-8bb5-051fa7f66a7b")
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != status {
			t.Fatalf("%s %s: %d, want %d: %s", method, path, rec.Code, status, rec.Body.String())
		}
		if result != nil {
			parse(rec.Body.Bytes(), result)
		}
	}
	var created struct {
		DatasetID string `json:"dataset_id"`
	}
	request(http.MethodPost, "/v1/datasets", map[string]any{"display_name": "Track lab", "types": types}, 201, &created)

	base := "/v1/datasets/" + created.DatasetID
	const workout = "com.voptrack.track.workout_session"
	const custom = "lab.climbing_workout"
	if _, ok := byID[workout]; !ok {
		t.Fatal("missing workout")
	}
	if os.Getenv("TESSERA_LAB_MANAGED_FIELDS") == "1" {
		typed, ok := any(engine).(interface {
			InstallManagedTypes(context.Context, store.Access, string, []string) error
		})
		if !ok {
			t.Fatal("this engine cannot provision managed tables")
		}
		if err := typed.InstallManagedTypes(t.Context(), access, created.DatasetID, []string{manifest.RootTypeID, workout}); err != nil {
			t.Fatal(err)
		}
	}
	var tx p.TransactionResult
	request(http.MethodPost, base+":transact", json.RawMessage(`{"operations":[
	  {"op":"define_type","meta":{"id":"lab.climbing_workout","parent_id":"com.voptrack.track.workout_session",
	    "fields":[{"id":"grade","meta_id":"lab.climbing_workout","type":"text"}]}},
	  {"op":"insert","type":"lab.climbing_workout","fields":{"timestamp":{"type":"date_time","value":"2026-01-02T10:00:00.123Z"},
	    "effort":{"type":"integer","value":7},"grade":{"type":"text","value":"V4"}}},
	  {"op":"insert","type":"com.voptrack.track.workout_session","fields":{"timestamp":{"type":"date_time","value":"2026-01-01T10:00:00.000Z"}}}
	]}`), 200, &tx)
	var newest p.RecordSet
	request(http.MethodPost, base+"/objects/"+workout+":query", json.RawMessage(`{
	  "query":{"type_scope":{"kind":"descendants"},"fields":["timestamp","effort"],
	    "result_sort":[{"field_id":"timestamp","ascending":false}]},"limit":10}`), 200, &newest)
	if len(newest.Records) != 2 || newest.Records[0].MetaID != custom || newest.Records[0].Values[1].Payload() != int64(7) {
		t.Fatalf("unexpected workout result: %+v", newest.Records)
	}
	var readBack p.RecordSet
	request(http.MethodGet, base+"/objects/"+custom+"/"+newest.Records[0].ID.ID+"?fields=grade", nil, 200, &readBack)
	if len(readBack.Records) != 1 || readBack.Records[0].Values[0].Payload() != "V4" {
		t.Fatalf("unexpected custom field result: %+v", readBack.Records)
	}
}
