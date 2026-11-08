package security_test

import (
	"encoding/json"
	"net/url"
	"testing"

	"github.com/ekarpov/tessera/internal/identity"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// A participant erasing their dataset through the protocol must not blank the
// administration pages for the people who administer it, and the account
// registry must follow the dataset to its new identity: the person who erased
// holds the replacement, and nobody else's hold on it comes back.
func TestAnErasureThroughTheProtocolKeepsAdministrationWorking(t *testing.T) {
	x := tenants(t)
	analysis, err := x.runtime.Authority.AddService("analysis", "", store.Grants{})
	if err != nil {
		t.Fatal(err)
	}
	secret, err := x.runtime.Authority.AddIssuer(t.Context(), identity.Issuer{ID: "study",
		Template: &identity.Template{Administrator: x.alice, DisplayName: "{account}", Types: []p.Meta{}, Readers: []identity.TemplateReader{{Account: "analysis"}}},
		Grants:   store.Grants{Read: true, Erase: true}, Cap: 5})
	if err != nil {
		t.Fatal(err)
	}
	code, _, _ := x.inviteNaming(t, "study", secret, "")
	if got := x.redeem(t, code, "p1"); got != 201 {
		t.Fatal(got)
	}
	held, err := x.runtime.Engine.Provisioned(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	made := ""
	for _, d := range held {
		if d.DisplayName == "p1" {
			made = d.ID
		}
	}
	erasing := x.signInGrants(t, "p1", made, url.Values{"read": {"yes"}, "erase": {"yes"}})
	w := x.call(t, "POST", "/v1/datasets/"+made+":erase", erasing, `{"confirm":"`+made+`"}`, nil)
	var erased struct {
		DatasetID string `json:"dataset_id"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &erased); err != nil || w.Code != 200 {
		t.Fatal("the participant could not erase:", w.Code, w.Body.String())
	}

	// The custodian's pages still work.
	if w = x.admin(t, "GET", "/overview", x.signIn(t, x.alice, x.aliceSet, true), ""); w.Code != 200 {
		t.Fatal("one participant's erasure failed their administrator's overview:", w.Code, w.Body.String())
	}
	// The erasing installation works on the replacement, and the person can
	// sign in to it again: the registry shows it under their account.
	if got := x.call(t, "GET", "/v1/datasets/"+erased.DatasetID+"/meta", erasing, "", nil).Code; got != 200 {
		t.Fatal("the erasing installation lost the replacement:", got)
	}
	x.signInGrants(t, "p1", erased.DatasetID, url.Values{"read": {"yes"}})
	// Nobody else's hold comes back through the registry.
	if _, status := x.serviceToken(t, "analysis", analysis, erased.DatasetID); status != 400 {
		t.Fatal("a reader of the erased dataset was given the replacement:", status)
	}
	if _, status := x.serviceToken(t, "analysis", analysis, made); status != 400 {
		t.Fatal("a reader could still ask for the erased identity:", status)
	}
}

// A registry that fell behind the storage — an erasure it was never told
// about — makes administration show less, not fail.
func TestAdministrationSurvivesARegistryBehindTheStorage(t *testing.T) {
	x := tenants(t)
	eraser := store.Access{Principal: "alice", Authorization: "behind-the-registry"}
	if err := x.runtime.Engine.SetAuthorization(t.Context(), x.aliceSet, eraser, store.Authorization{Grants: store.Grants{Erase: true}, View: store.FullView()}); err != nil {
		t.Fatal(err)
	}
	if _, err := x.runtime.Engine.Erase(t.Context(), eraser, x.aliceSet, x.aliceSet); err != nil {
		t.Fatal(err)
	}
	// alice's registry still names the erased identity; she also administers
	// Bob's, so the overview has something to show.
	if err := x.runtime.Authority.AddPerson("alice", password, x.bobSet, store.Grants{Read: true, Write: true, Schema: true, Erase: true}, false); err != nil {
		t.Fatal(err)
	}
	if w := x.admin(t, "GET", "/overview", x.signIn(t, x.alice, x.bobSet, true), ""); w.Code != 200 {
		t.Fatal("a dataset the storage no longer has failed the whole overview:", w.Code, w.Body.String())
	}
}

// Erasing gives the eraser the replacement with the rights it had, not more.
// A participant holding read, write and erase, and never schema, used to
// gain schema on the replacement by erasing.
func TestErasingGrantsNoRightTheEraserDidNotHold(t *testing.T) {
	x := tenants(t)
	secret, err := x.runtime.Authority.AddIssuer(t.Context(), identity.Issuer{ID: "study",
		Template: &identity.Template{Administrator: x.alice, DisplayName: "{account}", Types: []p.Meta{}},
		Grants:   store.Grants{Read: true, Write: true, Erase: true}, Cap: 5})
	if err != nil {
		t.Fatal(err)
	}
	code, _, _ := x.inviteNaming(t, "study", secret, "")
	if got := x.redeem(t, code, "participant"); got != 201 {
		t.Fatal(got)
	}
	held, err := x.runtime.Engine.Provisioned(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	made := ""
	for _, d := range held {
		if d.DisplayName == "participant" {
			made = d.ID
		}
	}
	erasing := x.signInGrants(t, "participant", made, url.Values{"read": {"yes"}, "write": {"yes"}, "erase": {"yes"}})
	w := x.call(t, "POST", "/v1/datasets/"+made+":erase", erasing, `{"confirm":"`+made+`"}`, nil)
	var erased struct {
		DatasetID string `json:"dataset_id"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &erased); err != nil || w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = x.call(t, "POST", "/v1/datasets/"+erased.DatasetID+"/meta", erasing, `{"type":{"id":"own.type","fields":[]}}`, nil); w.Code != 403 {
		t.Fatal("erasing gave the eraser a schema right it never held:", w.Code, w.Body.String())
	}
	if w = x.call(t, "GET", "/v1/datasets/"+erased.DatasetID+"/meta", erasing, "", nil); w.Code != 200 {
		t.Fatal("the eraser lost the read it held:", w.Code)
	}
}
