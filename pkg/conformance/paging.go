package conformance

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"io"
	"net/http"
	"net/url"
)

// CheckPaging tests replay, complete traversal after a deletion, identity
// uniqueness and sequence release through the public HTTP surface.
func CheckPaging(ctx context.Context, f CoreFixture) error {
	client := f.Client
	if client == nil {
		client = http.DefaultClient
	}
	call := func(method, path string, body []byte, status int) ([]byte, error) {
		req, e := http.NewRequestWithContext(ctx, method, f.URL+path, bytes.NewReader(body))
		if e != nil {
			return nil, e
		}
		req.Header.Set("Authorization", "Bearer "+f.Token)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		res, e := client.Do(req)
		if e != nil {
			return nil, fmt.Errorf("paging request failed")
		}
		defer res.Body.Close()
		data, e := io.ReadAll(io.LimitReader(res.Body, 8<<20))
		if e != nil {
			return nil, e
		}
		if res.StatusCode != status {
			return nil, fmt.Errorf("paging expected HTTP %d, received %d", status, res.StatusCode)
		}
		return data, nil
	}
	data, err := call("GET", "/v1/datasets", nil, 200)
	if err != nil {
		return err
	}
	var listing struct {
		Datasets []struct {
			ID string `json:"id"`
		} `json:"datasets"`
	}
	if json.Unmarshal(data, &listing) != nil || len(listing.Datasets) != 1 {
		return fmt.Errorf("paging requires one dataset")
	}
	base := "/v1/datasets/" + url.PathEscape(listing.Datasets[0].ID)
	// A fresh identifier per run, so this suite does not meet its own leftovers
	// on a second pass against one dataset. It still leaves the type behind;
	// the fixture is disposable and this is not cleanup.
	var nonce [8]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return err
	}
	typ := "conformance.paging.t" + hex.EncodeToString(nonce[:])
	if _, err = call("POST", base+"/meta", []byte(`{"type":{"id":"`+typ+`","name":"Paging","fields":[]}}`), 201); err != nil {
		return err
	}
	for i := 0; i < 5; i++ {
		if _, err = call("POST", base+"/objects/"+typ, []byte(`{"fields":{}}`), 201); err != nil {
			return err
		}
	}
	data, err = call("POST", base+"/objects/"+typ+":page", []byte(`{"query":{},"page_size":2}`), 200)
	if err != nil {
		return err
	}
	var start struct {
		Cursor    string `json:"cursor"`
		Watermark string `json:"watermark"`
	}
	if json.Unmarshal(data, &start) != nil || start.Cursor == "" || start.Watermark == "" {
		return fmt.Errorf("invalid page start")
	}
	next := start.Cursor
	seen := map[string]bool{}
	first := true
	for next != "" {
		path := base + "/pages/" + url.PathEscape(next) + ":next"
		// A body where none is accepted is a malformed request, refused as one
		// and without spending the cursor — so the same continuation, sent
		// properly, still answers. The refusal used to be invalid_field, which
		// a client read as something missing from the dataset.
		if first {
			refused, e := call("POST", path, []byte(`{}`), 400)
			if e != nil {
				return fmt.Errorf("a body sent to continue a page was not refused as malformed: %w", e)
			}
			var problem struct {
				Error struct {
					Code    string         `json:"code"`
					Details map[string]any `json:"details"`
				} `json:"error"`
			}
			if json.Unmarshal(refused, &problem) != nil || problem.Error.Code != "invalid_request" || problem.Error.Details["reason"] != "unexpected_body" {
				return fmt.Errorf("a body sent to continue a page was refused with %s, not invalid_request and unexpected_body", refused)
			}
		}
		raw, e := call("POST", path, nil, 200)
		if e != nil {
			return e
		}
		var page struct {
			Meta      p.Meta     `json:"meta"`
			Records   []p.Record `json:"records"`
			Watermark string     `json:"watermark"`
			Next      *string    `json:"next_cursor"`
		}
		if json.Unmarshal(raw, &page) != nil || page.Watermark == "" {
			return fmt.Errorf("invalid page")
		}
		if first {
			if len(page.Records) != 2 {
				return fmt.Errorf("page size not respected")
			}
			if _, e = call("DELETE", base+"/objects/"+typ+"/"+url.PathEscape(page.Records[0].ID.ID), nil, 200); e != nil {
				return e
			}
			first = false
		}
		again, e := call("POST", path, nil, 200)
		if e != nil {
			return e
		}
		if !bytes.Equal(raw, again) {
			return fmt.Errorf("page replay changed")
		}
		for _, row := range page.Records {
			if seen[row.ID.ID] {
				return fmt.Errorf("duplicate paged identity")
			}
			seen[row.ID.ID] = true
		}
		next = ""
		if page.Next != nil {
			next = *page.Next
		}
	}
	if len(seen) != 5 {
		return fmt.Errorf("stable rows skipped after deletion")
	}
	if _, err = call("DELETE", base+"/pages/"+url.PathEscape(start.Cursor), nil, 204); err != nil {
		return err
	}
	if _, err = call("POST", base+"/pages/"+url.PathEscape(start.Cursor)+":next", nil, 410); err != nil {
		return err
	}
	return checkSortedPaging(call, base, typ+".sorted")
}

