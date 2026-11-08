//go:build devfaults

package devfaults_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/ekarpov/tessera/internal/devfaults"
	"github.com/ekarpov/tessera/internal/devserver"
)

// The point of injecting a lost response is that the write still happened. A
// client that reads the failure and retries without its idempotency key
// duplicates the record; one that asks for the outcome recovers it. This test
// drives that through the real development composition over HTTP.
func TestADroppedResponseStillCommitted(t *testing.T) {
	if !devfaults.Enabled() {
		t.Fatal("this test requires the devfaults build tag")
	}
	runtime, err := devserver.OpenWithOptions(t.Context(), t.TempDir(), devserver.Options{Faults: true})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	server := httptest.NewServer(runtime.Handler)
	defer server.Close()
	token, err := os.ReadFile(runtime.TokenPath)
	if err != nil {
		t.Fatal(err)
	}
	bearer := strings.TrimSpace(string(token))

	call := func(method, path, body, fault, key string) (*http.Response, error) {
		request, e := http.NewRequestWithContext(t.Context(), method, server.URL+path, strings.NewReader(body))
		if e != nil {
			t.Fatal(e)
		}
		request.Header.Set("Authorization", "Bearer "+bearer)
		request.Header.Set("Content-Type", "application/json")
		if fault != "" {
			request.Header.Set(devfaults.Header, fault)
		}
		if key != "" {
			request.Header.Set("Idempotency-Key", key)
		}
		return server.Client().Do(request)
	}
	decode := func(response *http.Response) map[string]any {
		t.Helper()
		defer response.Body.Close()
		var value map[string]any
		if json.NewDecoder(response.Body).Decode(&value) != nil {
			t.Fatal("undecodable response")
		}
		return value
	}
	listing, err := call("GET", "/v1/datasets", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	datasets := decode(listing)
	dataset := datasets["datasets"].([]any)[0].(map[string]any)["id"].(string)
	base := "/v1/datasets/" + dataset

	const typeID = "test.faults"
	created, err := call("POST", base+"/meta", `{"type":{"id":"`+typeID+`","fields":[{"id":"n","meta_id":"`+typeID+`","type":"integer"}]}}`, "", "")
	if err != nil || created.StatusCode != 201 {
		t.Fatal("schema creation failed", err)
	}
	created.Body.Close()

	// Throttling happens before the work, so nothing is written.
	throttled, err := call("POST", base+"/objects/"+typeID, `{"fields":{"n":{"type":"integer","value":1}}}`, "throttle", "")
	if err != nil {
		t.Fatal(err)
	}
	if throttled.StatusCode != 429 || throttled.Header.Get("Retry-After") == "" {
		t.Fatal("throttle did not carry Retry-After", throttled.StatusCode)
	}
	throttled.Body.Close()
	if count(t, call, base, typeID) != 0 {
		t.Fatal("a throttled request wrote a record")
	}

	// A dropped response happens after the work. The client sees a transport
	// failure and the record exists anyway, which is the whole problem
	// idempotency and :status exist to solve.
	const key = "3f2504e0-4f89-11d3-9a0c-0305e82c3301"
	body := `{"operations":[{"op":"insert","type":"` + typeID + `","fields":{"n":{"type":"integer","value":7}},"as":"e"}]}`
	if _, err = call("POST", base+":transact", body, "drop-response", key); err == nil {
		t.Fatal("a dropped response looked like a successful reply")
	}
	if count(t, call, base, typeID) != 1 {
		t.Fatal("the dropped response did not commit, so there is nothing to recover")
	}
	// Asking recovers the outcome rather than guessing at it.
	recovered, err := call("POST", base+"/operations/"+key+":status", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	status := decode(recovered)
	if status["state"] != "committed" {
		t.Fatal("the lost outcome was not recoverable", status["state"])
	}
	// And replaying the identical request creates nothing further.
	replay, err := call("POST", base+":transact", body, "", key)
	if err != nil {
		t.Fatal(err)
	}
	replay.Body.Close()
	if count(t, call, base, typeID) != 1 {
		t.Fatal("replaying after a dropped response created a second record")
	}

	// A disconnect mid-response is a third shape: the client gets a truncated
	// body rather than a clean answer, and must not treat it as either.
	response, err := call("POST", base+"/objects/"+typeID, `{"fields":{"n":{"type":"integer","value":9}}}`, "disconnect", "")
	if err == nil {
		_, err = io.ReadAll(response.Body)
		response.Body.Close()
	}
	if err == nil {
		t.Fatal("a disconnected response read cleanly to the end")
	}
}

func count(t *testing.T, call func(method, path, body, fault, key string) (*http.Response, error), base, typeID string) int {
	t.Helper()
	response, err := call("POST", base+"/objects/"+typeID+":count", `{}`, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var value map[string]any
	if json.NewDecoder(response.Body).Decode(&value) != nil {
		t.Fatal("undecodable count")
	}
	return int(value["count"].(float64))
}
