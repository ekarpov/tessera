package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"

	p "github.com/ekarpov/tessera/pkg/protocol"
)

// ScopedAttachmentFixture supplies two independent principals and a trusted
// grant setter. Both guests can read alpha and beta.
type ScopedAttachmentFixture struct {
	URL, Owner, Contributor, Other string
	Client                         *http.Client
	Grant                          func(context.Context, string, string, bool, bool) error
}

// CheckScopedAttachments proves the target and principal binding over HTTP.
func CheckScopedAttachments(ctx context.Context, f ScopedAttachmentFixture) error {
	owner := newContentCall(ctx, f.Client, f.URL, f.Owner)
	guest := newContentCall(ctx, f.Client, f.URL, f.Contributor)
	other := newContentCall(ctx, f.Client, f.URL, f.Other)
	list, err := owner("GET", "/v1/stores", "", nil, 200)
	if err != nil {
		return err
	}
	var stores struct {
		Stores []struct {
			ID           string
			DatasetID    string `json:"dataset_id"`
			Capabilities []string
		}
	}
	raw, err := json.Marshal(list)
	if err != nil || json.Unmarshal(raw, &stores) != nil || len(stores.Stores) != 1 {
		return fmt.Errorf("scoped attachment fixture requires one content store")
	}
	entry := stores.Stores[0]
	if !slices.Contains(entry.Capabilities, "scoped_attachment_writes") {
		return fmt.Errorf("scoped attachment capability missing")
	}
	const typ = "test.scoped_attachment"
	records := "/v1/datasets/" + entry.DatasetID + "/objects/" + typ
	imports := "/v1/stores/" + entry.ID + "/imports"
	schema := []byte(`{"type":{"id":"test.scoped_attachment","fields":[
	 {"id":"membership","meta_id":"test.scoped_attachment","type":"text","collection":true},
	 {"id":"creator","meta_id":"test.scoped_attachment","type":"text","creator":true},
	 {"id":"attachment","meta_id":"test.scoped_attachment","type":"media"}]}}`)
	if _, err = owner("POST", "/v1/datasets/"+entry.DatasetID+"/meta", "", schema, 201); err != nil {
		return err
	}
	for _, token := range []string{f.Contributor, f.Other} {
		if err = f.Grant(ctx, token, typ, true, true); err != nil {
			return err
		}
	}
	request := func(operation, collection, object string) []byte {
		body, _ := json.Marshal(struct {
			Operation string             `json:"operation_id"`
			Kind      string             `json:"content_type"`
			Maximum   int                `json:"maximum_bytes"`
			Target    p.AttachmentTarget `json:"attachment_target"`
		}{operation, "application/octet-stream", 3, p.AttachmentTarget{TypeID: typ, CollectionID: collection, ObjectID: object}})
		return body
	}
	if _, err = guest("POST", imports, "", []byte(`{"operation_id":"unscoped","content_type":"application/octet-stream","maximum_bytes":3}`), 403); err != nil {
		return err
	}
	if _, err = guest("POST", imports, "", request("outside", "ungranted", ""), 403); err != nil {
		return err
	}
	begin := func(operation, collection, object string) (upload, uri string, err error) {
		result, err := guest("POST", imports, "", request(operation, collection, object), 200)
		if err != nil {
			return "", "", fmt.Errorf("begin scoped import: %w", err)
		}
		upload, ok := result["upload_id"].(string)
		if !ok || upload == "" {
			return "", "", fmt.Errorf("scoped import returned no upload")
		}
		uri, err = issuedURI(result["url"], "scoped transfer URI")
		return upload, uri, err
	}
	upload, uri, err := begin("collection", "alpha", "")
	if err != nil {
		return err
	}
	if _, err = guest("POST", imports, "", request("collection", "beta", ""), 409); err != nil {
		return err
	}
	if _, err = other("POST", imports+"/"+upload+":commit", "", nil, 403); err != nil {
		return err
	}
	finish := func(upload, uri string) (string, error) {
		if _, err := guest("PATCH", uri, "bytes 0-2/3", []byte("abc"), 204); err != nil {
			return "", err
		}
		item, err := guest("POST", imports+"/"+upload+":commit", "", nil, 200)
		if err != nil {
			return "", fmt.Errorf("commit scoped import: %w", err)
		}
		id, ok := item["item_id"].(string)
		if !ok || id == "" {
			return "", fmt.Errorf("scoped import returned no item")
		}
		return entry.ID + "//" + id, nil
	}
	ref, err := finish(upload, uri)
	if err != nil {
		return err
	}
	fields := func(collection, reference string) []byte {
		values := map[string]map[string]string{}
		if collection != "" {
			values["membership"] = map[string]string{"type": "text", "value": collection}
		}
		if reference != "" {
			values["attachment"] = map[string]string{"type": "media", "value": reference}
		}
		raw, _ := json.Marshal(map[string]any{"fields": values})
		return raw
	}
	if _, err = other("POST", records, "", fields("alpha", ref), 403); err != nil {
		return err
	}
	if _, err = guest("POST", records, "", fields("beta", ref), 403); err != nil {
		return err
	}
	insert := func(call contentCall, ref string) (string, error) {
		result, err := call("POST", records, "", fields("alpha", ref), 201)
		if err != nil {
			return "", err
		}
		var response struct{ Objects []p.Identity }
		raw, err := json.Marshal(result)
		if err != nil || json.Unmarshal(raw, &response) != nil || len(response.Objects) != 1 {
			return "", fmt.Errorf("scoped attachment insert returned no identity")
		}
		return response.Objects[0].ID, nil
	}
	own, err := insert(guest, ref)
	if err != nil {
		return err
	}
	another, err := insert(guest, "")
	if err != nil {
		return err
	}
	foreign, err := insert(other, "")
	if err != nil {
		return err
	}
	if _, err = guest("POST", imports, "", request("foreign", "alpha", foreign), 403); err != nil {
		return err
	}
	upload, uri, err = begin("object", "alpha", own)
	if err != nil {
		return err
	}
	ref, err = finish(upload, uri)
	if err != nil {
		return err
	}
	if _, err = guest("PATCH", records+"/"+another, "", fields("", ref), 403); err != nil {
		return err
	}
	if _, err = guest("PATCH", records+"/"+own, "", fields("", ref), 200); err != nil {
		return err
	}
	upload, uri, err = begin("revoked", "alpha", own)
	if err != nil {
		return err
	}
	if err = f.Grant(ctx, f.Contributor, typ, false, false); err != nil {
		return err
	}
	if _, err = guest("PATCH", uri, "bytes 0-2/3", []byte("abc"), 403); err != nil {
		return err
	}
	_, err = guest("POST", imports+"/"+upload+":commit", "", nil, 403)
	return err
}
