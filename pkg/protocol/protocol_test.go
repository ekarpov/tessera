package protocol_test

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"

	p "github.com/ekarpov/tessera/pkg/protocol"
)

func TestRequestBoundary(t *testing.T) {
	type request struct {
		Fields map[string]p.Value `json:"fields"`
	}
	cases := []struct{ name, body, code string }{
		{"valid", `{"fields":{"value":{"type":"integer","value":9007199254740993}}}`, ""},
		{"unknown_request", `{"fields":{},"typo":true}`, "invalid_request"},
		{"unknown_value", `{"fields":{"v":{"type":"integer","value":1,"typo":true}}}`, "invalid_request"},
		{"unknown_reference", `{"fields":{"v":{"type":"reference","value":{"dataset_id":"s","object_id":"1","meta_id":"root","typo":true}}}}`, "invalid_request"},
		{"duplicate_root", `{"fields":{},"fields":{}}`, "invalid_field"},
		{"duplicate_escaped_key", `{"fields":{"x":{"type":"null"},"\u0078":{"type":"null"}}}`, "invalid_field"},
		{"duplicate_in_array", `{"fields":{"v":{"type":"integer","value":1,"future":[{"a":1,"a":2}]}}}`, "invalid_field"},
		{"wrong_case", `{"Fields":{}}`, "invalid_request"},
		{"trailing_document", `{"fields":{}} {}`, "invalid_field_value"},
		{"invalid_utf8", string([]byte{'{', '"', 0xff, '"', ':', '1', '}'}), "invalid_field_value"},
		{"low_surrogate", `{"fields":{"v":{"type":"text","value":"\udc00"}}}`, "invalid_field_value"},
		{"high_surrogate", `{"fields":{"v":{"type":"text","value":"\ud800x"}}}`, "invalid_field_value"},
		{"valid_replacement_character", `{"fields":{"v":{"type":"text","value":"\ufffd"}}}`, ""},
		{"valid_surrogate_pair", `{"fields":{"v":{"type":"text","value":"\ud83d\ude42"}}}`, ""},
		{"json_text_is_not_request_structure", `{"fields":{"v":{"type":"json","value":"{\"a\":1,\"a\":2}"}}}`, ""},
		{"array_body", `[]`, "invalid_field"},
		{"null_body", `null`, "invalid_field"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var dst request
			checkCode(t, p.DecodeRequest([]byte(tc.body), &dst, len(tc.body)), tc.code)
		})
	}
	var dst request
	checkCode(t, p.DecodeRequest([]byte(`{"fields":{}}`), &dst, 1), "query_result_limit_exceeded")
	checkCode(t, p.DecodeRequest([]byte(`{}`), nil, 10), "invalid_field")
	checkCode(t, p.DecodeRequest([]byte(`{}`), (*request)(nil), 10), "invalid_field")
}

func TestErrorsDoNotContainInput(t *testing.T) {
	secret := "synthetic-secret-never-in-diagnostics"
	var v p.Value
	for _, input := range []string{`{"type":"` + secret + `","value":1}`, `{"type":"integer","value":"` + secret + `"}`} {
		err := json.Unmarshal([]byte(input), &v)
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Fatal("unsafe diagnostic")
		}
	}
}

func TestConstructorsRejectMalformedPayloads(t *testing.T) {
	for _, tc := range []struct {
		kind  p.Type
		value any
	}{
		{p.TextType, string([]byte{0xff})}, {p.FloatType, math.Inf(1)},
		{p.FloatType, math.NaN()}, {p.NullType, 1},
	} {
		if _, err := p.NewValue(tc.kind, tc.value); err == nil {
			t.Fatal("accepted invalid payload")
		}
	}
	var v p.Value
	if err := json.Unmarshal([]byte(`{"type":"integer","value":0e9999999999999999999999999}`), &v); err != nil {
		t.Fatal(err)
	}
	if v.Payload() != int64(0) {
		t.Fatal("zero exponent value changed")
	}
}

func TestEffectiveMetadata(t *testing.T) {
	root := "root"
	types := map[string]p.Meta{
		root:    {ID: root, Name: "Root", Fields: []p.Field{{ID: "recorded_at", MetaID: root, Name: "When", Type: p.DateTimeType}}},
		"child": {ID: "child", ParentID: &root, Name: "Child", Fields: []p.Field{{ID: "amount", MetaID: "child", Name: "Amount", Type: p.IntegerType}}},
	}
	m := types[root]
	effective, err := p.EffectiveMetadata(types, "child")
	if err != nil {
		t.Fatal(err)
	}
	if len(effective.Fields) != 2 || effective.Fields[0].MetaID != root || effective.Fields[1].ID != "amount" {
		t.Fatal("wrong inheritance or stable order")
	}
	effective.Fields[0].Name = "changed"
	*effective.ParentID = "changed"
	if types[root].Fields[0].Name != "When" || *types["child"].ParentID != root {
		t.Fatal("output aliases input")
	}
	child := types["child"]
	child.Fields[0].ID = "recorded_at"
	types["child"] = child
	_, err = p.EffectiveMetadata(types, "child")
	checkCode(t, err, "meta_invalid_field")
	child.Fields[0].ID = "amount"
	types["child"] = child
	childID := "child"
	m.ParentID = &childID
	types[root] = m
	_, err = p.EffectiveMetadata(types, "child")
	checkCode(t, err, "meta_schema_error")
	delete(types, root)
	_, err = p.EffectiveMetadata(types, "child")
	checkCode(t, err, "meta_schema_error")
	_, err = p.EffectiveMetadata(types, "missing")
	checkCode(t, err, "meta_type_not_found")
}

