package security_test

import (
	"encoding/json"
	"testing"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

func TestCreatorAttributionKeepsContributionAndReceiptAuthoritySeparate(t *testing.T) {
	x := tenants(t)
	const typ = "test.attributed"
	const service = "synthetic-contributor"
	const key = "c2d4309b-3447-4b73-83a4-221352ea16a8"
	const schema = `{"type":{"id":"test.attributed","fields":[{"id":"created_by","meta_id":"test.attributed","type":"text","creator":true}]}}`
	const insert = `{"fields":{}}`
	const forged = `{"fields":{"created_by":{"type":"text","value":"forged"}}}`
	secret, err := x.runtime.Authority.AddService(service, "", store.Grants{})
	if err != nil {
		t.Fatal(err)
	}

	setup := func(account, dataset, ownerToken string) (string, string) {
		t.Helper()
		base := "/v1/datasets/" + dataset
		if w := x.call(t, "POST", base+"/meta", ownerToken, schema, nil); w.Code != 201 {
			t.Fatal("could not create attributed type", w.Code)
		}
		if err := x.runtime.Authority.SetContributor(t.Context(), account, dataset, service, []string{typ}, nil, ""); err != nil {
			t.Fatal(err)
		}
		token, status := x.serviceToken(t, service, secret, dataset)
		if status != 200 {
			t.Fatal("could not sign in contributor", status)
		}
		return base, token
	}
	insertID := func(path, token string, headers map[string]string) string {
		t.Helper()
		w := x.call(t, "POST", path, token, insert, headers)
		var body struct {
			Objects   []p.Identity    `json:"objects"`
			RecordSet json.RawMessage `json:"record_set"`
		}
		if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &body) != nil || len(body.Objects) != 1 {
			t.Fatal("expected one committed identity", w.Code)
		}
		if body.RecordSet != nil || w.Header().Get("Preference-Applied") != "" {
			t.Fatal("identity-only insert disclosed a record representation")
		}
		return body.Objects[0].ID
	}
	creator := func(path, token string) string {
		t.Helper()
		w := x.call(t, "GET", path, token, "", nil)
		var body p.RecordSet
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || len(body.Records) != 1 || len(body.Records[0].Values) != 1 {
			t.Fatal("owner could not read the attributed record", w.Code)
		}
		id, ok := body.Records[0].Values[0].Payload().(string)
		if !ok || len(id) != p.CreatorIDBytes {
			t.Fatal("record lacks an opaque creator ID")
		}
		return id
	}

	base, token := setup(x.alice, x.aliceSet, x.aliceTok)
	path := base + "/objects/" + typ
	ownerID := insertID(path, x.aliceTok, nil)
	if got := x.answer(t, "POST", path, token, forged); got != (answer{400, "invalid_field"}) {
		t.Fatal("an insert-only contributor could supply a creator field", got)
	}
	const batch = `{"operations":[{"op":"insert","type":"test.attributed","fields":{}},{"op":"insert","type":"test.attributed","fields":{"created_by":{"type":"null"}}}]}`
	if got := x.answer(t, "POST", base+":transact", token, batch); got != (answer{400, "invalid_field"}) {
		t.Fatal("a transaction could supply a creator field", got)
	}
	headers := map[string]string{"Idempotency-Key": key, "Prefer": "return=representation"}
	contributedID := insertID(path, token, headers)
	contributed := creator(path+"/"+contributedID, x.aliceTok)
	if contributed == creator(path+"/"+ownerID, x.aliceTok) {
		t.Fatal("contribution was attributed to the dataset owner")
	}
	if got := x.answer(t, "GET", path+"/"+contributedID, token, ""); got != (answer{403, "permission_denied"}) {
		t.Fatal("a contributor gained read authority over its record", got)
	}
	if got := x.answer(t, "PATCH", path+"/"+contributedID, x.aliceTok, forged); got != (answer{400, "invalid_field"}) {
		t.Fatal("an owner could change a contributor's creator field", got)
	}

	checkReceipt := func(method, path, body string, headers map[string]string) {
		t.Helper()
		w := x.call(t, method, path, token, body, headers)
		var r store.Receipt
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &r) != nil || r.State != "committed" ||
			!r.ResultWithheld || len(r.Result) != 0 || len(r.Objects) != 1 || r.Objects[0].ObjectID != contributedID {
			t.Fatal("contributor could not recover just its committed identity", w.Code)
		}
	}
	status := base + "/operations/" + key + ":status"
	checkReceipt("POST", status, "", nil)
	checkReceipt("POST", path, insert, headers)
	if got := x.answer(t, "POST", status, x.aliceTok, ""); got != (answer{403, "permission_denied"}) {
		t.Fatal("a different principal recovered the contributor's receipt", got)
	}
	var counted struct{ Count int }
	w := x.call(t, "POST", path+":count", x.aliceTok, "{}", nil)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &counted) != nil || counted.Count != 2 {
		t.Fatal("the refused batch or replay changed the record count", w.Code, counted.Count)
	}

	otherBase, otherToken := setup(x.bob, x.bobSet, x.bobTok)
	otherPath := otherBase + "/objects/" + typ
	otherID := insertID(otherPath, otherToken, nil)
	if creator(otherPath+"/"+otherID, x.bobTok) == contributed {
		t.Fatal("the same service was linkable by creator ID across datasets")
	}
	if foreign, absent := x.answer(t, "GET", otherPath+"/"+otherID, token, ""),
		x.answer(t, "GET", "/v1/datasets/"+absentDataset+"/objects/"+typ+"/"+otherID, token, ""); foreign != absent || foreign.Status < 400 {
		t.Fatal("a token disclosed another dataset's attributed record", foreign, absent)
	}
}
