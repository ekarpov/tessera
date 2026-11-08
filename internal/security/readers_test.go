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

// serviceToken asks for a service's token on one dataset, as a deployed
// service does, and returns it with the status.
func (x *tenancy) serviceToken(t *testing.T, service, secret, dataset string) (string, int) {
	t.Helper()
	r := httptest.NewRequest("POST", "/oauth/token", strings.NewReader(url.Values{"grant_type": {"client_credentials"}, "dataset": {dataset}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.SetBasicAuth(service, secret)
	w := httptest.NewRecorder()
	x.runtime.Handler.ServeHTTP(w, r)
	var issued struct {
		Token string `json:"access_token"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &issued)
	return issued.Token, w.Code
}

// typesSeen lists the types a token can see in a dataset.
func (x *tenancy) typesSeen(t *testing.T, dataset, token string) map[string]bool {
	t.Helper()
	w := x.call(t, "GET", "/v1/datasets/"+dataset+"/meta", token, "", nil)
	if w.Code != 200 {
		t.Fatal("the schema could not be read:", w.Code, w.Body.String())
	}
	var listed struct {
		Types []struct {
			ID string `json:"id"`
		} `json:"types"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil {
		t.Fatal(err, w.Body.String())
	}
	seen := map[string]bool{}
	for _, m := range listed.Types {
		seen[m.ID] = true
	}
	return seen
}

// A template issuer's policy can name readers for every dataset it creates:
// a study's analysis service reading every participant, a household's service
// reading only what each member shares. They get read, scoped when the policy
// says so, in the step that creates the dataset — and nothing more.
func TestATemplateGivesItsNamedReadersEachDatasetItCreates(t *testing.T) {
	x := tenants(t)
	var types []p.Meta
	if err := json.Unmarshal([]byte(`[
		{"id":"study.measurement","name":"Measurement","fields":[{"id":"n","meta_id":"study.measurement","type":"integer"}]},
		{"id":"study.diary","name":"Diary","fields":[{"id":"text","meta_id":"study.diary","type":"text"}]}]`), &types); err != nil {
		t.Fatal(err)
	}
	analysis, err := x.runtime.Authority.AddService("analysis", "", store.Grants{})
	if err != nil {
		t.Fatal(err)
	}
	home, err := x.runtime.Authority.AddService("home", "", store.Grants{})
	if err != nil {
		t.Fatal(err)
	}
	for _, refused := range []identity.TemplateReader{{Account: "nobody"}, {Account: x.alice}, {Account: "home", Types: []string{}}} {
		if _, err = x.runtime.Authority.AddIssuer(t.Context(), identity.Issuer{ID: "bad-" + refused.Account,
			Template: &identity.Template{Administrator: x.alice, DisplayName: "{account}", Types: types, Readers: []identity.TemplateReader{refused}},
			Grants:   store.Grants{Read: true}, Cap: 5}); err == nil {
			t.Fatal("a template accepted a reader it should not:", refused)
		}
	}
	// A type the engine would refuse is the operator's mistake now, not every
	// participant's failed sign-up later.
	broken := append([]p.Meta{}, types...)
	broken[0].Fields = append([]p.Field{}, broken[0].Fields...)
	broken[0].Fields[0].Type = "string"
	if _, err = x.runtime.Authority.AddIssuer(t.Context(), identity.Issuer{ID: "broken",
		Template: &identity.Template{Administrator: x.alice, DisplayName: "{account}", Types: broken},
		Grants:   store.Grants{Read: true}, Cap: 5}); err == nil {
		t.Fatal("a template with an invalid type was accepted")
	}
	secret, err := x.runtime.Authority.AddIssuer(t.Context(), identity.Issuer{ID: "study",
		Template: &identity.Template{Administrator: x.alice, DisplayName: "{account}", Types: types,
			Readers: []identity.TemplateReader{{Account: "analysis"}, {Account: "home", Types: []string{"study.measurement"}}}},
		Grants: store.Grants{Read: true, Write: true, Schema: true, Erase: true}, Cap: 5})
	if err != nil {
		t.Fatal(err)
	}
	code, _, _ := x.inviteNaming(t, "study", secret, "")
	if got := x.redeem(t, code, "sara"); got != 201 {
		t.Fatal("redemption failed", got)
	}
	held, err := x.runtime.Engine.Provisioned(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	made := ""
	for _, d := range held {
		if d.DisplayName == "sara" {
			made = d.ID
		}
	}
	if made == "" {
		t.Fatal("the template created no dataset")
	}

	reading, status := x.serviceToken(t, "analysis", analysis, made)
	if status != 200 {
		t.Fatal("the named reader could not get a token for the created dataset:", status)
	}
	if seen := x.typesSeen(t, made, reading); !seen["study.measurement"] || !seen["study.diary"] {
		t.Fatal("an unscoped reader did not read the whole dataset:", seen)
	}
	if w := x.call(t, "POST", "/v1/datasets/"+made+"/meta", reading,
		`{"type":{"id":"study.extra","name":"Extra","fields":[]}}`, nil); w.Code != 403 {
		t.Fatal("a template reader changed the schema:", w.Code)
	}
	if _, status = x.serviceToken(t, "analysis", analysis, x.bobSet); status != 400 {
		t.Fatal("a template reader reached a dataset the template did not create:", status)
	}

	household, _ := x.serviceToken(t, "home", home, made)
	if seen := x.typesSeen(t, made, household); !seen["study.measurement"] || seen["study.diary"] {
		t.Fatal("a scoped reader saw past its types:", seen)
	}
	// The person the dataset is for administers it, and changes what the
	// service reads the way they would for a person.
	sara := x.signIn(t, "sara", made, true)
	if w := x.admin(t, "PUT", "/datasets/"+made+"/readers/home", sara, `{"types":["study.measurement","study.diary"]}`); w.Code != 204 {
		t.Fatal("the dataset's person could not widen what a service reads:", w.Code, w.Body.String())
	}
	if seen := x.typesSeen(t, made, household); !seen["study.diary"] {
		t.Fatal("widening a service's share did not reach its existing token:", seen)
	}
	// The share can be read back, one reader at a time, so a person can be
	// shown what a service sees.
	if w := x.admin(t, "GET", "/datasets/"+made+"/readers/home", sara, ""); w.Code != 200 ||
		!strings.Contains(w.Body.String(), `"study.diary"`) || !strings.Contains(w.Body.String(), `"account":"home"`) {
		t.Fatal("one reader's share could not be read:", w.Code, w.Body.String())
	}
	if w := x.admin(t, "GET", "/datasets/"+made+"/readers/nobody", sara, ""); w.Code != 404 || !strings.Contains(w.Body.String(), `"not_found"`) {
		t.Fatal("an account that is not a reader was not answered not_found:", w.Code, w.Body.String())
	}
	if w := x.admin(t, "GET", "/no/such/route", sara, ""); w.Code != 404 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Fatal("an unknown API route was not answered in the API's shape:", w.Code, w.Header().Get("Content-Type"))
	}
	if w := x.admin(t, "PUT", "/datasets/"+made+"/readers/home", sara, `{"type":["study.diary"]}`); w.Code != 400 || !strings.Contains(w.Body.String(), "unknown_member") {
		t.Fatal("a misspelled member was not named as one:", w.Code, w.Body.String())
	}
	if w := x.admin(t, "PUT", "/datasets/"+made+"/readers/home", sara, `{"types":["study.diary"]`); w.Code != 400 || !strings.Contains(w.Body.String(), "malformed_body") {
		t.Fatal("unparseable JSON was not named as such:", w.Code, w.Body.String())
	}
	if w := x.admin(t, "PUT", "/datasets/"+made+"/readers/home", sara, `{"types":["study.measurement"]}`); w.Code != 204 {
		t.Fatal(w.Code)
	}
	if seen := x.typesSeen(t, made, household); seen["study.diary"] {
		t.Fatal("narrowing a service's share did not reach its existing token:", seen)
	}
}

// A client learns whose token it holds and what that account may administer,
// rather than inferring either from a refusal.
func TestATokenSaysWhoseItIs(t *testing.T) {
	x := tenants(t)
	ask := func(token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/oauth/account", nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		x.runtime.Handler.ServeHTTP(w, r)
		return w
	}
	var described struct {
		Account             string          `json:"account"`
		ServerAdministrator bool            `json:"server_administrator"`
		Installation        map[string]bool `json:"installation"`
	}
	w := ask(x.aliceTok)
	if err := json.Unmarshal(w.Body.Bytes(), &described); err != nil || w.Code != 200 || described.Account != "alice" || described.ServerAdministrator {
		t.Fatal("a token did not say whose it is:", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), x.aliceSet) {
		t.Fatal("the account description named a dataset")
	}
	if err := x.runtime.Authority.AddServerAdministrator("operator", password, "", store.Grants{}, false); err != nil {
		t.Fatal(err)
	}
	w = ask(x.signIn(t, "operator", "", true))
	described.ServerAdministrator = false
	if err := json.Unmarshal(w.Body.Bytes(), &described); err != nil || !described.ServerAdministrator || !described.Installation["administer"] {
		t.Fatal("a server administrator's token did not say so:", w.Body.String())
	}
	for _, token := range []string{"", "not-a-token"} {
		if w = ask(token); w.Code != 401 || strings.Contains(w.Body.String(), "alice") {
			t.Fatal("an unauthenticated request was described:", w.Code, w.Body.String())
		}
	}
	if err := x.runtime.Authority.SuspendAccount("alice"); err != nil {
		t.Fatal(err)
	}
	if w = ask(x.aliceTok); w.Code != 401 {
		t.Fatal("a suspended account's token was described:", w.Code)
	}
}

// A deployment restored onto a machine where a service's secret file is gone
// replaces the secret through the administration API. Whoever holds the new
// secret reads what the service reads, so it takes the server's
// administrator who also administers every dataset the service holds: one
// holding no data must not be able to read a dataset by taking over the
// service that reads it. The old secret stops at once, and a person's account
// is not a service.
func TestReplacingAServiceSecretTakesAdministeringWhatItReads(t *testing.T) {
	x := tenants(t)
	old, err := x.runtime.Authority.AddService("reader", x.aliceSet, store.Grants{Read: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = x.runtime.Authority.AddService("idle", "", store.Grants{}); err != nil {
		t.Fatal(err)
	}
	if err = x.runtime.Authority.AddServerAdministrator("operator", password, "", store.Grants{}, false); err != nil {
		t.Fatal(err)
	}
	if err = x.runtime.Authority.AddServerAdministrator("steward", password, x.aliceSet, store.Grants{Read: true, Write: true, Schema: true, Erase: true}, false); err != nil {
		t.Fatal(err)
	}
	if w := x.admin(t, "POST", "/services/reader/secret", x.signIn(t, x.alice, x.aliceSet, true), ""); w.Code != 403 {
		t.Fatal("a dataset administrator who is not the server's replaced a service secret:", w.Code)
	}
	operator := x.signIn(t, "operator", "", true)
	if w := x.admin(t, "POST", "/services/reader/secret", operator, ""); w.Code != 403 {
		t.Fatal("a server administrator holding no data took over a service that reads a dataset:", w.Code, w.Body.String())
	}
	if _, status := x.serviceToken(t, "reader", old, x.aliceSet); status != 200 {
		t.Fatal("a refused rotation changed the secret:", status)
	}
	if w := x.admin(t, "POST", "/services/idle/secret", operator, ""); w.Code != 200 {
		t.Fatal("a service holding nothing could not have its secret replaced:", w.Code)
	}
	w := x.admin(t, "POST", "/services/reader/secret", x.signIn(t, "steward", x.aliceSet, true), "")
	var rotated struct {
		Secret string `json:"secret"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &rotated); err != nil || w.Code != 200 || rotated.Secret == "" || rotated.Secret == old {
		t.Fatal("the service secret was not replaced by one who administers what it reads:", w.Code, w.Body.String())
	}
	if _, status := x.serviceToken(t, "reader", old, x.aliceSet); status < 400 {
		t.Fatal("the old service secret still works")
	}
	if _, status := x.serviceToken(t, "reader", rotated.Secret, x.aliceSet); status != 200 {
		t.Fatal("the new service secret does not work:", status)
	}
	for _, account := range []string{"alice", "nobody"} {
		if w = x.admin(t, "POST", "/services/"+account+"/secret", operator, ""); w.Code != 404 {
			t.Fatal("something that is not a service had its secret replaced:", account, w.Code)
		}
	}
}

// Administering the server is not administering a dataset. A server
// administrator holding no data may neither list a dataset's readers nor add
// one — themselves included — nor configure or re-secret an issuer that
// invites people into it: each is a way to read it.
func TestAServerAdministratorWithoutDataCannotLetThemselvesIn(t *testing.T) {
	x := tenants(t)
	if err := x.runtime.Authority.AddServerAdministrator("operator", password, "", store.Grants{}, false); err != nil {
		t.Fatal(err)
	}
	if err := x.runtime.Authority.AddServerAdministrator("steward", password, x.aliceSet, store.Grants{Read: true, Write: true, Schema: true, Erase: true}, false); err != nil {
		t.Fatal(err)
	}
	operator := x.signIn(t, "operator", "", true)
	if w := x.admin(t, "PUT", "/datasets/"+x.aliceSet+"/readers/operator", operator, `{"types":["test.record"]}`); w.Code != 403 {
		t.Fatal("a server administrator without data made themselves a reader:", w.Code)
	}
	if w := x.admin(t, "GET", "/datasets/"+x.aliceSet+"/readers", operator, ""); w.Code != 403 {
		t.Fatal("a server administrator without data listed a dataset's readers:", w.Code)
	}
	if _, err := x.runtime.Authority.AddAutomation(t.Context(), "operator", x.aliceSet); err == nil {
		t.Fatal("the refused reader was added anyway")
	}
	if w := x.admin(t, "POST", "/issuers", operator, `{"id":"backdoor","datasets":["`+x.aliceSet+`"],"grants":{"read":true}}`); w.Code != 403 {
		t.Fatal("a server administrator without data configured an issuer into a dataset:", w.Code)
	}
	if w := x.admin(t, "POST", "/issuers", operator, `{"id":"study","template":{"administrator":"alice","display_name":"{account}","types":[]},"grants":{"read":true},"cap":5}`); w.Code != 201 {
		t.Fatal("a template issuer, which reaches no existing data, was refused:", w.Code, w.Body.String())
	}
	steward := x.signIn(t, "steward", x.aliceSet, true)
	if w := x.admin(t, "POST", "/issuers", steward, `{"id":"household","datasets":["`+x.aliceSet+`"],"grants":{"read":true}}`); w.Code != 201 {
		t.Fatal("a server administrator of the dataset could not configure an issuer into it:", w.Code, w.Body.String())
	}
	if w := x.admin(t, "POST", "/issuers/household/secret", operator, ""); w.Code != 403 {
		t.Fatal("a server administrator without data re-secreted an issuer into a dataset:", w.Code)
	}
	if w := x.admin(t, "POST", "/issuers/household/secret", steward, ""); w.Code != 200 {
		t.Fatal("the dataset's administrator could not re-secret its issuer:", w.Code)
	}
	if w := x.admin(t, "PUT", "/datasets/"+x.aliceSet+"/readers/relative", x.signIn(t, x.alice, x.aliceSet, true), `{"types":["test.record"],"password":"correct horse battery staple"}`); w.Code != 204 {
		t.Fatal("the dataset's own administrator could no longer add a reader:", w.Code, w.Body.String())
	}
}
