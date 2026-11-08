package security_test

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ekarpov/tessera/internal/identity"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// invite asks the issuer endpoint for one invitation, returning the code or the
// status it was refused with.
func (x *tenancy) invite(t *testing.T, issuer, secret string) (string, int) {
	t.Helper()
	r := httptest.NewRequest("POST", "/oauth/invitations", nil)
	r.SetBasicAuth(issuer, secret)
	w := httptest.NewRecorder()
	x.runtime.Handler.ServeHTTP(w, r)
	var answer struct {
		Invitation string `json:"invitation"`
		RedeemURL  string `json:"redeem_url"`
	}
	if w.Code == 201 {
		if json.Unmarshal(w.Body.Bytes(), &answer) != nil || answer.Invitation == "" || !strings.Contains(answer.RedeemURL, "/oauth/invitation?code=") {
			t.Fatal("an issued invitation carries no code or link", w.Body.String())
		}
	}
	return answer.Invitation, w.Code
}

// redeem submits the invitation form as the invitee would.
func (x *tenancy) redeem(t *testing.T, code, account string) int {
	t.Helper()
	form := url.Values{"code": {code}, "account": {account}, "password": {password}}
	r := httptest.NewRequest("POST", "/oauth/invitation", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	x.runtime.Handler.ServeHTTP(w, r)
	return w.Code
}

// A household invites a member into its shared dataset. The member joins that
// dataset with the grants the operator gave the issuer, and nothing else.
func TestAnInvitationAddsItsInviteeToTheNamedDatasetOnly(t *testing.T) {
	x := tenants(t)
	secret, err := x.runtime.Authority.AddIssuer(t.Context(), identity.Issuer{ID: "household", Datasets: []string{x.aliceSet}, Grants: store.Grants{Read: true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, code := x.invite(t, "household", "not the secret"); code != 401 {
		t.Fatal("an issuer with the wrong secret was answered", code)
	}
	code, status := x.invite(t, "household", secret)
	if status != 201 {
		t.Fatal("the issuer could not issue", status)
	}
	if got := x.redeem(t, code, "newcomer"); got != 201 {
		t.Fatal("the invitation could not be redeemed", got)
	}
	if got := x.redeem(t, code, "second-newcomer"); got != 404 {
		t.Fatal("an invitation was redeemed twice", got)
	}
	token, err := x.runtime.Authority.AddAutomation(t.Context(), "newcomer", x.aliceSet)
	if err != nil {
		t.Fatal("the new account holds nothing in the dataset it was invited to:", err)
	}
	if got := x.call(t, "GET", "/v1/datasets/"+x.aliceSet+"/meta", token, "", nil).Code; got != 200 {
		t.Fatal("the invitee cannot read the dataset it joined", got)
	}
	if got := x.call(t, "POST", "/v1/datasets/"+x.aliceSet+"/meta", token,
		`{"type":{"id":"security.invited","name":"X","fields":[]}}`, nil).Code; got < 400 {
		t.Fatal("the invitee holds more than the issuer was allowed to give", got)
	}
	if _, err = x.runtime.Authority.AddAutomation(t.Context(), "newcomer", x.bobSet); err == nil {
		t.Fatal("the invitee reaches a dataset the issuer never named")
	}
}

// A study invites each participant into a dataset of their own, created at
// redemption with the study's types and administered by the researcher. The
// issuing service ends up holding nothing in any of them.
func TestATemplateInvitationCreatesADatasetPerInvitee(t *testing.T) {
	x := tenants(t)
	const typeID = "study.measurement"
	types := []p.Meta{{ID: typeID, Name: "Measurement", Fields: []p.Field{{ID: "n", MetaID: typeID, Type: p.IntegerType}}}}
	if _, err := x.runtime.Authority.AddIssuer(t.Context(), identity.Issuer{ID: "uncapped", Template: &identity.Template{Administrator: x.alice, Types: types}, Grants: store.Grants{Read: true}}); err == nil {
		t.Fatal("a template issuer without a cap was configured")
	}
	secret, err := x.runtime.Authority.AddIssuer(t.Context(), identity.Issuer{
		ID: "study", Template: &identity.Template{Administrator: x.alice, DisplayName: "Participant", Types: types},
		Grants: store.Grants{Read: true, Write: true, Erase: true}, Cap: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	datasets := map[string]string{}
	for _, participant := range []string{"participant-1", "participant-2"} {
		code, status := x.invite(t, "study", secret)
		if status != 201 {
			t.Fatal("the study could not issue within its cap", status)
		}
		if got := x.redeem(t, code, participant); got != 201 {
			t.Fatal("a participant could not redeem", got)
		}
	}
	if _, status := x.invite(t, "study", secret); status != 403 {
		t.Fatal("an issuer issued past its cap", status)
	}
	for _, d := range x.runtime.Authority.AdminOverview(t.Context(), x.alice).Principals {
		if strings.HasPrefix(d.ID, "participant-") {
			for dataset := range d.Datasets {
				datasets[d.ID] = dataset
			}
		}
	}
	if len(datasets) != 2 || datasets["participant-1"] == datasets["participant-2"] {
		t.Fatal("each participant did not get a dataset of their own, visible to the researcher:", datasets)
	}
	for participant, dataset := range datasets {
		token, err := x.runtime.Authority.AddAutomation(t.Context(), participant, dataset)
		if err != nil {
			t.Fatal(participant, "holds nothing in its own dataset:", err)
		}
		if got := x.call(t, "POST", "/v1/datasets/"+dataset+"/objects/"+typeID, token, `{"fields":{"n":{"type":"integer","value":1}}}`, nil).Code; got != 201 {
			t.Fatal(participant, "cannot record into its dataset, so the types were not installed with it", got)
		}
		if got := x.call(t, "POST", "/v1/datasets/"+dataset+"/meta", token, `{"type":{"id":"study.own","name":"X","fields":[]}}`, nil).Code; got < 400 {
			t.Fatal(participant, "may change the study's schema", got)
		}
		for other, theirs := range datasets {
			if other != participant {
				if got := x.call(t, "GET", "/v1/datasets/"+theirs+"/meta", token, "", nil).Code; got < 400 {
					t.Fatal(participant, "reaches another participant's dataset", got)
				}
			}
		}
		// The researcher administers it.
		if _, err = x.runtime.Authority.AddAutomation(t.Context(), x.alice, dataset); err != nil {
			t.Fatal("the researcher does not hold a participant's dataset:", err)
		}
	}
	reach, err := x.runtime.Engine.Reach(t.Context(), store.Access{Principal: "invitation", Authorization: "invitation-installer"})
	if err != nil {
		t.Fatal(err)
	}
	if len(reach) != 0 {
		t.Fatal("the handle that created participants' datasets still reaches them:", reach)
	}
}

// Redemption only ever creates a new account. Redeeming into an existing name
// would let whoever holds a code add datasets to somebody else's account, and a
// deleted account's name stays retired.
func TestAnInvitationCannotBeRedeemedIntoAnExistingOrRetiredName(t *testing.T) {
	x := tenants(t)
	secret, err := x.runtime.Authority.AddIssuer(t.Context(), identity.Issuer{ID: "household", Datasets: []string{x.aliceSet}, Grants: store.Grants{Read: true}})
	if err != nil {
		t.Fatal(err)
	}
	if err = x.runtime.Authority.AddPerson("gone", password, x.aliceSet, store.Grants{Read: true}, false); err != nil {
		t.Fatal(err)
	}
	if _, err = x.runtime.Authority.DeleteAccount(t.Context(), "gone", nil); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{x.bob, "gone"} {
		code, _ := x.invite(t, "household", secret)
		if got := x.redeem(t, code, name); got != 409 {
			t.Fatalf("an invitation was redeemed into the name %q: %d", name, got)
		}
	}
	if _, err = x.runtime.Authority.AddAutomation(t.Context(), x.bob, x.aliceSet); err == nil {
		t.Fatal("redeeming into an existing name added a dataset to that account")
	}
}

// An invitation that leads into an application can lead back to it: the redeem
// page offers a way on only to one of that application's registered redirect
// URIs — a private-use scheme included, which is what an app registers — and
// never to an address it did not register.
func TestARedeemedInvitationLeadsBackIntoTheApplication(t *testing.T) {
	x := tenants(t)
	secret, err := x.runtime.Authority.AddIssuer(t.Context(), identity.Issuer{ID: "household", Datasets: []string{x.aliceSet}, Grants: store.Grants{Read: true}})
	if err != nil {
		t.Fatal(err)
	}
	const back = "com.example.app:/connect"
	registration := httptest.NewRecorder()
	x.runtime.Handler.ServeHTTP(registration, httptest.NewRequest("POST", "/oauth/register",
		strings.NewReader(`{"client_name":"Home","redirect_uris":["`+back+`"]}`)))
	var client struct {
		ID string `json:"client_id"`
	}
	if registration.Code != 201 || json.Unmarshal(registration.Body.Bytes(), &client) != nil {
		t.Fatal("could not register the application", registration.Code, registration.Body.String())
	}
	for _, c := range []struct {
		returnTo string
		offered  bool
	}{{back, true}, {"https://elsewhere.example/phish", false}} {
		code, status := x.invite(t, "household", secret)
		if status != 201 {
			t.Fatal(status)
		}
		shown := httptest.NewRecorder()
		x.runtime.Handler.ServeHTTP(shown, httptest.NewRequest("GET", "/oauth/invitation?"+url.Values{"code": {code}, "client_id": {client.ID}, "return_to": {c.returnTo}}.Encode(), nil))
		if shown.Header().Get("Referrer-Policy") != "no-referrer" {
			t.Fatal("the redeem page, whose address carries the code, sends it on as a referrer")
		}
		form := url.Values{"code": {code}, "account": {"member-" + fmt.Sprint(c.offered)}, "password": {password}, "client_id": {client.ID}, "return_to": {c.returnTo}}
		r := httptest.NewRequest("POST", "/oauth/invitation", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		done := httptest.NewRecorder()
		x.runtime.Handler.ServeHTTP(done, r)
		if done.Code != 201 {
			t.Fatal("redeeming failed", done.Code)
		}
		page := done.Body.String()
		offered := strings.Contains(page, `href="`+c.returnTo+`"`) && strings.Contains(page, "Continue to Home")
		if offered != c.offered {
			t.Fatalf("the way back to %q was offered: %v, want %v\n%s", c.returnTo, offered, c.offered, page)
		}
		if strings.Contains(page, "elsewhere.example") {
			t.Fatal("an address the application never registered appears on the page")
		}
	}
	// The account pages share the rule, and shared the fault: a private-use
	// return address was rendered as a dead link.
	for _, path := range []string{"/oauth/account/password", "/oauth/account/delete", "/oauth/account/recovery"} {
		w := httptest.NewRecorder()
		x.runtime.Handler.ServeHTTP(w, httptest.NewRequest("GET", path+"?"+url.Values{"client_id": {client.ID}, "return_to": {back}}.Encode(), nil))
		if !strings.Contains(w.Body.String(), `href="`+back+`"`) {
			t.Fatal("the account page does not lead back into the application:", path)
		}
	}
}
