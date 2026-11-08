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

	p "github.com/ekarpov/tessera/pkg/protocol"
)

// CheckSharing exercises the optional sharing capability: read-only guests,
// exact concrete type grants, and the difference between a record that is
// absent and one that is merely outside the caller's view.
//
// A server that does not advertise sharing passes trivially. One that does
// must supply CoreFixture.Share, because grant administration is outside the
// protocol and without it the capability has no evidence behind it.
func CheckSharing(ctx context.Context, f CoreFixture) error {
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
		request, err := http.NewRequestWithContext(ctx, method, f.URL+path, bytes.NewReader(data))
		if err != nil {
			return err
		}
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		if body != nil {
			request.Header.Set("Content-Type", "application/json")
		}
		response, err := client.Do(request)
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

	var listing struct {
		Datasets []struct {
			ID           string   `json:"id"`
			Capabilities []string `json:"capabilities"`
		} `json:"datasets"`
	}
	if err := call("GET", "/v1/datasets", f.Token, nil, 200, &listing); err != nil {
		return err
	}
	if len(listing.Datasets) != 1 {
		return fmt.Errorf("fixture needs exactly one dataset")
	}
	shares := false
	for _, name := range listing.Datasets[0].Capabilities {
		shares = shares || name == "sharing"
	}
	if !shares {
		return nil
	}
	if f.Share == nil {
		return fmt.Errorf("this dataset advertises sharing, so the fixture must supply Share")
	}
	if f.Contribute == nil {
		return fmt.Errorf("this dataset advertises sharing, so the fixture must supply Contribute")
	}
	base := "/v1/datasets/" + url.PathEscape(listing.Datasets[0].ID)

	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	shared := "test.shared.t" + hex.EncodeToString(nonce[:])
	private := shared + ".child"
	for _, meta := range []p.Meta{
		{ID: shared, Name: "Shared", Fields: []p.Field{{ID: "note", MetaID: shared, Name: "Note", Type: p.TextType}}},
		{ID: private, ParentID: &shared, Name: "Child", Fields: []p.Field{{ID: "extra", MetaID: private, Name: "Extra", Type: p.TextType}}},
	} {
		if err := call("POST", base+"/meta", f.Token, map[string]any{"type": meta}, 201, nil); err != nil {
			return err
		}
	}
	text, _ := p.NewValue(p.TextType, "visible")
	var inserted struct {
		Objects []p.Identity `json:"objects"`
	}
	if err := call("POST", base+"/objects/"+shared, f.Token, map[string]any{"fields": map[string]p.Value{"note": text}}, 201, &inserted); err != nil {
		return err
	}
	if len(inserted.Objects) != 1 {
		return fmt.Errorf("shared insert returned no identity")
	}
	visible := inserted.Objects[0].ID
	if err := call("POST", base+"/objects/"+private, f.Token, map[string]any{"fields": map[string]p.Value{"extra": text}}, 201, nil); err != nil {
		return err
	}

	guest, err := f.Share(ctx, []string{shared})
	if err != nil {
		return fmt.Errorf("the fixture could not install a guest grant")
	}
	if guest == "" || guest == f.Token {
		return fmt.Errorf("Share must return a distinct guest token")
	}

	// The guest reads what it was granted.
	var result p.RecordSet
	if err = call("POST", base+"/objects/"+shared+":query", guest, map[string]any{"query": map[string]any{}}, 200, &result); err != nil {
		return err
	}
	if len(result.Records) != 1 {
		return fmt.Errorf("a granted type returned %d records rather than its own one", len(result.Records))
	}
	if err = call("GET", base+"/objects/"+shared+"/"+url.PathEscape(visible), guest, nil, 200, nil); err != nil {
		return err
	}

	// A type grant is exact. The subtype is neither queryable nor disclosed as
	// existing, and a descendant-scoped query does not reach it either.
	if err = problem("POST", base+"/objects/"+private+":query", guest, map[string]any{"query": map[string]any{}}, 403, "permission_denied"); err != nil {
		return err
	}
	var descendants p.RecordSet
	if err = call("POST", base+"/objects/"+shared+":query", guest, map[string]any{"query": map[string]any{
		"type_scope": map[string]any{"kind": "descendants"}}}, 200, &descendants); err != nil {
		return err
	}
	if len(descendants.Records) != 1 {
		return fmt.Errorf("a descendant-scoped query crossed an exact type grant")
	}
	// Counting must respect the same intersection, or it discloses membership
	// without returning a row.
	var counted struct {
		Count int `json:"count"`
	}
	if err = call("POST", base+"/objects/"+shared+":count", guest, map[string]any{}, 200, &counted); err != nil {
		return err
	}
	if counted.Count != 1 {
		return fmt.Errorf("count reported %d rows to a guest granted one type", counted.Count)
	}

	// A record outside the view is unavailable, never not_found: telling its
	// owner "not found" would say the data had been deleted.
	if err = problem("GET", base+"/objects/"+private+"/1", guest, nil, 404, "unavailable"); err != nil {
		return err
	}
	// And a genuinely absent record inside the granted type is absent.
	if err = problem("GET", base+"/objects/"+shared+"/999999999", guest, nil, 404, "not_found"); err != nil {
		return err
	}

	// Guests are read-only, through every route that writes.
	for _, write := range []struct {
		method, path string
		body         any
	}{
		{"POST", base + "/objects/" + shared, map[string]any{"fields": map[string]p.Value{"note": text}}},
		{"PATCH", base + "/objects/" + shared + "/" + url.PathEscape(visible), map[string]any{"fields": map[string]p.Value{"note": text}}},
		{"DELETE", base + "/objects/" + shared + "/" + url.PathEscape(visible), nil},
		{"POST", base + "/meta", map[string]any{"type": p.Meta{ID: shared + ".guest", Fields: []p.Field{}}}},
		{"POST", base + ":transact", map[string]any{"operations": []any{map[string]any{
			"op": "delete_where", "type": shared}}}},
	} {
		if err = problem(write.method, write.path, guest, write.body, 403, "permission_denied"); err != nil {
			return fmt.Errorf("a guest reached a write: %w", err)
		}
	}
	return checkContribution(ctx, f, client, base, shared, private, call, problem)
}

