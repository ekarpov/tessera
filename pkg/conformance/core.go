// Package conformance exercises the implemented Tessera core subset over HTTP. Passing
// this subset does not establish full protocol or composed-server isolation.
package conformance

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	p "github.com/ekarpov/tessera/pkg/protocol"
)

// CoreFixture addresses a dataset this suite may damage.
//
// **Never point this at a deployment holding records anybody wants.** The
// suites here are not read-only and are not a health check: they define types,
// write records, upload content and leave all of it behind. Against a real
// deployment that is synthetic conformance data written permanently into
// somebody's storage, and no part of this package removes it.
//
// "Disposable" is the whole requirement and it reads past easily — a consumer
// building a deployment gate pointed one at a live server, because a gate that
// exercises the real thing sounds like the more honest gate. It is not: the
// dataset must be one created for the run and thrown away after it.
//
// Re-running against the same dataset is not supported either. Some suites
// mint a fresh type per run and some use a fixed identifier, so a second run
// meets its own leftovers — which is a defect that only ever appears on the
// second run, and never in a job that starts from nothing.
//
// Token needs read, write and schema; RefusedToken authenticates but has write
// without read.
type CoreFixture struct {
	URL, Token, RefusedToken string
	Client                   *http.Client
	// Share installs a read-only guest grant over the named concrete types and
	// returns a token authenticating as that guest. Grant administration is
	// deliberately outside the protocol, so the host has to supply it; a
	// server that advertises sharing and leaves this nil fails CheckSharing
	// rather than skipping it, because then nothing proves the capability.
	Share func(ctx context.Context, types []string) (token string, err error)
	// Contribute installs an insert grant over the named concrete types, with
	// no read, and returns a token authenticating as that contributor. Owed
	// alongside Share, for the same reason: an insert grant is a grant.
	Contribute func(ctx context.Context, types []string) (token string, err error)

	// WithdrawCreation takes the fixture token's create_dataset right away and
	// returns a function restoring it. Server rights are account state, so like
	// grant administration they sit outside the protocol and the host has to
	// supply this; a server advertising dataset_creation and leaving it nil
	// fails CheckCreation rather than skipping, because the interesting promise
	// is what happens to an outcome once the right that produced it is gone.
	WithdrawCreation func(ctx context.Context) (restore func(), err error)

	// DisableCreation stops the deployment offering creation at all and returns
	// a function restoring it. Distinct from WithdrawCreation on purpose: one
	// withdraws this caller's authority, the other withdraws the capability,
	// and a client has to be able to tell the refusals apart while both leave
	// recovery working.
	DisableCreation func(ctx context.Context) (restore func(), err error)
}

