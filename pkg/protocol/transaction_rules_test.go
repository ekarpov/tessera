package protocol_test

import (
	"encoding/json"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"strings"
	"testing"
)

func TestResolvedTransactionGrammar(t *testing.T) {
	prefix := `{"op":"insert","type":"example.item","fields":{},"as":"entry"}`
	for _, tc := range []struct {
		name, ops, code string
		index           int
	}{
		{"unknown_member", prefix + `,{"op":"delete","target":{"type":"example.item","id":"$entry"},"typo":true}`, "invalid_request", 1},
		{"known_but_wrong_member", `{"op":"delete_type","type":"example.item","fields":{}}`, "invalid_request", 0},
		{"missing", `{"op":"insert","type":"example.item"}`, "invalid_request", 0},
		{"unknown_op", `{"op":"future"}`, "invalid_request", 0},
		{"duplicate", prefix + `,` + prefix, "invalid_request", 1},
		{"forward", `{"op":"delete","target":{"type":"example.item","id":"$entry"}},` + prefix, "invalid_request", 0},
		{"unknown_reference", `{"op":"insert","type":"example.item","fields":{"ref":{"type":"reference","value":"$missing"}}}`, "invalid_request", 0},
		{"unknown_predicate", `{"op":"delete_where","type":"example.item","filter":{"object_ids":["$missing"]}}`, "invalid_request", 0},
		{"unknown_object_ids", `{"op":"delete_where","type":"example.item","object_ids":["$missing"]}`, "invalid_request", 0},
		{"as_on_check", `{"op":"expect_type","type":"example.item","schema_version":"v","as":"entry"}`, "invalid_request", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := p.DecodeTransaction([]byte(`{"operations":[`+tc.ops+`]}`), p.DevelopmentLimits())
			problem, ok := err.(*p.Error)
			if !ok || problem.Code != tc.code || problem.OperationIndex == nil || *problem.OperationIndex != tc.index {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
	for _, name := range []string{"a", "_", "A0_", strings.Repeat("a", 64), "0bad", "two.words", "é", "$entry", strings.Repeat("a", 65), ""} {
		raw, _ := json.Marshal(map[string]any{"operations": []any{map[string]any{"op": "insert", "type": "example.item", "fields": map[string]any{}, "as": name}}})
		_, err := p.DecodeTransaction(raw, p.DevelopmentLimits())
		valid := name == "a" || name == "_" || name == "A0_" || name == strings.Repeat("a", 64)
		if (err == nil) != valid {
			t.Fatalf("alias boundary accepted=%v", err == nil)
		}
	}
	// Receiving an opaque ID beginning with '$' remains valid. Minting is tested
	// at the engine/HTTP boundary, not by changing the identifier vectors.
	if err := p.ValidateIdentifier(p.ObjectIdentifier, "$foo"); err != nil {
		t.Fatal(err)
	}
	_, err := p.DecodeTransaction([]byte(`{"operations":[`+prefix+`,{"op":"delete_where","type":"example.item","object_ids":["$entry"]}]}`), p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
}