// checkContribution holds an insert grant to what the specification says it
// is: it adds records of exactly the named type, and conveys nothing else — no
// read of the type, no hold on what it added, not even of its own record.
func checkContribution(ctx context.Context, f CoreFixture, client *http.Client, base, shared, private string, call caller, problem problemCaller) error {
	contributor, err := f.Contribute(ctx, []string{shared})
	if err != nil {
		return fmt.Errorf("the fixture could not install an insert grant")
	}
	if contributor == "" || contributor == f.Token {
		return fmt.Errorf("Contribute must return a distinct token")
	}
	var listing struct {
		Datasets []struct {
			Access map[string]any `json:"access"`
		} `json:"datasets"`
	}
	if err = call("GET", "/v1/datasets", contributor, nil, 200, &listing); err != nil {
		return err
	}
	if len(listing.Datasets) != 1 || listing.Datasets[0].Access["insert"] != true || listing.Datasets[0].Access["read"] != false || listing.Datasets[0].Access["write"] != false {
		return fmt.Errorf("a contributor's listing does not report insert alone")
	}
	// The schema of what it adds to, and nothing else of the dataset.
	if err = call("GET", base+"/meta/"+shared, contributor, nil, 200, nil); err != nil {
		return fmt.Errorf("a contributor could not read the schema it writes against: %w", err)
	}
	if err = problem("GET", base+"/meta/"+private, contributor, nil, 404, "unavailable"); err != nil {
		return fmt.Errorf("a contributor read the schema of a type it cannot reach: %w", err)
	}

	// It adds. A representation is declined, because a caller that cannot
	// read the row is not handed it, and the declining is said by leaving
	// the header out.
	text, _ := p.NewValue(p.TextType, "contributed")
	body, _ := p.MarshalWire(map[string]any{"fields": map[string]p.Value{"note": text}})
	request, err := http.NewRequestWithContext(ctx, "POST", f.URL+base+"/objects/"+shared, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+contributor)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Prefer", "return=representation")
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("HTTP request failed")
	}
	var added struct {
		Objects   []p.Identity    `json:"objects"`
		RecordSet json.RawMessage `json:"record_set"`
	}
	decodeErr := json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&added)
	response.Body.Close()
	if response.StatusCode != 201 || decodeErr != nil || len(added.Objects) != 1 {
		return fmt.Errorf("a contributor could not add to its type")
	}
	if added.RecordSet != nil || response.Header.Get("Preference-Applied") != "" {
		return fmt.Errorf("a contributor was handed a representation of a row it cannot read")
	}
	mine := base + "/objects/" + shared + "/" + url.PathEscape(added.Objects[0].ID)

	// And holds nothing afterwards, not even what it added. Holding no read
	// at all, it is refused reading as an operation — the same answer for
	// its own record as for any identity, so the refusal says nothing.
	for _, refused := range []struct {
		method, path string
		body         any
	}{
		{"GET", mine, nil},
		{"PATCH", mine, map[string]any{"fields": map[string]p.Value{"note": text}}},
		{"DELETE", mine, nil},
		{"POST", base + "/objects/" + shared + ":query", map[string]any{"query": map[string]any{}}},
		{"POST", base + "/objects/" + private, map[string]any{"fields": map[string]p.Value{"extra": text}}},
		{"POST", base + ":transact", map[string]any{"operations": []any{
			map[string]any{"op": "insert", "type": shared, "fields": map[string]p.Value{"note": text}},
			map[string]any{"op": "delete_where", "type": shared}}}},
	} {
		if err = problem(refused.method, refused.path, contributor, refused.body, 403, "permission_denied"); err != nil {
			return fmt.Errorf("an insert grant reached beyond inserting: %s %w", refused.method, err)
		}
	}
	// The dataset holds it: the owner sees the contribution beside its own.
	if err = call("GET", mine, f.Token, nil, 200, nil); err != nil {
		return fmt.Errorf("the owner cannot read what was contributed: %w", err)
	}
	return nil
}
