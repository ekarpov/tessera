package devserver

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/ekarpov/tessera/pkg/conformance"
	p "github.com/ekarpov/tessera/pkg/protocol"
)

// This is a backend HTTP gate for the Swift handoff, not an independent Swift pass.
func TestClientIntegration(t *testing.T) {
	dir := t.TempDir()
	var runtime *Runtime
	var srv *httptest.Server
	var token string
	start := func(reset bool) {
		t.Helper()
		srv = httptest.NewUnstartedServer(nil)
		var err error
		runtime, err = OpenWithOptions(t.Context(), dir, Options{Content: true, ContentOrigin: "http://" + srv.Listener.Addr().String(), Reset: reset})
		if err != nil {
			srv.Close()
			t.Fatal(err)
		}
		data, err := os.ReadFile(runtime.TokenPath)
		if err != nil {
			t.Fatal(err)
		}
		token = strings.TrimSpace(string(data))
		srv.Config.Handler = runtime.Handler
		srv.Start()
	}
	stop := func() {
		srv.Close()
		if err := runtime.Close(); err != nil {
			t.Fatal(err)
		}
	}
	start(false)
	defer func() { stop() }()
	call := func(method, path, body, key string, status int) []byte {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal("HTTP request failed")
		}
		defer res.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
		if err != nil || res.StatusCode != status {
			t.Fatalf("%s expected %d, got %d", method, status, res.StatusCode)
		}
		return raw
	}
	listing := func() (string, string, string) {
		t.Helper()
		var v struct {
			Datasets []struct {
				ID        string `json:"id"`
				Store     string `json:"content_store_id"`
				Watermark string `json:"watermark"`
			} `json:"datasets"`
		}
		if json.Unmarshal(call("GET", "/v1/datasets", "", "", 200), &v) != nil || len(v.Datasets) != 1 {
			t.Fatal("invalid listing")
		}
		return v.Datasets[0].ID, v.Datasets[0].Store, v.Datasets[0].Watermark
	}
	call("GET", "/.well-known/tessera", "", "", 200)
	call("GET", "/.well-known/tessera-content", "", "", 200)
	dataset, contentID, _ := listing()
	base := "/v1/datasets/" + dataset
	raw := call("POST", base+"/objects/com.example.entry:query", `{"query":{"include_versions":true}}`, "", 200)
	var records p.RecordSet
	if json.Unmarshal(raw, &records) != nil || len(records.Records) != 1 {
		t.Fatal("fixture query")
	}
	if !bytes.Contains(raw, []byte(`9007199254740993`)) || !bytes.Contains(raw, []byte(`{ "z": 1e0, "a": "\u0061" }`)) {
		t.Fatal("fixture fidelity")
	}
	var media string
	for i, f := range records.Meta.Fields {
		if f.ID == "attachment" {
			media = records.Records[0].Values[i].Payload().(string)
		}
	}
	parts := strings.Split(media, "//")
	if len(parts) != 2 || parts[0] != contentID {
		t.Fatal("unqualified attachment")
	}
	objectPath := base + "/objects/com.example.entry/" + records.Records[0].ID.ID
	fixtureBefore := call("GET", objectPath, "", "", 200)

	// Create related records using an alias, then discard the response entirely.
	call("POST", base+"/meta", `{"type":{"id":"com.example.link","fields":[{"id":"parent","meta_id":"com.example.link","type":"reference"}]}}`, "", 201)
	const key = "aabbccdd-0000-4000-8000-000000000001"
	const transaction = `{"operations":[{"op":"insert","type":"com.example.note","fields":{"body":{"type":"text","value":"Parent"}},"as":"parent"},{"op":"insert","type":"com.example.link","fields":{"parent":{"type":"reference","value":"$parent"}}}]}`
	_ = call("POST", base+":transact", transaction, key, 200)
	_, _, beforeRestart := listing()
	oldToken := token
	stop()
	start(false)
	newDataset, newStore, afterRestart := listing()
	if dataset != newDataset || contentID != newStore || beforeRestart != afterRestart || oldToken != token {
		t.Fatal("restart changed identities or watermark")
	}
	var receipt struct {
		State  string              `json:"state"`
		Result p.TransactionResult `json:"result"`
	}
	if json.Unmarshal(call("POST", base+"/operations/"+key+":status", "", "", 200), &receipt) != nil || receipt.State != "committed" || len(receipt.Result.Results) != 2 {
		t.Fatal("lost response not recoverable")
	}
	recovered := receipt.Result.Results[0].Objects[0].ID
	linked := receipt.Result.Results[1].Objects[0].ID
	var link p.RecordSet
	if json.Unmarshal(call("GET", base+"/objects/com.example.link/"+linked, "", "", 200), &link) != nil || link.Records[0].Values[0].Payload().(p.Reference).ObjectID != recovered {
		t.Fatal("alias did not resolve")
	}
	parentPath := base + "/objects/com.example.note/" + recovered
	var parent p.RecordSet
	if json.Unmarshal(call("GET", parentPath, "", "", 200), &parent) != nil {
		t.Fatal("parent read")
	}
	guarded, _ := json.Marshal(map[string]any{"operations": []any{map[string]any{"op": "expect", "target": p.Target{Type: "com.example.note", ID: recovered}, "version": parent.Records[0].Version}, map[string]any{"op": "update", "target": p.Target{Type: "com.example.note", ID: recovered}, "fields": map[string]any{"body": map[string]string{"type": "text", "value": "Updated"}}}}})
	call("POST", base+":transact", string(guarded), "", 200)
	call("POST", base+":transact", string(guarded), "", 409)
	f := conformance.CoreFixture{URL: srv.URL, Token: token, Client: srv.Client()}
	if err := conformance.CheckPaging(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	if err := conformance.CheckContent(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	// The development server advertises the optional reads, so this run takes
	// the behavioural path rather than the refusal one.
	if err := conformance.CheckOptionalReads(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	call("POST", "/v1/stores/"+contentID+":erase", `{"confirm":"`+contentID+`"}`, "", 200)
	call("POST", "/v1/stores/"+contentID+"/items/"+parts[1]+":resolve", `{"representation":"original"}`, "", 404)
	var before, after p.RecordSet
	_ = json.Unmarshal(fixtureBefore, &before)
	_ = json.Unmarshal(call("GET", objectPath, "", "", 200), &after)
	a, _ := p.MarshalWire(before.Records)
	b, _ := p.MarshalWire(after.Records)
	if !bytes.Equal(a, b) {
		t.Fatal("content erase changed the stored record")
	}
	stop()
	start(true)
	replacement, _, _ := listing()
	if replacement == dataset {
		t.Fatal("reset reused dataset identity")
	}
	call("GET", objectPath, "", "", 404)
	// A reset refuses a live database rather than racing the running server.
	other, err := OpenWithOptions(t.Context(), dir, Options{Reset: true})
	if err == nil {
		other.Close()
		t.Fatal("reset accepted a live database")
	}
	t.Log("HTTP discovery, exact fixtures, aliases, restart/recovery, version guards, paging, content and reset passed")
}
