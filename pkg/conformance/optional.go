package conformance

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	p "github.com/ekarpov/tessera/pkg/protocol"
)

// CheckOptionalReads exercises aggregation, distinct references and text
// matching, which are required operations rather than capabilities.
//
// They were capabilities, and a client without them compensated: reading rows
// and folding them, or filtering after reading. Neither compensation is
// equivalent to the operation it replaces — filtering after reading narrows a
// result set silently, and a client folding rows invents a bound that means
// nothing outside its own codebase. A server that finds any of these expensive
// declares a scan bound and refuses past it, which is what every other
// required operation already does.
//
// The name is kept so a host calling it keeps calling it.
func CheckOptionalReads(ctx context.Context, f CoreFixture) error {
	client := f.Client
	if client == nil {
		client = http.DefaultClient
	}
	call := func(method, path, token string, body any, status int, dst any) error {
		var data []byte
		var err error
		if body != nil {
			if data, err = p.MarshalWire(body); err != nil {
				return err
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, f.URL+path, bytes.NewReader(data))
		if err != nil {
			return err
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		response, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("HTTP request failed")
		}
		defer response.Body.Close()
		if response.StatusCode != status {
			return fmt.Errorf("%s %s: expected HTTP %d, received %d", method, operation(path), status, response.StatusCode)
		}
		if dst != nil {
			if err = json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(dst); err != nil {
				return fmt.Errorf("invalid response encoding")
			}
		}
		return nil
	}
	problem := func(method, path, token string, body any, status int, code string) error {
		var envelope struct {
			Error p.Error `json:"error"`
		}
		if err := call(method, path, token, body, status, &envelope); err != nil {
			return err
		}
		if envelope.Error.Code != code {
			return fmt.Errorf("%s: expected error %s, received %s", operation(path), code, envelope.Error.Code)
		}
		return nil
	}
	// query_result_limit_exceeded covers several bounds, so for the three the
	// protocol names a reason for, the reason is the assertion: a client that
	// cannot tell a sum overflow from a group limit cannot decide whether
	// narrowing the query would help.
	bounded := func(path, token string, body any, reason string) error {
		var envelope struct {
			Error p.Error `json:"error"`
		}
		if err := call("POST", path, token, body, 422, &envelope); err != nil {
			return err
		}
		if envelope.Error.Code != "query_result_limit_exceeded" {
			return fmt.Errorf("%s: expected query_result_limit_exceeded, received %s", operation(path), envelope.Error.Code)
		}
		if envelope.Error.Details["reason"] != reason {
			return fmt.Errorf("%s: expected details.reason %s, received %v", operation(path), reason, envelope.Error.Details["reason"])
		}
		return nil
	}

	var listing struct {
		Datasets []struct {
			ID           string   `json:"id"`
			Capabilities []string `json:"capabilities"`
			Limits       p.Limits `json:"limits"`
		} `json:"datasets"`
	}
	if err := call("GET", "/v1/datasets", f.Token, nil, 200, &listing); err != nil {
		return err
	}
	if len(listing.Datasets) != 1 {
		return fmt.Errorf("fixture needs exactly one dataset")
	}
	dataset := listing.Datasets[0]
	advertised := map[string]bool{}
	for _, name := range dataset.Capabilities {
		advertised[name] = true
	}
	base := "/v1/datasets/" + url.PathEscape(dataset.ID)

	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	typeID := "test.optional.t" + hex.EncodeToString(nonce[:])
	meta := p.Meta{ID: typeID, Name: "Optional reads", Fields: []p.Field{
		{ID: "amount", MetaID: typeID, Name: "Amount", Type: p.IntegerType},
		{ID: "ratio", MetaID: typeID, Name: "Ratio", Type: p.FloatType},
		{ID: "label", MetaID: typeID, Name: "Label", Type: p.TextType},
		{ID: "link", MetaID: typeID, Name: "Link", Type: p.ReferenceType},
	}}
	if err := call("POST", base+"/meta", f.Token, map[string]any{"type": meta}, 201, nil); err != nil {
		return err
	}
	aggregate := base + "/objects/" + typeID + ":aggregate"
	distinct := base + "/objects/" + typeID + ":distinct-references"
	query := base + "/objects/" + typeID + ":query"

	textFilter := map[string]any{"text_contains": map[string]any{"field_id": "label", "text": "ee"}}
	// These three are required operations now, so none of them may be
	// declined and a server naming one in its capabilities is claiming a
	// choice it does not have.
	for _, retired := range p.RetiredReadCapabilities {
		if advertised[retired] {
			return fmt.Errorf("%s is a required operation and MUST NOT be advertised as a capability", retired)
		}
	}

	// Every read requires the read grant. A fixture without a refused token
	// cannot check that, and says so by leaving it empty.
	if f.RefusedToken != "" {
		if err := problem("POST", aggregate, f.RefusedToken, map[string]any{}, 403, "permission_denied"); err != nil {
			return err
		}
	}

	insert := func(fields map[string]p.Value) (string, error) {
		var inserted struct {
			Objects []p.Identity `json:"objects"`
		}
		if err := call("POST", base+"/objects/"+typeID, f.Token, map[string]any{"fields": fields}, 201, &inserted); err != nil {
			return "", err
		}
		if len(inserted.Objects) != 1 {
			return "", fmt.Errorf("insert returned no identity")
		}
		return inserted.Objects[0].ID, nil
	}
	text := func(s string) p.Value { v, _ := p.NewValue(p.TextType, s); return v }
	integer := func(n int64) p.Value { v, _ := p.NewValue(p.IntegerType, n); return v }

	// Unconditional. These were behind the advertisement, so a server that
	// advertised nothing ran none of them — which is what a required
	// operation must not be able to do.
	if err := checkAggregation(aggregate, call, problem, bounded, insert, text, integer, f.Token, dataset.Limits); err != nil {
		return err
	}
	if err := checkSumBoundaries(ctx, base, f.Token, call, bounded, nonce[:]); err != nil {
		return err
	}
	if err := checkDistinctReferences(distinct, dataset.ID, typeID, call, problem, bounded, insert, integer, f.Token, dataset.Limits); err != nil {
		return err
	}
	if err := checkGroupingByReference(base, dataset.ID, f.Token, call, integer, nonce[:]); err != nil {
		return err
	}
	if err := checkExactDecimals(base, f.Token, call, problem, nonce[:]); err != nil {
		return err
	}
	return checkTextMatching(query, textFilter, call, insert, text, f.Token)
}

// operation names the endpoint without the dataset or type identifiers, so a
// failure message carries no user-supplied identifier.
func operation(path string) string {
	for _, suffix := range []string{":aggregate", ":distinct-references", ":query"} {
		if len(path) >= len(suffix) && path[len(path)-len(suffix):] == suffix {
			return suffix
		}
	}
	return "read"
}

type caller func(method, path, token string, body any, status int, dst any) error
type problemCaller func(method, path, token string, body any, status int, code string) error
type boundedCaller func(path, token string, body any, reason string) error
type inserter func(map[string]p.Value) (string, error)

// aggregates decodes the answer without funnelling an integer statistic through
// binary64, so a sum beyond 2^53 can be compared against its exact spelling.
type aggregateRow struct {
	// Group is raw because a group is a text label, a reference identity or
	// null, and which one it is follows from the field the caller grouped by.
	// Decoding it as a string would silently accept a server that answered a
	// reference group as its object_id alone.
	Group      json.RawMessage `json:"group"`
	RowCount   int             `json:"row_count"`
	ValueCount int             `json:"value_count"`
	Sum        json.RawMessage `json:"sum"`
	Minimum    json.RawMessage `json:"minimum"`
	Maximum    json.RawMessage `json:"maximum"`
}

func (r aggregateRow) nullGroup() bool { return string(r.Group) == "null" }

func (r aggregateRow) text() (string, bool) {
	var label string
	return label, json.Unmarshal(r.Group, &label) == nil
}

// reference decodes a group as a qualified identity, and requires all three
// members. A server answering with two of them has not answered with an
// identity: two objects agreeing on the members it sent are still two objects.
func (r aggregateRow) reference() (p.Reference, bool) {
	var identity p.Reference
	if json.Unmarshal(r.Group, &identity) != nil {
		return identity, false
	}
	return identity, identity.Validate() == nil
}

func (r aggregateRow) null() bool {
	for _, raw := range []json.RawMessage{r.Sum, r.Minimum, r.Maximum} {
		if string(raw) != "null" {
			return false
		}
	}
	return true
}

func checkAggregation(path string, call caller, problem problemCaller, bounded boundedCaller, insert inserter, text func(string) p.Value, integer func(int64) p.Value, token string, limits p.Limits) error {
	answer := func(body map[string]any) ([]aggregateRow, error) {
		var response struct {
			Aggregates []aggregateRow `json:"aggregates"`
			Watermark  string         `json:"watermark"`
		}
		if err := call("POST", path, token, body, 200, &response); err != nil {
			return nil, err
		}
		if response.Watermark == "" {
			return nil, fmt.Errorf("aggregate response carries no watermark")
		}
		return response.Aggregates, nil
	}
	// The type is fresh, so nothing matches yet.
	empty, err := answer(map[string]any{"field": "amount"})
	if err != nil {
		return err
	}
	if len(empty) != 1 || empty[0].RowCount != 0 || empty[0].ValueCount != 0 || !empty[0].null() {
		return fmt.Errorf("ungrouped aggregate over no rows must answer once, with null statistics")
	}
	grouped, err := answer(map[string]any{"field": "amount", "group_by": "label"})
	if err != nil {
		return err
	}
	if len(grouped) != 0 {
		return fmt.Errorf("grouped aggregate over no rows must return an empty array")
	}

	// Two rows carry a value; one carries the group but no value, and one
	// carries neither, which is the null group.
	beyond := int64(9007199254740993)
	for _, row := range []map[string]p.Value{
		{"label": text("manual"), "amount": integer(beyond)},
		{"label": text("manual"), "amount": integer(2)},
		{"label": text("manual")},
		{"amount": integer(5)},
	} {
		if _, err = insert(row); err != nil {
			return err
		}
	}
	all, err := answer(map[string]any{"field": "amount"})
	if err != nil {
		return err
	}
	if len(all) != 1 || all[0].RowCount != 4 || all[0].ValueCount != 3 {
		return fmt.Errorf("row_count counts matching rows and value_count only non-null ones")
	}
	// A statistic that lost the integer beyond 2^53 would compare unequal here.
	if string(all[0].Sum) != "9007199254741000" || string(all[0].Maximum) != "9007199254740993" || string(all[0].Minimum) != "2" {
		return fmt.Errorf("exact integer sum or extremes were rounded")
	}
	byLabel, err := answer(map[string]any{"field": "amount", "group_by": "label"})
	if err != nil {
		return err
	}
	if len(byLabel) != 2 {
		return fmt.Errorf("a null group is a group and must not be dropped")
	}
	var nullGroup, manual *aggregateRow
	for i, row := range byLabel {
		if row.nullGroup() {
			nullGroup = &byLabel[i]
		} else if label, ok := row.text(); ok && label == "manual" {
			manual = &byLabel[i]
		}
	}
	// The null group sorts first, whatever the data happened to contain. A
	// client drawing an axis from this needs the order to be the contract's
	// rather than the engine's.
	if !byLabel[0].nullGroup() {
		return fmt.Errorf("the null group must sort first")
	}
	if nullGroup == nil || manual == nil {
		return fmt.Errorf("grouped aggregate lost the null group or its named group")
	}
	if nullGroup.RowCount != 1 || nullGroup.ValueCount != 1 || manual.RowCount != 3 || manual.ValueCount != 2 {
		return fmt.Errorf("grouped counts did not follow the rows")
	}

	// Rows exist but none carries a value: the statistics are null, not zero.
	none, err := answer(map[string]any{"field": "ratio"})
	if err != nil {
		return err
	}
	if len(none) != 1 || none[0].RowCount != 4 || none[0].ValueCount != 0 || !none[0].null() {
		return fmt.Errorf("value_count 0 must report null statistics, never zero")
	}
	// A count alone needs no field.
	counted, err := answer(map[string]any{})
	if err != nil {
		return err
	}
	if len(counted) != 1 || counted[0].RowCount != 4 || counted[0].ValueCount != 0 {
		return fmt.Errorf("aggregate without a field must still count rows")
	}

	for _, refusal := range []struct {
		body   map[string]any
		status int
		code   string
	}{
		{map[string]any{"field": "label"}, 400, "invalid_field"},
		{map[string]any{"field": "absent"}, 400, "invalid_field"},
		{map[string]any{"field": "amount", "group_by": "amount"}, 400, "invalid_field"},
		{map[string]any{"field": "amount", "group_by": "absent"}, 400, "invalid_field"},
	} {
		if err = problem("POST", path, token, refusal.body, refusal.status, refusal.code); err != nil {
			return err
		}
	}

	// One more group than the dataset declares fails rather than truncating.
	// Read from the limit rather than written here: a bound a client cannot
	// read is one it cannot plan around, and a suite asserting its own number
	// would pass against a server that advertised a different one.
	if limits.MaxAggregateGroups < 1 {
		return fmt.Errorf("the dataset declares no max_aggregate_groups")
	}
	for i := 0; i <= limits.MaxAggregateGroups; i++ {
		if _, err = insert(map[string]p.Value{"label": text(fmt.Sprintf("g%03d", i))}); err != nil {
			return err
		}
	}
	return bounded(path, token, map[string]any{"group_by": "label"}, "group_limit")
}

// Grouping by a reference groups by identity, and by the whole identity.
//
// The last part is the one a server can get wrong while looking right, so it
// is checked with identities built to collide: three references agreeing on
// object_id, differing only in meta_id or only in dataset_id. A server
// comparing object_id alone folds them into one group and fails here; a server
// comparing two of the three members fails on one of the pairs. A reference is
// a pointer rather than a foreign key — nothing requires its target to exist,
// which is what makes these constructible without a second dataset.
//
// In a type of its own, because these rows would otherwise change what the
// checks either side of this one see on the same reference field. A suite
// whose cases perturb each other through shared fixture state is one where a
// later edit fails an earlier assertion for no reason a reader can find.
func checkGroupingByReference(base, datasetID, token string, call caller, integer func(int64) p.Value, nonce []byte) error {
	typeID := "test.grouped.t" + hex.EncodeToString(nonce)
	meta := p.Meta{ID: typeID, Name: "Grouped by reference", Fields: []p.Field{
		{ID: "amount", MetaID: typeID, Name: "Amount", Type: p.IntegerType},
		{ID: "link", MetaID: typeID, Name: "Link", Type: p.ReferenceType},
	}}
	if err := call("POST", base+"/meta", token, map[string]any{"type": meta}, 201, nil); err != nil {
		return err
	}
	path := base + "/objects/" + typeID + ":aggregate"
	identity := func(dataset, object, meta string) p.Value {
		v, _ := p.NewValue(p.ReferenceType, p.Reference{DatasetID: dataset, ObjectID: object, MetaID: meta})
		return v
	}
	subject := "o-conformance-subject"
	alternative := typeID + ".other"
	elsewhere := "d-conformance-elsewhere"
	for _, row := range []map[string]p.Value{
		{"link": identity(datasetID, subject, typeID), "amount": integer(10)},
		{"link": identity(datasetID, subject, typeID), "amount": integer(20)},
		{"link": identity(datasetID, subject, alternative), "amount": integer(3)},
		{"link": identity(elsewhere, subject, typeID), "amount": integer(7)},
		{"amount": integer(1)},
	} {
		if err := call("POST", base+"/objects/"+typeID, token, map[string]any{"fields": row}, 201, nil); err != nil {
			return err
		}
	}
	var response struct {
		Aggregates []aggregateRow `json:"aggregates"`
	}
	if err := call("POST", path, token, map[string]any{"field": "amount", "group_by": "link"}, 200, &response); err != nil {
		return err
	}
	grouped := response.Aggregates
	if len(grouped) != 4 {
		return fmt.Errorf("grouping by a reference must compare the whole qualified identity: expected 4 groups, got %d", len(grouped))
	}
	if !grouped[0].nullGroup() {
		return fmt.Errorf("the null group must sort first when grouping by a reference")
	}
	if grouped[0].RowCount != 1 || grouped[0].ValueCount != 1 {
		return fmt.Errorf("a row whose reference is null belongs to the null group, counted")
	}
	// Ascending by meta_id, then dataset_id, then object_id. Every group after
	// the null one must be a whole identity and must not repeat.
	seen := map[p.Reference]int{}
	var previous p.Reference
	for i, row := range grouped[1:] {
		found, ok := row.reference()
		if !ok {
			return fmt.Errorf("a reference group must be a qualified identity with all three members")
		}
		if _, repeated := seen[found]; repeated {
			return fmt.Errorf("the same identity appeared as two groups")
		}
		seen[found] = row.RowCount
		if i > 0 && !ascending(previous, found) {
			return fmt.Errorf("reference groups must ascend by meta_id, then dataset_id, then object_id")
		}
		previous = found
	}
	for wanted, expected := range map[p.Reference]int{
		{DatasetID: datasetID, ObjectID: subject, MetaID: typeID}:      2,
		{DatasetID: datasetID, ObjectID: subject, MetaID: alternative}: 1,
		{DatasetID: elsewhere, ObjectID: subject, MetaID: typeID}:      1,
	} {
		if seen[wanted] != expected {
			return fmt.Errorf("a reference group did not carry the rows that share its identity")
		}
	}
	return nil
}

// ascending is the protocol's total order over references: meta_id, then
// dataset_id, then object_id, each by code point.
func ascending(a, b p.Reference) bool {
	if a.MetaID != b.MetaID {
		return a.MetaID < b.MetaID
	}
	if a.DatasetID != b.DatasetID {
		return a.DatasetID < b.DatasetID
	}
	return a.ObjectID < b.ObjectID
}

func checkDistinctReferences(path, datasetID, typeID string, call caller, problem problemCaller, bounded boundedCaller, insert inserter, integer func(int64) p.Value, token string, limits p.Limits) error {
	reference := func(objectID string) p.Value {
		v, _ := p.NewValue(p.ReferenceType, p.Reference{DatasetID: datasetID, ObjectID: objectID, MetaID: typeID})
		return v
	}
	first, err := insert(map[string]p.Value{"amount": integer(1)})
	if err != nil {
		return err
	}
	second, err := insert(map[string]p.Value{"amount": integer(2)})
	if err != nil {
		return err
	}
	// Two rows point at the same target, so the distinct answer holds two.
	for _, target := range []string{first, second, first} {
		if _, err = insert(map[string]p.Value{"link": reference(target)}); err != nil {
			return err
		}
	}
	// Presence is the negation of an exact null; there is no not_null form.
	present := map[string]any{"not": map[string]any{"field": map[string]any{"id": "link", "exact": map[string]any{"type": "null"}}}}
	linked := map[string]any{"field": "link", "maximum_count": 8, "filter": present}
	var response struct {
		References []p.Reference `json:"references"`
		Watermark  string        `json:"watermark"`
	}
	if err = call("POST", path, token, linked, 200, &response); err != nil {
		return err
	}
	if response.Watermark == "" {
		return fmt.Errorf("distinct references response carries no watermark")
	}
	if len(response.References) != 2 {
		return fmt.Errorf("distinct references did not de-duplicate")
	}
	// Sorted by qualified identity: meta_id, then dataset_id, then object_id.
	for i := 1; i < len(response.References); i++ {
		before, after := response.References[i-1], response.References[i]
		if before.MetaID > after.MetaID ||
			(before.MetaID == after.MetaID && before.DatasetID > after.DatasetID) ||
			(before.MetaID == after.MetaID && before.DatasetID == after.DatasetID && before.ObjectID >= after.ObjectID) {
			return fmt.Errorf("distinct references are not sorted by qualified identity")
		}
	}
	// Exceeding the bound fails; a truncated list must not be returned.
	narrow := map[string]any{"field": "link", "maximum_count": 1, "filter": present}
	if err = bounded(path, token, narrow, "distinct_reference_limit"); err != nil {
		return err
	}
	// Rows whose reference is null fail rather than being skipped: the earlier
	// rows in this type have no link at all.
	unfiltered := map[string]any{"field": "link", "maximum_count": 8}
	if err = problem("POST", path, token, unfiltered, 400, "invalid_field_value"); err != nil {
		return err
	}
	for _, refusal := range []map[string]any{
		{"field": "amount", "maximum_count": 8},
		{"field": "absent", "maximum_count": 8},
		{"field": "link", "maximum_count": 0},
		{"field": "link", "maximum_count": limits.MaxDistinctReferences + 1},
	} {
		if err = problem("POST", path, token, refusal, 400, "invalid_field"); err != nil {
			return err
		}
	}
	return nil
}

func checkTextMatching(path string, filter map[string]any, call caller, insert inserter, text func(string) p.Value, token string) error {
	if _, err := insert(map[string]p.Value{"label": text("Greenhouse")}); err != nil {
		return err
	}
	if _, err := insert(map[string]p.Value{"label": text("GREENHOUSE")}); err != nil {
		return err
	}
	if _, err := insert(map[string]p.Value{}); err != nil {
		return err
	}
	var result p.RecordSet
	body := map[string]any{"query": map[string]any{"filter": filter, "fields": []string{"label"}}}
	if err := call("POST", path, token, body, 200, &result); err != nil {
		return err
	}
	// Literal and case-sensitive: "ee" matches Greenhouse, not GREENHOUSE, and
	// never the row whose label is null.
	if len(result.Records) != 1 {
		return fmt.Errorf("text_contains must match literally and case-sensitively, and never a null")
	}
	// A wildcard has no special meaning, so it matches nothing here.
	wild := map[string]any{"query": map[string]any{"filter": map[string]any{
		"text_contains": map[string]any{"field_id": "label", "text": "Green%"}}, "fields": []string{"label"}}}
	var none p.RecordSet
	if err := call("POST", path, token, wild, 200, &none); err != nil {
		return err
	}
	if len(none.Records) != 0 {
		return fmt.Errorf("wildcards must have no special meaning in text_contains")
	}
	return nil
}

// checkSumBoundaries proves the exactness, cancellation and rounding rules the
// protocol states for aggregate sums. Each case gets its own type, so one
// case's rows cannot reach another's total and an overflow is unambiguous
// about which group produced it.
func checkSumBoundaries(ctx context.Context, base, token string, call caller, bounded boundedCaller, nonce []byte) error {
	suffix := hex.EncodeToString(nonce)
	round := 0
	// each builds a type holding one numeric field, fills it, and returns the
	// aggregate outcome: the sum as raw JSON, or the refusal reason.
	each := func(kind p.Type, values []any) (json.RawMessage, string, error) {
		round++
		typeID := fmt.Sprintf("test.sum.t%s%d", suffix, round)
		meta := p.Meta{ID: typeID, Name: "Sum", Fields: []p.Field{{ID: "v", MetaID: typeID, Name: "V", Type: kind}}}
		if err := call("POST", base+"/meta", token, map[string]any{"type": meta}, 201, nil); err != nil {
			return nil, "", err
		}
		for _, raw := range values {
			value, err := p.NewValue(kind, raw)
			if err != nil {
				return nil, "", err
			}
			if err = call("POST", base+"/objects/"+typeID, token, map[string]any{"fields": map[string]p.Value{"v": value}}, 201, nil); err != nil {
				return nil, "", err
			}
		}
		var response struct {
			Aggregates []aggregateRow `json:"aggregates"`
			Error      p.Error        `json:"error"`
		}
		body := map[string]any{"field": "v"}
		if e := call("POST", base+"/objects/"+typeID+":aggregate", token, body, 200, &response); e != nil {
			// Not a success, so it must be a named bound.
			if e := bounded(base+"/objects/"+typeID+":aggregate", token, body, "sum_overflow"); e != nil {
				return nil, "", e
			}
			return nil, "sum_overflow", nil
		}
		if len(response.Aggregates) != 1 {
			return nil, "", fmt.Errorf("an ungrouped aggregate answered %d times", len(response.Aggregates))
		}
		return response.Aggregates[0].Sum, "", nil
	}

	const maxInt64, minInt64 = int64(9223372036854775807), int64(-9223372036854775808)
	for _, boundary := range []struct {
		name   string
		kind   p.Type
		values []any
		sum    string
		reason string
	}{
		{"exact beyond 2^53", p.IntegerType, []any{int64(9007199254740993), int64(9007199254740993)}, "18014398509481986", ""},
		{"the maximum alone", p.IntegerType, []any{maxInt64}, "9223372036854775807", ""},
		{"the minimum alone", p.IntegerType, []any{minInt64}, "-9223372036854775808", ""},
		{"cancellation back into range", p.IntegerType, []any{maxInt64, int64(1), int64(-1)}, "9223372036854775807", ""},
		{"cancellation from below", p.IntegerType, []any{minInt64, int64(-1), int64(1)}, "-9223372036854775808", ""},
		{"overflow above", p.IntegerType, []any{maxInt64, int64(1)}, "", "sum_overflow"},
		{"overflow below", p.IntegerType, []any{minInt64, int64(-1)}, "", "sum_overflow"},
		{"float cancellation", p.FloatType, []any{9007199254740992.0, 1.0, -9007199254740992.0}, "1", ""},
		{"ties to even, down", p.FloatType, []any{9007199254740992.0, 1.0}, "9007199254740992", ""},
		{"ties to even, up", p.FloatType, []any{9007199254740994.0, 1.0}, "9007199254740996", ""},
	} {
		got, reason, err := each(boundary.kind, boundary.values)
		if err != nil {
			return fmt.Errorf("%s: %w", boundary.name, err)
		}
		if boundary.reason != "" {
			if reason != boundary.reason {
				return fmt.Errorf("%s: expected %s, received sum %s", boundary.name, boundary.reason, got)
			}
			continue
		}
		if reason != "" {
			return fmt.Errorf("%s: refused with %s where a sum was required", boundary.name, reason)
		}
		if !sameNumber(string(got), boundary.sum) {
			return fmt.Errorf("%s: expected sum %s, received %s", boundary.name, boundary.sum, got)
		}
	}
	return nil
}

// sameNumber compares integer spellings exactly and float spellings as
// binary64 values, since the protocol compares the number and not its text.
func sameNumber(got, want string) bool {
	if got == want {
		return true
	}
	a, errA := strconv.ParseFloat(got, 64)
	b, errB := strconv.ParseFloat(want, 64)
	return errA == nil && errB == nil && a == b
}

// An exact decimal survives the round trip and the sum, in a type of its own
// so that nothing either side of it depends on these rows.
//
// The two values below are not examples; each defeats a different platform's
// obvious implementation and neither catches the other. A server that parses
// through binary64 loses the second and keeps the first; one built on a
// fixed-width 38-digit decimal loses the first and keeps the second. Both are
// stored and read back, then summed, because a value can survive storage and
// still die in an accumulator.
func checkExactDecimals(base, token string, call caller, problem problemCaller, nonce []byte) error {
	typeID := "test.fixed.t" + hex.EncodeToString(nonce)
	scale := 20
	meta := p.Meta{ID: typeID, Name: "Exact decimals", Fields: []p.Field{
		{ID: "amount", MetaID: typeID, Name: "Amount", Type: p.FixedType, Scale: &scale},
	}}
	if err := call("POST", base+"/meta", token, map[string]any{"type": meta}, 201, nil); err != nil {
		return err
	}
	path := base + "/objects/" + typeID
	beyondBinary64 := "1.00000000000000001"
	beyond38Digits := "1234567890123456789012345678901234567890.5"
	for _, value := range []string{beyondBinary64, beyond38Digits, "0.1", "0.1"} {
		fields := map[string]any{"amount": map[string]any{"type": "fixed", "value": value}}
		var inserted struct {
			Objects []p.Identity `json:"objects"`
		}
		if err := call("POST", path, token, map[string]any{"fields": fields}, 201, &inserted); err != nil {
			return fmt.Errorf("an exact decimal was refused: %w", err)
		}
		if len(inserted.Objects) != 1 {
			return fmt.Errorf("inserting an exact decimal returned no identity")
		}
		// Read back through the ordinary single-object path, not from the
		// insert's own answer: what matters is what the store holds.
		// Values are positional, aligned with the answer's own meta; this
		// type declares one field, so there is one value.
		var read struct {
			Records []struct {
				Values []json.RawMessage `json:"values"`
			} `json:"records"`
		}
		if err := call("GET", path+"/"+inserted.Objects[0].ID, token, nil, 200, &read); err != nil {
			return err
		}
		if len(read.Records) != 1 || len(read.Records[0].Values) != 1 {
			return fmt.Errorf("an exact decimal could not be read back")
		}
		if got := string(read.Records[0].Values[0]); got != `{"type":"fixed","value":"`+value+`"}` {
			return fmt.Errorf("an exact decimal did not survive the round trip: %s", got)
		}
	}
	// Non-canonical spellings are refused rather than normalised. A server
	// that normalised would pass every assertion above and return values its
	// caller never sent.
	for _, refused := range []string{"1.50", "1.0", "01", "-0", "+1", "1e5"} {
		body := map[string]any{"fields": map[string]any{"amount": map[string]any{"type": "fixed", "value": refused}}}
		if err := problem("POST", path, token, body, 400, "invalid_field_value"); err != nil {
			return fmt.Errorf("a non-canonical decimal %q was not refused: %w", refused, err)
		}
	}
	// A value past the field's declared scale needs rounding to be stored, and
	// this protocol does not round.
	tooPrecise := "0." + strings.Repeat("0", scale) + "1"
	body := map[string]any{"fields": map[string]any{"amount": map[string]any{"type": "fixed", "value": tooPrecise}}}
	if err := problem("POST", path, token, body, 400, "invalid_field_value"); err != nil {
		return fmt.Errorf("a value past the declared scale was not refused: %w", err)
	}

	var response struct {
		Aggregates []aggregateRow `json:"aggregates"`
	}
	if err := call("POST", path+":aggregate", token, map[string]any{"field": "amount"}, 200, &response); err != nil {
		return err
	}
	if len(response.Aggregates) != 1 {
		return fmt.Errorf("an ungrouped aggregate over a fixed field must answer once")
	}
	// A string, not a number. A number here is where the exactness dies on the
	// client, after the server took care to keep it.
	var total string
	if json.Unmarshal(response.Aggregates[0].Sum, &total) != nil {
		return fmt.Errorf("the sum of a fixed field must be a JSON string, not a number: %s", response.Aggregates[0].Sum)
	}
	// 1.00000000000000001 + 1234567890123456789012345678901234567890.5 + 0.1 + 0.1
	const expected = "1234567890123456789012345678901234567891.70000000000000001"
	if total != expected {
		return fmt.Errorf("the sum of exact decimals was %s, expected %s", total, expected)
	}
	return nil
}