// CheckCore fails on the first violated contract, without including response
// bodies, field values or credentials in diagnostics. It accepts any HTTP server.
func CheckCore(ctx context.Context, f CoreFixture) error {
	client := f.Client
	if client == nil {
		client = http.DefaultClient
	}
	call := func(method, path, token string, body any, status int, dst any) error {
		var data []byte
		var err error
		if body != nil {
			data, err = p.MarshalWire(body)
			if err != nil {
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
			return fmt.Errorf("%s: expected HTTP %d, received %d", method, status, response.StatusCode)
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
			return fmt.Errorf("expected error %s, received %s", code, envelope.Error.Code)
		}
		return nil
	}
	// Unknown response members are additive; validate the version metadata we know.
	var discovery map[string]json.RawMessage
	if err := call("GET", "/.well-known/tessera", "", nil, 200, &discovery); err != nil {
		return err
	}
	if err := checkVersionMetadata(discovery); err != nil {
		return err
	}
	var versions []string
	_ = json.Unmarshal(discovery["protocol_versions"], &versions) // Validated above.
	if _, err := p.SelectProtocolVersion(versions, []string{"1"}); err != nil {
		return fmt.Errorf("core suite requires advertised protocol 1")
	}
	if err := problem("GET", "/v1/datasets", "", nil, 401, "authentication_failed"); err != nil {
		return err
	}
	if raw, present := discovery["display_name"]; present {
		var name string
		if json.Unmarshal(raw, &name) != nil || name == "" {
			return fmt.Errorf("discovery display_name must be a non-empty string")
		}
	}
	// A list member is a list even when it is empty. A null forces every
	// client to special-case a member the protocol says is always there.
	for _, member := range []string{"protocol_versions", "auth_methods", "authorization_servers"} {
		raw, present := discovery[member]
		if !present {
			return fmt.Errorf("discovery omits %s", member)
		}
		var list []string
		if json.Unmarshal(raw, &list) != nil || list == nil {
			return fmt.Errorf("discovery encodes %s as null rather than an array", member)
		}
	}
	if err := checkAuthorizationServersAreReachableSafely(discovery["authorization_servers"]); err != nil {
		return err
	}
	var listing struct {
		Datasets []struct {
			ID     string   `json:"id"`
			Limits p.Limits `json:"limits"`
			Access struct {
				Read bool `json:"read"`
			} `json:"access"`
		} `json:"datasets"`
	}
	if err := call("GET", "/v1/datasets", f.Token, nil, 200, &listing); err != nil {
		return err
	}
	if len(listing.Datasets) != 1 || !listing.Datasets[0].Access.Read {
		return fmt.Errorf("fixture needs exactly one readable dataset")
	}
	base := "/v1/datasets/" + url.PathEscape(listing.Datasets[0].ID)
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	typeID := "test.core.t" + hex.EncodeToString(nonce[:])
	const opaque = `{ "z": 1.00, "a": [true, false] }`
	meta := p.Meta{ID: typeID, Name: "Synthetic", Roles: []string{"foreign.vocabulary"}, Annotations: map[string]json.RawMessage{"opaque": json.RawMessage(opaque)}, Fields: []p.Field{{ID: "amount", MetaID: typeID, Name: "Amount", Type: p.IntegerType}, {ID: "note", MetaID: typeID, Name: "Note", Type: p.TextType}}}
	if err := call("POST", base+"/meta", f.Token, map[string]any{"type": meta}, 201, nil); err != nil {
		return err
	}
	if err := problem("POST", base+"/meta", f.Token, map[string]any{"type": meta}, 409, "meta_type_already_exists"); err != nil {
		return err
	}
	if err := problem("GET", base+"/meta/"+typeID, f.RefusedToken, nil, 403, "permission_denied"); err != nil {
		return err
	}
	var gotMeta struct {
		Type p.Meta `json:"type"`
	}
	if err := call("GET", base+"/meta/"+typeID, f.Token, nil, 200, &gotMeta); err != nil {
		return err
	}
	if gotMeta.Type.SchemaVersion == "" || string(gotMeta.Type.Annotations["opaque"]) != opaque || gotMeta.Type.ID != typeID || len(gotMeta.Type.Fields) != 2 {
		return fmt.Errorf("metadata round trip failed")
	}
	if err := checkImmutable(base, f.Token, call, nonce[:]); err != nil {
		return err
	}
	// Schema names are returned as stored. A display language is the client's
	// business: if Accept-Language could change a name or a version, changing
	// language would invalidate an expect_type guard the client still holds.
	for _, language := range []string{"fi", "ru-RU", "en;q=0.8, *;q=0.5", ""} {
		request, err := http.NewRequestWithContext(ctx, "GET", f.URL+base+"/meta/"+typeID, nil)
		if err != nil {
			return err
		}
		request.Header.Set("Authorization", "Bearer "+f.Token)
		if language != "" {
			request.Header.Set("Accept-Language", language)
		}
		response, err := client.Do(request)
		if err != nil {
			return fmt.Errorf("HTTP request failed")
		}
		var negotiated struct {
			Type p.Meta `json:"type"`
		}
		decodeErr := json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&negotiated)
		response.Body.Close()
		if response.StatusCode != 200 || decodeErr != nil {
			return fmt.Errorf("metadata read under Accept-Language failed")
		}
		if negotiated.Type.Name != gotMeta.Type.Name || negotiated.Type.SchemaVersion != gotMeta.Type.SchemaVersion {
			return fmt.Errorf("Accept-Language changed a metadata name or schema version")
		}
		for i, field := range negotiated.Type.Fields {
			if field.Name != gotMeta.Type.Fields[i].Name {
				return fmt.Errorf("Accept-Language changed a field name")
			}
		}
	}
	// Declared bounds are enforced server-side, against the limits this server
	// advertises rather than the ones this suite happens to know. A server that
	// enforces nothing must fail here: without these, the suite that defines
	// conformance cannot tell a bounded server from an unbounded one.
	bounds := listing.Datasets[0].Limits
	// A bound that can be set low enough to void the guarantee it bounds is
	// not a bound. Retention is required to be at least the advertised value
	// and the advertised value was unconstrained, so a server could promise a
	// minute and stay conformant while leaving a client with no way to
	// reconcile a lost response — which is the single thing the key exists for.
	if bounds.IdempotencyRetentionSeconds < p.MinimumIdempotencyRetentionSeconds {
		return fmt.Errorf("advertised idempotency retention is %d seconds, below the floor of %d: a client cannot reconcile inside it",
			bounds.IdempotencyRetentionSeconds, p.MinimumIdempotencyRetentionSeconds)
	}
	// A storage quota is optional; one that is advertised has to be one a
	// client can show, so no negative quota passes for an absent one.
	if bounds.MaxStoredBytes < 0 {
		return fmt.Errorf("advertised max_stored_bytes is %d; a quota is absent or positive", bounds.MaxStoredBytes)
	}
	wrapped := func(body map[string]any) map[string]any {
		return map[string]any{"query": body}
	}
	deep := any(map[string]any{"field": map[string]any{"id": "amount", "exact": map[string]any{"type": "null"}}})
	for i := 0; i < bounds.MaxPredicateDepth+1; i++ {
		deep = map[string]any{"not": deep}
	}
	wide := make([]any, 0, bounds.MaxPredicateWidth+1)
	for i := 0; i <= bounds.MaxPredicateWidth; i++ {
		wide = append(wide, map[string]any{"field": map[string]any{"id": "amount", "exact": map[string]any{"type": "null"}}})
	}
	manyFields := make([]string, 0, bounds.MaxProjectedFields+1)
	for i := 0; i <= bounds.MaxProjectedFields; i++ {
		manyFields = append(manyFields, "amount")
	}
	sorts := make([]any, 0, bounds.MaxSortTerms+1)
	for i := 0; i <= bounds.MaxSortTerms; i++ {
		sorts = append(sorts, map[string]any{"field_id": "amount", "ascending": true})
	}
	for _, exceeded := range []struct {
		name string
		body map[string]any
		code string
	}{
		{"overall limit", map[string]any{"query": map[string]any{}, "limit": bounds.MaxOverallLimit + 1}, "query_result_limit_exceeded"},
		{"predicate depth", wrapped(map[string]any{"filter": deep}), "invalid_filter"},
		{"predicate width", wrapped(map[string]any{"filter": map[string]any{"and": wide}}), "invalid_filter"},
		{"projected fields", wrapped(map[string]any{"fields": manyFields}), "query_result_limit_exceeded"},
		{"sort terms", wrapped(map[string]any{"result_sort": sorts}), "query_result_limit_exceeded"},
	} {
		// A server with a small max_request_bytes refuses some of these on
		// size before it can see the bound under test, which is itself
		// correct. Only send what this server can actually receive.
		encoded, err := p.MarshalWire(exceeded.body)
		if err != nil {
			return err
		}
		if len(encoded) > bounds.MaxRequestBytes {
			continue
		}
		if err = problem("POST", base+"/objects/"+typeID+":query", f.Token, exceeded.body, p.ErrorStatus(exceeded.code), exceeded.code); err != nil {
			return fmt.Errorf("%s: %w", exceeded.name, err)
		}
	}
	// A transaction longer than the advertised bound is refused with a reason,
	// before any of it runs. The bound was enforced before it was declared,
	// and a caller exceeding it got a bare invalid_field naming nothing.
	if bounds.MaxTransactionOperations < 1 {
		return fmt.Errorf("the dataset declares no max_transaction_operations")
	}
	countOf := func(base, typeID string) (int, error) {
		var counted struct {
			Count int `json:"count"`
		}
		err := call("POST", base+"/objects/"+typeID+":count", f.Token, map[string]any{}, 200, &counted)
		return counted.Count, err
	}
	operations := make([]map[string]any, bounds.MaxTransactionOperations+1)
	for i := range operations {
		operations[i] = map[string]any{"op": "insert", "type": typeID, "fields": map[string]any{}}
	}
	long := map[string]any{"operations": operations}
	if encoded, err := p.MarshalWire(long); err != nil {
		return err
	} else if len(encoded) <= bounds.MaxRequestBytes {
		before, err := countOf(base, typeID)
		if err != nil {
			return err
		}
		if err = problem("POST", base+":transact", f.Token, long, p.ErrorStatus("query_result_limit_exceeded"), "query_result_limit_exceeded"); err != nil {
			return fmt.Errorf("transaction length: %w", err)
		}
		after, err := countOf(base, typeID)
		if err != nil {
			return err
		}
		if after != before {
			return fmt.Errorf("a transaction refused for its length still wrote")
		}
	}
	// A request beyond the advertised body size fails at the transport, before
	// anything in it is interpreted.
	oversized := map[string]any{"query": map[string]any{"filter": map[string]any{
		"text_contains": map[string]any{"field_id": "note", "text": strings.Repeat("x", bounds.MaxRequestBytes)}}}}
	if err := problem("POST", base+"/objects/"+typeID+":query", f.Token, oversized, 413, "query_result_limit_exceeded"); err != nil {
		return fmt.Errorf("request size: %w", err)
	}

	// The guard still matches after those reads, which is the failure the rule
	// exists to prevent rather than a restatement of it.
	guard := map[string]any{"operations": []any{map[string]any{"op": "expect_type", "type": typeID, "schema_version": gotMeta.Type.SchemaVersion}}}
	if err := call("POST", base+":transact", f.Token, guard, 200, nil); err != nil {
		return err
	}
	integer, _ := p.NewValue(p.IntegerType, int64(9007199254740993))
	changed, _ := p.NewValue(p.IntegerType, int64(7))
	var inserted struct {
		Objects   []p.Identity `json:"objects"`
		Watermark string       `json:"watermark"`
	}
	if err := call("POST", base+"/objects/"+typeID, f.Token, map[string]any{"fields": map[string]p.Value{"amount": integer}}, 201, &inserted); err != nil {
		return err
	}
	if len(inserted.Objects) != 1 || inserted.Objects[0].MetaID != typeID || inserted.Watermark == "" || strings.HasPrefix(inserted.Objects[0].ID, "$") {
		return fmt.Errorf("insert identity or watermark missing")
	}
	id := inserted.Objects[0].ID
	path := base + "/objects/" + typeID + "/" + url.PathEscape(id)
	var record p.RecordSet
	if err := call("GET", path+"?fields=note,amount", f.Token, nil, 200, &record); err != nil {
		return err
	}
	if record.Watermark == "" || len(record.Meta.Fields) != 2 || len(record.Records) != 1 || len(record.Records[0].Values) != 2 {
		return fmt.Errorf("invalid projected shape")
	}
	if record.Records[0].Version == "" {
		return fmt.Errorf("single-object version missing")
	}
	// Resolve through response metadata: request order is not normative.
	projected := map[string]p.Value{}
	for i, field := range record.Meta.Fields {
		projected[field.ID] = record.Records[0].Values[i]
	}
	amount, hasAmount := projected["amount"]
	note, hasNote := projected["note"]
	equal, err := p.Equal(amount, integer)
	if err != nil || !equal || !hasAmount || !hasNote || note.Type() != p.NullType {
		return fmt.Errorf("projection mapping, null or exact integer failed")
	}
	if err := problem("GET", path+"?fields=amount,amount", f.Token, nil, 400, "invalid_field"); err != nil {
		return err
	}
	if err := call("PATCH", path, f.Token, map[string]any{"fields": map[string]any{}}, 200, nil); err != nil {
		return err
	}
	updates := map[string]any{"operations": []any{map[string]any{"op": "expect", "target": p.Target{Type: typeID, ID: id}, "fields": map[string]p.Value{"amount": integer}}, map[string]any{"op": "update", "target": p.Target{Type: typeID, ID: id}, "fields": map[string]p.Value{"amount": changed}}}}
	if err := call("POST", base+":transact", f.Token, updates, 200, nil); err != nil {
		return err
	}
	if err := problem("POST", base+":transact", f.Token, updates, 409, "conflict"); err != nil {
		return err
	}
	if err := problem("PATCH", path, f.Token, map[string]any{"fields": map[string]p.Value{"unknown": changed}}, 400, "invalid_field"); err != nil {
		return err
	}
	if err := call("GET", path+"?fields=amount", f.Token, nil, 200, &record); err != nil {
		return err
	}
	if len(record.Records) != 1 || len(record.Records[0].Values) != 1 {
		return fmt.Errorf("invalid updated shape")
	}
	equal, err = p.Equal(record.Records[0].Values[0], changed)
	if err != nil || !equal {
		return fmt.Errorf("failed write changed stored value")
	}
	baselineWatermark := record.Watermark
	baselineVersion := record.Records[0].Version
	var observed struct {
		Watermark string `json:"watermark"`
	}
	if err := call("PATCH", path, f.Token, map[string]any{"fields": map[string]p.Value{"amount": changed}}, 200, &observed); err != nil {
		return err
	}
	if observed.Watermark != baselineWatermark {
		return fmt.Errorf("same-value update advanced watermark")
	}

	rollback := map[string]any{"operations": []any{
		map[string]any{"op": "update", "target": p.Target{Type: typeID, ID: id}, "fields": map[string]p.Value{"amount": integer}},
		map[string]any{"op": "expect", "target": p.Target{Type: typeID, ID: id}, "fields": map[string]p.Value{"amount": changed}},
	}}
	var failed struct {
		Error   p.Error         `json:"error"`
		Results json.RawMessage `json:"results"`
	}
	if err := call("POST", base+":transact", f.Token, rollback, 409, &failed); err != nil {
		return err
	}
	if failed.Error.Code != "conflict" || failed.Error.OperationIndex == nil || *failed.Error.OperationIndex != 1 || failed.Results != nil {
		return fmt.Errorf("transaction failure does not identify its first failing operation")
	}
	if err := call("GET", path+"?fields=amount", f.Token, nil, 200, &record); err != nil {
		return err
	}
	if len(record.Records) != 1 || record.Watermark != baselineWatermark || record.Records[0].Version != baselineVersion {
		return fmt.Errorf("failed transaction changed values or watermark")
	}
	var queried p.RecordSet
	for _, include := range []bool{false, true} {
		if err := call("POST", base+"/objects/"+typeID+":query", f.Token, map[string]any{"query": map[string]any{"fields": []string{}, "include_versions": include}}, 200, &queried); err != nil {
			return err
		}
		if len(queried.Records) != 1 || len(queried.Meta.Fields) != 0 || queried.Watermark != baselineWatermark {
			return fmt.Errorf("query projection or watermark failed")
		}
		if include && queried.Records[0].Version != baselineVersion || !include && queried.Records[0].Version != "" {
			return fmt.Errorf("query version opt-in failed")
		}
	}
	if err := call("DELETE", path, f.Token, nil, 200, nil); err != nil {
		return err
	}
	if err := problem("GET", path, f.Token, nil, 404, "not_found"); err != nil {
		return err
	}
	if err := call("POST", base+"/objects/"+typeID, f.Token, map[string]any{"fields": map[string]any{}}, 201, &inserted); err != nil {
		return err
	}
	if len(inserted.Objects) != 1 || inserted.Objects[0].ID == id {
		return fmt.Errorf("deleted identity reused")
	}

	// Schema result shapes, confirmation and error grammar are shared by both
	// SQL engines and the independent HTTP consumer, not merely unit assertions.
	childID := typeID + ".child"
	var schemaResult p.TransactionResult
	if err := call("POST", base+":transact", f.Token, map[string]any{"operations": []any{map[string]any{"op": "define_type", "meta": p.Meta{ID: childID, ParentID: &typeID, Fields: []p.Field{}}}}}, 200, &schemaResult); err != nil {
		return err
	}
	if len(schemaResult.Results) != 1 || schemaResult.Results[0].Type == nil || len(schemaResult.Results[0].Type.Fields) != 2 || string(schemaResult.Results[0].Type.Annotations["opaque"]) != opaque {
		return fmt.Errorf("schema result is not effective metadata")
	}
	reduced := meta
	reduced.Fields = []p.Field{}
	var rejected struct {
		Error     p.Error `json:"error"`
		Watermark string  `json:"watermark"`
	}
	updateType := map[string]any{"op": "update_type", "meta": reduced}
	if err := call("POST", base+":transact", f.Token, map[string]any{"operations": []any{updateType}}, 400, &rejected); err != nil {
		return err
	}
	if rejected.Error.Code != "meta_schema_error" || rejected.Error.Details["reason"] != "confirmation_required" || rejected.Watermark != "" {
		return fmt.Errorf("field removal lacks confirmation error")
	}
	if err := call("POST", base+":transact", f.Token, map[string]any{"operations": []any{map[string]any{"op": "delete_type", "type": typeID}}}, 409, &rejected); err != nil {
		return err
	}
	if rejected.Error.Details["reason"] != "type_not_empty" {
		return fmt.Errorf("nonempty deletion lacks reason")
	}
	updateType["confirm"] = typeID
	if err := call("POST", base+":transact", f.Token, map[string]any{"operations": []any{updateType}}, 200, &schemaResult); err != nil {
		return err
	}
	if len(schemaResult.Results) != 1 || schemaResult.Results[0].Type == nil || len(schemaResult.Results[0].Type.Fields) != 0 {
		return fmt.Errorf("confirmed schema result incorrect")
	}
	beforeInvalid := schemaResult.Watermark
	for _, op := range []any{
		map[string]any{"op": "insert", "type": typeID, "fields": map[string]any{}, "typo": true},
		map[string]any{"op": "delete", "target": p.Target{Type: typeID, ID: "$missing"}},
	} {
		// The first operation must not run when the second has invalid grammar.
		if err := call("POST", base+":transact", f.Token, map[string]any{"operations": []any{map[string]any{"op": "insert", "type": typeID, "fields": map[string]any{}}, op}}, 400, &rejected); err != nil {
			return err
		}
		if rejected.Error.Code != "invalid_request" || rejected.Error.OperationIndex == nil || *rejected.Error.OperationIndex != 1 {
			return fmt.Errorf("grammar rejection lacks operation index")
		}
	}
	var unchanged p.RecordSet
	if err := call("POST", base+"/objects/"+typeID+":query", f.Token, map[string]any{"query": map[string]any{}}, 200, &unchanged); err != nil {
		return err
	}
	if unchanged.Watermark != beforeInvalid {
		return fmt.Errorf("invalid grammar executed earlier operation")
	}
	if err := call("POST", base+":transact", f.Token, map[string]any{"operations": []any{map[string]any{"op": "delete_type", "type": childID}}}, 200, &schemaResult); err != nil {
		return err
	}
	if len(schemaResult.Results) != 1 || len(schemaResult.Results[0].DeletedTypes) != 1 || schemaResult.Results[0].DeletedObjects == nil || *schemaResult.Results[0].DeletedObjects != 0 {
		return fmt.Errorf("type deletion result shape")
	}
	return nil
}

