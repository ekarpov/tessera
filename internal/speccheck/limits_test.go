package speccheck_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	p "github.com/ekarpov/tessera/pkg/protocol"
)

// The discovery example in the protocol is the only place a client is shown
// what `limits` contains, and it is prose: nothing made it agree with the
// struct that produces the real one. A bound the code sends and the example
// omits is a bound no client knows to read, which is the whole substance of
// the smaller half of review finding 18.
//
// Compared by reflection rather than by parsing the Go source, so the binding
// is to the type rather than to how it is written.
func TestTheDiscoveryExampleShowsTheLimitsTheCodeSends(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "specs", "tessera-protocol-v1.md"))
	if err != nil {
		t.Fatal(err)
	}
	shown := exampleLimits(t, string(raw))

	// What a server actually sends, rather than what the struct tags say it
	// might. DevelopmentLimits is documented as matching this example, so
	// marshalling it is the closest thing to a real discovery document
	// available here — and a member that survives `omitempty` is one every
	// caller receives.
	//
	// **This distinction is the whole point and it was got wrong once.** The
	// check used to treat an `omitempty` tag as evidence a limit was
	// optional, and skip it. `max_materialized_bytes` is tagged `omitempty`
	// and is never zero in practice, so it was sent to every client, defined
	// in no document, and checked by nothing. Tagged optional and actually
	// optional are not the same thing, and only one of them is observable.
	sent := map[string]bool{}
	encoded, err := json.Marshal(p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	var members map[string]json.RawMessage
	if err = json.Unmarshal(encoded, &members); err != nil {
		t.Fatal(err)
	}
	for name := range members {
		sent[name] = true
	}

	declared := map[string]bool{}
	fields := reflect.TypeOf(p.Limits{})
	for i := 0; i < fields.NumField(); i++ {
		name, _, _ := strings.Cut(fields.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			declared[name] = true
		}
	}

	for name := range sent {
		if !shown[name] {
			t.Errorf("a server sends %s and the discovery example does not show it", name)
		}
	}
	for name := range shown {
		if !declared[name] {
			t.Errorf("the discovery example shows %s and the code has no such limit", name)
		}
	}
}

// exampleLimits reads the `"limits": { … }` object out of the first fenced
// block that carries one. It decodes rather than pattern-matches, so a block
// that is not valid JSON fails here too — which is worth catching on its own,
// since the example is the first thing a client copies.
func exampleLimits(t *testing.T, document string) map[string]bool {
	t.Helper()
	for _, block := range strings.Split(document, "```")[1:] {
		body := strings.TrimPrefix(strings.TrimSpace(block), "json")
		if !strings.Contains(body, `"limits"`) {
			continue
		}
		var parsed struct {
			Datasets []struct {
				Limits map[string]json.RawMessage `json:"limits"`
			} `json:"datasets"`
		}
		if json.Unmarshal([]byte(body), &parsed) != nil || len(parsed.Datasets) == 0 {
			continue
		}
		shown := map[string]bool{}
		for name := range parsed.Datasets[0].Limits {
			shown[name] = true
		}
		if len(shown) > 0 {
			return shown
		}
	}
	t.Fatal("no discovery example with a limits object was found in the protocol")
	return nil
}
