package security_test

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ekarpov/tessera/internal/identity"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// inviteNaming asks for an invitation, optionally naming the account.
func (x *tenancy) inviteNaming(t *testing.T, issuer, secret, account string) (code, assigned string, status int) {
	t.Helper()
	form := url.Values{}
	if account != "" {
		form.Set("account", account)
	}
	r := httptest.NewRequest("POST", "/oauth/invitations", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.SetBasicAuth(issuer, secret)
	w := httptest.NewRecorder()
	x.runtime.Handler.ServeHTTP(w, r)
	var answer struct {
		Invitation string `json:"invitation"`
		Account    string `json:"account"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &answer)
	return answer.Invitation, answer.Account, w.Code
}

// For a study, the issuer chooses the account name, so a participant cannot
// type their real one. The name is shown to the participant as fixed, and
// whatever the form sends in its place is ignored.
func TestAnIssuerThatAssignsNamesKeepsParticipantsPseudonymous(t *testing.T) {
	x := tenants(t)
	secret, err := x.runtime.Authority.AddIssuer(t.Context(), identity.Issuer{ID: "study", Datasets: []string{x.aliceSet},
		Grants: store.Grants{Read: true}, AssignNames: true})
	if err != nil {
		t.Fatal(err)
	}
	code, assigned, status := x.inviteNaming(t, "study", secret, "p-7f3a")
	if status != 201 || assigned != "p-7f3a" {
		t.Fatal("the issuer could not assign a name:", status, assigned)
	}
	if _, _, status = x.inviteNaming(t, "study", secret, "p-7f3a"); status < 400 {
		t.Fatal("a name reserved by an unredeemed invitation was assigned twice")
	}
	page := httptest.NewRecorder()
	x.runtime.Handler.ServeHTTP(page, httptest.NewRequest("GET", "/oauth/invitation?code="+url.QueryEscape(code), nil))
	if !strings.Contains(page.Body.String(), `data-account="p-7f3a"`) || strings.Contains(page.Body.String(), `name="account"`) {
		t.Fatal("the redeem page does not show the assigned name as fixed")
	}
	// The participant types their real name anyway.
	if got := x.redeem(t, code, "Jane Realname"); got != 201 {
		t.Fatal("redemption failed", got)
	}
	if _, err = x.runtime.Authority.AddAutomation(t.Context(), "Jane Realname", x.aliceSet); err == nil {
		t.Fatal("an account was created under the name the participant typed")
	}
	if _, err = x.runtime.Authority.AddAutomation(t.Context(), "p-7f3a", x.aliceSet); err != nil {
		t.Fatal("the account was not created under the assigned name:", err)
	}
	// The rule is the authority's, not the redeem page's: a caller that skips
	// the page and names somebody else still gets the assigned name.
	direct, _, _ := x.inviteNaming(t, "study", secret, "p-direct")
	if _, err = x.runtime.Authority.RedeemInvitation(t.Context(), direct, "John Realname", "correct horse battery staple"); err != nil {
		t.Fatal("direct redemption failed:", err)
	}
	if _, err = x.runtime.Authority.AddAutomation(t.Context(), "John Realname", x.aliceSet); err == nil {
		t.Fatal("the authority created an account under a name the invitee chose")
	}
	// Named by nobody, it is generated.
	_, generated, status := x.inviteNaming(t, "study", secret, "")
	if status != 201 || !strings.HasPrefix(generated, "p-") {
		t.Fatal("an issuer naming nobody did not get a generated pseudonym:", status, generated)
	}

	// An issuer not configured to assign names cannot.
	household, err := x.runtime.Authority.AddIssuer(t.Context(), identity.Issuer{ID: "household", Datasets: []string{x.aliceSet}, Grants: store.Grants{Read: true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, status = x.inviteNaming(t, "household", household, "chosen-by-issuer"); status != 400 {
		t.Fatal("an issuer not configured to assign names named an account:", status)
	}
	// And an invitee choosing their own name cannot take one reserved for somebody else.
	reservation, _, _ := x.inviteNaming(t, "study", secret, "p-reserved")
	_ = reservation
	chooser, _, _ := x.inviteNaming(t, "household", household, "")
	if got := x.redeem(t, chooser, "p-reserved"); got != 409 {
		t.Fatal("an invitee took a name reserved by another invitation:", got)
	}
}

// Each dataset a template creates can be named after its person, and a
// dataset's administrator can rename one.
func TestTemplateDatasetsAreNamedAfterTheirPeopleAndCanBeRenamed(t *testing.T) {
	x := tenants(t)
	secret, err := x.runtime.Authority.AddIssuer(t.Context(), identity.Issuer{ID: "home",
		Template: &identity.Template{Administrator: x.alice, DisplayName: "{account}", Types: []p.Meta{}},
		Grants:   store.Grants{Read: true, Write: true}, Cap: 5})
	if err != nil {
		t.Fatal(err)
	}
	code, _, _ := x.inviteNaming(t, "home", secret, "")
	if got := x.redeem(t, code, "sara"); got != 201 {
		t.Fatal("redemption failed", got)
	}
	held, err := x.runtime.Engine.Provisioned(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	named := ""
	for _, d := range held {
		if d.DisplayName == "sara" {
			named = d.ID
		}
	}
	if named == "" {
		t.Fatal("the template's dataset was not named after its person")
	}
	admin := x.signIn(t, x.alice, x.aliceSet, true)
	if w := x.admin(t, "PUT", "/datasets/"+named+"/name", admin, `{"display_name":"Sara's records"}`); w.Code != 204 {
		t.Fatal("the administrator could not rename the dataset:", w.Code, w.Body.String())
	}
	if w := x.admin(t, "PUT", "/datasets/"+named+"/name", x.signIn(t, x.bob, x.bobSet, true), `{"display_name":"Taken over"}`); w.Code != 403 {
		t.Fatal("an administrator of another dataset renamed this one:", w.Code)
	}
	held, _ = x.runtime.Engine.Provisioned(t.Context())
	for _, d := range held {
		if d.ID == named && d.DisplayName != "Sara's records" {
			t.Fatal("the rename did not take:", d.DisplayName)
		}
	}
}
