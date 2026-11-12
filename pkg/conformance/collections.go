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
	"slices"
	"time"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// CollectionFixture addresses a disposable dataset on a durable server.
// Set replaces a named fixture principal's grants and returns its stable token.
type CollectionFixture struct {
	URL, Owner string
	Client     *http.Client
	Set        func(context.Context, string, []store.CollectionScope, []store.CollectionScope, *time.Time) (string, error)
}

// CheckCollections verifies collection_access through the wire protocol. The
// fixture's trusted host supplies grants, which are outside the record protocol.
func CheckCollections(ctx context.Context, f CollectionFixture) error {
	client := f.Client
	if client == nil {
		client = http.DefaultClient
	}
	callWithHeaders := func(method, path, token string, body any, headers map[string]string, status int, dest any) error {
		var payload []byte
		if body != nil {
			var err error
			payload, err = p.MarshalWire(body)
			if err != nil {
				return err
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, f.URL+path, bytes.NewReader(payload))
		if err != nil {
			return fmt.Errorf("invalid collection fixture request")
		}
		req.Header.Set("Authorization", "Bearer "+token)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		for name, value := range headers {
			req.Header.Set(name, value)
		}
		res, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("collection fixture request failed")
		}
		defer res.Body.Close()
		if res.StatusCode != status {
			return fmt.Errorf("collection fixture %s expected HTTP %d, received %d", method, status, res.StatusCode)
		}
		if dest != nil && json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(dest) != nil {
			return fmt.Errorf("invalid collection fixture response")
		}
		return nil
	}
	call := func(method, path, token string, body any, status int, dest any) error {
		return callWithHeaders(method, path, token, body, nil, status, dest)
	}
	problem := func(method, path, token string, body any, status int, code string) error {
		var result struct{ Error p.Error }
		if err := call(method, path, token, body, status, &result); err != nil {
			return err
		}
		if result.Error.Code != code {
			return fmt.Errorf("expected collection refusal %s, received %s", code, result.Error.Code)
		}
		return nil
	}
	var listing struct {
		Datasets []struct {
			ID           string
			Capabilities []string
		}
	}
	if err := call("GET", "/v1/datasets", f.Owner, nil, 200, &listing); err != nil {
		return err
	}
	if len(listing.Datasets) != 1 {
		return fmt.Errorf("collection fixture needs one dataset")
	}
	if !slices.Contains(listing.Datasets[0].Capabilities, p.CollectionAccessCapability) {
		return fmt.Errorf("collection fixture must advertise collection_access")
	}
	if f.Set == nil {
		return fmt.Errorf("collection fixture must supply Set")
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	base := "/v1/datasets/" + url.PathEscape(listing.Datasets[0].ID)
	typ := "test.collections.t" + hex.EncodeToString(nonce[:])
	child := typ + ".child"
	objects := base + "/objects/" + typ
	schema := p.Meta{ID: typ, Fields: []p.Field{
		{ID: "membership", MetaID: typ, Type: p.TextType, Collection: true},
		{ID: "rank", MetaID: typ, Type: p.IntegerType},
	}}
	for _, m := range []p.Meta{schema, {ID: child, ParentID: &typ}} {
		if err := call("POST", base+"/meta", f.Owner, map[string]any{"type": m}, 201, nil); err != nil {
			return err
		}
	}
	fields := func(id string, rank int64) map[string]p.Value {
		number, _ := p.NewValue(p.IntegerType, rank)
		values := map[string]p.Value{"rank": number}
		if id != "" {
			values["membership"], _ = p.NewValue(p.TextType, id)
		}
		return values
	}
	insert := func(token, id string, rank int64) (string, error) {
		var result struct{ Objects []p.Identity }
		if err := call("POST", objects, token, map[string]any{"fields": fields(id, rank)}, 201, &result); err != nil {
			return "", err
		}
		if len(result.Objects) != 1 {
			return "", fmt.Errorf("collection insert must return one identity")
		}
		return result.Objects[0].ID, nil
	}
	visible, err := insert(f.Owner, "alpha", 10)
	if err != nil {
		return err
	}
	hidden, err := insert(f.Owner, "beta", 99)
	if err != nil {
		return err
	}
	unassigned, err := insert(f.Owner, "", 50)
	if err != nil {
		return err
	}
	alpha := []store.CollectionScope{{TypeID: typ, ID: "alpha"}}
	beta := []store.CollectionScope{{TypeID: typ, ID: "beta"}}
	reader, err := f.Set(ctx, "reader", alpha, nil, nil)
	if err != nil {
		return err
	}
	contributor, err := f.Set(ctx, "contributor", nil, alpha, nil)
	if err != nil {
		return err
	}
	if reader == "" || contributor == "" || reader == contributor || reader == f.Owner || contributor == f.Owner {
		return fmt.Errorf("fixture must supply distinct owner, reader and contributor credentials")
	}
	for _, id := range []string{hidden, unassigned, "999999999"} {
		if err = problem("GET", objects+"/"+url.PathEscape(id), reader, nil, 404, "unavailable"); err != nil {
			return err
		}
	}
	if err = call("GET", objects+"/"+url.PathEscape(visible), reader, nil, 200, nil); err != nil {
		return err
	}
	if err = call("GET", base+"/meta/"+typ, contributor, nil, 200, nil); err != nil {
		return err
	}
	for _, id := range []string{"beta", ""} {
		if err = problem("POST", objects, contributor, map[string]any{"fields": fields(id, 2)}, 403, "permission_denied"); err != nil {
			return err
		}
	}
	if err = problem("POST", base+"/objects/"+child, contributor, map[string]any{"fields": fields("alpha", 3)}, 403, "permission_denied"); err != nil {
		return err
	}
	if err = problem("POST", objects, reader, map[string]any{"fields": fields("alpha", 3)}, 403, "permission_denied"); err != nil {
		return err
	}

	batch := map[string]any{"operations": []map[string]any{
		{"op": "insert", "type": typ, "fields": fields("alpha", 5)},
		{"op": "insert", "type": typ, "fields": fields("beta", 5)},
	}}
	if err = problem("POST", base+":transact", contributor, batch, 403, "permission_denied"); err != nil {
		return err
	}
	grown, err := insert(contributor, "alpha", 20)
	if err != nil {
		return err
	}
	if err = problem("GET", objects+"/"+url.PathEscape(grown), contributor, nil, 403, "permission_denied"); err != nil {
		return err
	}
	if err = problem("DELETE", objects+"/"+url.PathEscape(grown), contributor, nil, 403, "permission_denied"); err != nil {
		return err
	}
	if err = problem("PATCH", objects+"/"+url.PathEscape(grown), f.Owner, map[string]any{"fields": fields("beta", 20)}, 400, "invalid_field"); err != nil {
		return err
	}
	schema.Fields[0].Collection = false
	if err = problem("PUT", base+"/meta/"+typ, f.Owner, map[string]any{"type": schema}, 400, "meta_schema_error"); err != nil {
		return err
	}

	query := map[string]any{"query": map[string]any{
		"result_sort": []map[string]any{{"field_id": "rank", "ascending": false}},
	}}
	var records p.RecordSet
	if err = call("POST", objects+":query", reader, query, 200, &records); err != nil {
		return err
	}
	if len(records.Records) != 2 || records.Records[0].ID.ID != grown || records.Records[1].ID.ID != visible {
		return fmt.Errorf("sorted collection query returned an incorrect view")
	}
	var count struct{ Count int }
	if err = call("POST", objects+":count", reader, map[string]any{}, 200, &count); err != nil {
		return err
	}
	if count.Count != 2 {
		return fmt.Errorf("collection count crossed the view")
	}
	var grouped struct {
		Rows []aggregateRow `json:"aggregates"`
	}
	if err = call("POST", objects+":aggregate", reader, map[string]any{"field": "rank", "group_by": "membership"}, 200, &grouped); err != nil {
		return err
	}
	if len(grouped.Rows) != 1 || grouped.Rows[0].RowCount != 2 || string(grouped.Rows[0].Sum) != "30" {
		return fmt.Errorf("collection aggregation crossed the view")
	}
	query["page_size"] = 1
	var start store.PageStart
	if err = call("POST", objects+":page", reader, query, 200, &start); err != nil {
		return err
	}
	var page struct {
		Records []p.Record
		Next    string `json:"next_cursor"`
	}
	pagePath := func(cursor string) string { return base + "/pages/" + url.PathEscape(cursor) + ":next" }
	if start.Cursor == "" {
		return fmt.Errorf("collection page has no cursor")
	}
	if err = call("POST", pagePath(start.Cursor), reader, nil, 200, &page); err != nil {
		return err
	}
	if len(page.Records) != 1 || page.Records[0].ID.ID != grown || page.Next == "" {
		return fmt.Errorf("collection page did not return the first visible row")
	}
	replacement, err := f.Set(ctx, "reader", beta, nil, nil)
	if err != nil {
		return err
	}
	if replacement != reader {
		return fmt.Errorf("fixture changed the reader token on scope replacement")
	}
	if err = problem("POST", pagePath(page.Next), reader, nil, 410, "view_changed"); err != nil {
		return err
	}
	if err = call("GET", objects+"/"+url.PathEscape(hidden), reader, nil, 200, nil); err != nil {
		return err
	}
	if err = problem("GET", objects+"/"+url.PathEscape(visible), reader, nil, 404, "unavailable"); err != nil {
		return err
	}
	if _, err = f.Set(ctx, "contributor", nil, beta, nil); err != nil {
		return err
	}
	if err = problem("POST", objects, contributor, map[string]any{"fields": fields("alpha", 30)}, 403, "permission_denied"); err != nil {
		return err
	}
	if _, err = f.Set(ctx, "reader", beta, alpha, nil); err != nil {
		return err
	}
	combined, err := insert(reader, "alpha", 30)
	if err != nil {
		return err
	}
	if err = problem("GET", objects+"/"+url.PathEscape(combined), reader, nil, 404, "unavailable"); err != nil {
		return err
	}
	if _, err = f.Set(ctx, "reader", alpha, alpha, nil); err != nil {
		return err
	}
	operation := "53c49d33-8f35-439e-836b-" + hex.EncodeToString(nonce[:6])
	request := map[string]any{"fields": fields("alpha", 40)}
	headers := map[string]string{"Idempotency-Key": operation}
	var original json.RawMessage
	if err = callWithHeaders("POST", objects, reader, request, headers, 201, &original); err != nil {
		return err
	}
	var response struct {
		Objects []p.Identity
	}
	if json.Unmarshal(original, &response) != nil || len(response.Objects) != 1 {
		return fmt.Errorf("a keyed collection insert returned no identity")
	}
	status := base + "/operations/" + operation + ":status"
	var receipt store.Receipt
	if err = call("POST", status, reader, nil, 200, &receipt); err != nil {
		return err
	}
	if receipt.ResultWithheld || !bytes.Equal(receipt.Result, original) {
		return fmt.Errorf("collection recovery withheld a currently readable representation")
	}
	var replayed json.RawMessage
	if err = callWithHeaders("POST", objects, reader, request, headers, 201, &replayed); err != nil {
		return err
	}
	if !bytes.Equal(replayed, original) {
		return fmt.Errorf("collection replay changed the original result")
	}
	if _, err = f.Set(ctx, "reader", beta, alpha, nil); err != nil {
		return err
	}
	receipt = store.Receipt{}
	if err = call("POST", status, reader, nil, 200, &receipt); err != nil {
		return err
	}
	if !receipt.ResultWithheld || len(receipt.Result) != 0 || len(receipt.Objects) != 1 ||
		receipt.Objects[0].ObjectID != response.Objects[0].ID {
		return fmt.Errorf("collection recovery disclosed a withdrawn representation or lost its identity")
	}
	expired := time.Now().Add(-time.Minute)
	if _, err = f.Set(ctx, "reader", beta, nil, &expired); err != nil {
		return err
	}
	return problem("GET", objects+"/"+url.PathEscape(hidden), reader, nil, 404, "unavailable")
}