func TestRecordsPreserveProjectionAndIdentity(t *testing.T) {
	value, _ := p.NewValue(p.IntegerType, int64(42))
	r := p.RecordSet{Meta: p.Meta{ID: "root", Fields: []p.Field{{ID: "amount", MetaID: "root", Type: p.IntegerType}}}, Records: []p.Record{{ID: p.ObjectID{ID: "10", DatasetID: "s"}, MetaID: "child", Values: []p.Value{value}}}}
	checkCode(t, r.Validate("s", p.DevelopmentLimits()), "")
	checkCode(t, r.Validate("other", p.DevelopmentLimits()), "invalid_field_value")
	r.Records[0].Values = nil
	checkCode(t, r.Validate("s", p.DevelopmentLimits()), "invalid_field_value")
	r.Meta.Fields = nil
	checkCode(t, r.Validate("s", p.DevelopmentLimits()), "")
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"fields":[]`)) || !bytes.Contains(data, []byte(`"values":[]`)) {
		t.Fatal("empty projection must use arrays")
	}
	r.Records = nil
	data, err = json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"records":[]`)) {
		t.Fatal("empty record set must use an array")
	}
}

func TestRequestDigest(t *testing.T) {
	body := []byte(`{"fields":{}}`)
	want := p.RequestDigest("POST", "/v1/datasets/s/objects/root", body)
	if want != p.RequestDigest("POST", "/v1/datasets/s/objects/root", bytes.Clone(body)) {
		t.Fatal("identical retry differs")
	}
	for _, got := range [][32]byte{
		p.RequestDigest("PUT", "/v1/datasets/s/objects/root", body),
		p.RequestDigest("POST", "/v1/datasets/s/objects/other", body),
		p.RequestDigest("POST", "/v1/datasets/s/objects/root?confirm=x", body),
		p.RequestDigest("POST", "/v1/datasets/s/objects/root", []byte(`{ "fields":{} }`)),
	} {
		if got == want {
			t.Fatal("changed request replays")
		}
	}
	if p.RequestDigest("AB", "C", []byte("D")) == p.RequestDigest("A", "BC", []byte("D")) {
		t.Fatal("ambiguous framing")
	}
	checkCode(t, p.ValidateOperationKey("2bdc64e0-fb0f-4f98-ae30-0ce132b4dc1b"), "")
	checkCode(t, p.ValidateOperationKey("not-a-uuid"), "invalid_identifier")
}

func FuzzValueRoundTrip(f *testing.F) {
	for _, input := range []string{`{"type":"integer","value":9007199254740993}`, `{"type":"null"}`, `{"type":"json","value":" [1, 2] "}`, `{"type":"text","value":"🙂"}`} {
		f.Add(input)
	}
	f.Fuzz(func(t *testing.T, input string) {
		if len(input) > 65536 {
			t.Skip()
		}
		var v p.Value
		if json.Unmarshal([]byte(input), &v) != nil {
			return
		}
		encoded, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var decoded p.Value
		if err = json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		if !valueEqual(v, decoded) {
			t.Fatal("round-trip changed value")
		}
	})
}

// A dataset stored before `max_aggregate_groups` had a name decodes it as
// zero, and a zero read as a bound would refuse every grouped aggregation on
// every dataset a deployment already holds. This is the one migration-shaped
// risk in adding the limit, and it was written down as such before the limit
// was added.
func TestStoredLimitsWithoutTheGroupBoundGetTheDefault(t *testing.T) {
	stored, err := json.Marshal(p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	var members map[string]json.RawMessage
	if err = json.Unmarshal(stored, &members); err != nil {
		t.Fatal(err)
	}
	if _, present := members["max_aggregate_groups"]; !present {
		t.Fatal("the limit is no longer written, so this test proves nothing")
	}
	delete(members, "max_aggregate_groups")
	older, err := json.Marshal(members)
	if err != nil {
		t.Fatal(err)
	}
	var limits p.Limits
	if err = json.Unmarshal(older, &limits); err != nil {
		t.Fatal(err)
	}
	if limits.MaxAggregateGroups != p.DefaultAggregateGroups {
		t.Fatal("a row written before the bound existed decoded as a bound of", limits.MaxAggregateGroups)
	}
	// Every other bound still decodes as stored: the default applies to the
	// absent member and not to whatever else happened to be zero.
	if limits.MaxScannedRows != p.DevelopmentLimits().MaxScannedRows {
		t.Fatal("decoding replaced a limit that was present")
	}
}