// checkSortedPaging holds a sorted sequence to the two guarantees while its
// rows move: a row already returned is not returned again even when its sort
// value moves ahead of where the sequence has reached, and a row that stays
// put is returned once however the others change. A server that snapshots
// passes without trying; one that continues by key passes only by excluding
// what it returned and by resuming strictly after its position.
func checkSortedPaging(call func(method, path string, body []byte, status int) ([]byte, error), base, typ string) error {
	if _, err := call("POST", base+"/meta", []byte(`{"type":{"id":"`+typ+`","name":"Sorted paging","fields":[{"id":"n","meta_id":"`+typ+`","name":"N","type":"integer"}]}}`), 201); err != nil {
		return err
	}
	ids := make([]string, 6)
	for n := range ids {
		raw, err := call("POST", base+"/objects/"+typ, []byte(fmt.Sprintf(`{"fields":{"n":{"type":"integer","value":%d}}}`, n)), 201)
		if err != nil {
			return err
		}
		var inserted struct {
			Objects []p.Identity `json:"objects"`
		}
		if json.Unmarshal(raw, &inserted) != nil || len(inserted.Objects) != 1 {
			return fmt.Errorf("an insert returned no identity")
		}
		ids[n] = inserted.Objects[0].ID
	}
	raw, err := call("POST", base+"/objects/"+typ+":page", []byte(`{"query":{"result_sort":[{"field_id":"n","ascending":true}]},"page_size":2}`), 200)
	if err != nil {
		return err
	}
	var start struct {
		Cursor string `json:"cursor"`
	}
	if json.Unmarshal(raw, &start) != nil || start.Cursor == "" {
		return fmt.Errorf("invalid sorted page start")
	}
	seen := map[string]int{}
	moved := false
	for next := start.Cursor; next != ""; {
		raw, err = call("POST", base+"/pages/"+url.PathEscape(next)+":next", nil, 200)
		if err != nil {
			return err
		}
		var page struct {
			Records []p.Record `json:"records"`
			Next    *string    `json:"next_cursor"`
		}
		if json.Unmarshal(raw, &page) != nil {
			return fmt.Errorf("invalid sorted page")
		}
		for _, row := range page.Records {
			seen[row.ID.ID]++
		}
		// After the first page (n = 0 and 1): the row with n = 0 moves ahead
		// of everything, and the one with n = 3 is deleted.
		if !moved {
			moved = true
			if _, err = call("PATCH", base+"/objects/"+typ+"/"+url.PathEscape(ids[0]), []byte(`{"fields":{"n":{"type":"integer","value":100}}}`), 200); err != nil {
				return err
			}
			if _, err = call("DELETE", base+"/objects/"+typ+"/"+url.PathEscape(ids[3]), nil, 200); err != nil {
				return err
			}
		}
		next = ""
		if page.Next != nil {
			next = *page.Next
		}
	}
	for id, count := range seen {
		if count > 1 {
			return fmt.Errorf("a sorted sequence returned a row twice after its sort value moved: %s", id)
		}
	}
	for _, n := range []int{1, 2, 4, 5} {
		if seen[ids[n]] != 1 {
			return fmt.Errorf("a sorted sequence skipped a row that stayed put (n = %d)", n)
		}
	}
	return nil
}