// Every authorization server a deployment advertises is one a client is told
// to go to, so a plaintext entry is this server sending somebody's credentials
// over the clear by its own say-so. The client-side rule — refuse it — is a
// client obligation this suite cannot test. This half can be, and it is the
// only mechanical half there is: a conformant server never offers the entry in
// the first place.
//
// The consumer that found this had followed such an entry before validating
// the metadata it fetched, which is checking after the thing you meant to
// prevent has happened.
func checkAuthorizationServersAreReachableSafely(raw json.RawMessage) error {
	var entries []string
	if len(raw) == 0 || json.Unmarshal(raw, &entries) != nil {
		return nil // absence and shape are already reported above
	}
	for _, entry := range entries {
		parsed, err := url.Parse(entry)
		if err != nil {
			return fmt.Errorf("an authorization_servers entry is not a URL")
		}
		// Case-insensitively, because a scheme is case-insensitive and an
		// exact comparison against "http" admits "HTTP".
		if strings.EqualFold(parsed.Scheme, "https") {
			continue
		}
		if strings.EqualFold(parsed.Scheme, "http") && isLoopback(parsed.Hostname()) {
			continue
		}
		return fmt.Errorf("discovery advertises an authorization server a conformant client must refuse: scheme %q", parsed.Scheme)
	}
	return nil
}

// isLoopback answers for the address rather than for its spelling. A host this
// rejects is one a client refuses, so being wrong here is an outage rather
// than a hole — which is why it parses instead of matching literals.
func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}
