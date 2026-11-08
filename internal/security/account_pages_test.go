package security_test

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/ekarpov/tessera/internal/identity"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// form posts a form to the server and returns the answer.
func (x *tenancy) form(t *testing.T, path string, values url.Values) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", path, strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	x.runtime.Handler.ServeHTTP(w, r)
	return w
}

// whoami asks /oauth/account about a token.
func (x *tenancy) whoami(t *testing.T, token string) map[string]any {
	t.Helper()
	r := httptest.NewRequest("GET", "/oauth/account", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	x.runtime.Handler.ServeHTTP(w, r)
	var answer map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &answer)
	return answer
}

// Setting somebody's password is signing in as them, so it is reading their
// data. A server administrator who administers none of an account's datasets
// may suspend or delete it, and may not set its password. Whoever does set one
// is named to the person until they choose their own, and choosing their own
// signs out everybody who was signed in as them.
func TestSettingAPasswordIsDataAccessAndThePersonIsTold(t *testing.T) {
	x := tenants(t)
	if err := x.runtime.Authority.AddServerAdministrator("operator", password, "", store.Grants{}, false); err != nil {
		t.Fatal(err)
	}
	if err := x.runtime.Authority.AddPerson("member", password, x.aliceSet, store.Grants{Read: true}, false); err != nil {
		t.Fatal(err)
	}
	if err := x.runtime.Authority.AddPerson("newcomer", password, "", store.Grants{}, true); err != nil {
		t.Fatal(err)
	}
	operator := x.signIn(t, "operator", "", true)
	if w := x.admin(t, "POST", "/accounts/member/password", operator, `{"password":"operator-chosen-password"}`); w.Code != 403 {
		t.Fatal("a server administrator set the password of an account whose data they do not administer:", w.Code)
	}
	if w := x.admin(t, "POST", "/accounts/member/suspend", operator, ""); w.Code != 204 {
		t.Fatal("the server administrator could no longer suspend:", w.Code)
	}
	if w := x.admin(t, "POST", "/accounts/member/reinstate", operator, ""); w.Code != 204 {
		t.Fatal(w.Code)
	}
	if w := x.admin(t, "POST", "/accounts/newcomer/password", operator, `{"password":"operator-chosen-password"}`); w.Code != 204 {
		t.Fatal("a server administrator could not set the password of an account holding no data:", w.Code)
	}

	alice := x.signIn(t, x.alice, x.aliceSet, true)
	if w := x.admin(t, "POST", "/accounts/member/password", alice, `{"password":"alice-chosen-password"}`); w.Code != 204 {
		t.Fatal("the administrator of every dataset the member holds could not set the password:", w.Code, w.Body.String())
	}
	memberToken := x.signInWith(t, "member", "alice-chosen-password", x.aliceSet)
	told, _ := x.whoami(t, memberToken)["password_set_by"].(map[string]any)
	if told["account"] != "alice" {
		t.Fatal("the person was not told who set their password:", x.whoami(t, memberToken))
	}

	change := func(current, next string) *httptest.ResponseRecorder {
		return x.form(t, "/oauth/account/password", url.Values{"account": {"member"}, "password": {current}, "new_password": {next}})
	}
	if w := change("wrong-password-entirely", "member-own-password"); w.Code != 401 {
		t.Fatal("a wrong current password changed the password:", w.Code)
	}
	if w := change("alice-chosen-password", "short"); w.Code != 400 {
		t.Fatal("a short new password was accepted:", w.Code)
	}
	if w := change("alice-chosen-password", "member-own-password"); w.Code != 200 {
		t.Fatal("the person could not choose their own password:", w.Code, w.Body.String())
	}
	if code := x.call(t, "GET", "/v1/datasets/"+x.aliceSet+"/meta", memberToken, "", nil).Code; code != 401 {
		t.Fatal("a sign-in made with the administrator's password survived the change:", code)
	}
	fresh := x.signInWith(t, "member", "member-own-password", x.aliceSet)
	if _, still := x.whoami(t, fresh)["password_set_by"]; still {
		t.Fatal("a password the person chose was still reported as set by somebody else")
	}
}

// A template can make its datasets go with their person: deleting the account
// erases them even though the template's administrator — a custodian nobody
// signs in as — also holds them. Still only when the deletion names them.
func TestATemplateCanEraseItsDatasetsWithTheirAccount(t *testing.T) {
	x := tenants(t)
	if err := x.runtime.Authority.AddServerAdministrator("operator", password, "", store.Grants{}, false); err != nil {
		t.Fatal(err)
	}
	for _, erase := range []bool{false, true} {
		issuer := map[bool]string{false: "keeps", true: "erases"}[erase]
		secret, err := x.runtime.Authority.AddIssuer(t.Context(), identity.Issuer{ID: issuer,
			Template: &identity.Template{Administrator: x.alice, DisplayName: "{account}", Types: []p.Meta{}, EraseWithAccount: erase},
			Grants:   store.Grants{Read: true, Write: true, Erase: true}, Cap: 5})
		if err != nil {
			t.Fatal(err)
		}
		code, _, _ := x.inviteNaming(t, issuer, secret, "")
		if got := x.redeem(t, code, "member-"+issuer); got != 201 {
			t.Fatal(got)
		}
	}
	operator := x.signIn(t, "operator", "", true)
	// Without the flag the custodian keeps it, as before.
	if w := x.admin(t, "POST", "/accounts/member-keeps/delete", operator, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "another_administrator") {
		t.Fatal("a member's dataset without erase_with_account was not kept with its custodian:", w.Code, w.Body.String())
	}
	w := x.admin(t, "POST", "/accounts/member-erases/delete", operator, "")
	var refused struct {
		Error struct {
			Details struct {
				Datasets []string `json:"datasets"`
			} `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &refused)
	if w.Code != 409 || len(refused.Error.Details.Datasets) != 1 {
		t.Fatal("deleting a member whose dataset goes with them did not ask to confirm its erasure:", w.Code, w.Body.String())
	}
	made := refused.Error.Details.Datasets[0]
	w = x.admin(t, "POST", "/accounts/member-erases/delete", operator, `{"erase":["`+made+`"]}`)
	if w.Code != 200 || strings.Contains(w.Body.String(), "another_administrator") {
		t.Fatal("the confirmed deletion did not erase the member's dataset:", w.Code, w.Body.String())
	}
	held, err := x.runtime.Engine.Provisioned(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range held {
		if d.ID == made {
			t.Fatal("the member's dataset survived their deletion")
		}
	}
}

var ticketField = regexp.MustCompile(`name="ticket" value="([^"]+)"`)

// A person deletes their account on a page Tessera renders, so no product
// holds their password. The page shows exactly what will be erased and what
// will not before anything happens, and the confirmation carries a one-time
// ticket rather than the password.
func TestThePersonDeletesTheirAccountOnTesserasOwnPage(t *testing.T) {
	x := tenants(t)
	if err := x.runtime.Authority.AddPerson("member", password, x.aliceSet, store.Grants{Read: true}, false); err != nil {
		t.Fatal(err)
	}
	registration := httptest.NewRecorder()
	x.runtime.Handler.ServeHTTP(registration, httptest.NewRequest("POST", "/oauth/register",
		strings.NewReader(`{"client_name":"Console","redirect_uris":["https://console.example/back"]}`)))
	var client struct {
		ID string `json:"client_id"`
	}
	_ = json.Unmarshal(registration.Body.Bytes(), &client)

	page := httptest.NewRecorder()
	x.runtime.Handler.ServeHTTP(page, httptest.NewRequest("GET", "/oauth/account/delete?"+url.Values{"client_id": {client.ID}, "return_to": {"https://console.example/back"}}.Encode(), nil))
	if page.Code != 200 || !strings.Contains(page.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") || !strings.Contains(page.Body.String(), "https://console.example/back") {
		t.Fatal("the deletion page was not served with its way back:", page.Code)
	}
	elsewhere := httptest.NewRecorder()
	x.runtime.Handler.ServeHTTP(elsewhere, httptest.NewRequest("GET", "/oauth/account/delete?"+url.Values{"client_id": {client.ID}, "return_to": {"https://attacker.example/"}}.Encode(), nil))
	if strings.Contains(elsewhere.Body.String(), "attacker.example") {
		t.Fatal("the page offered a way back the product never registered")
	}

	if w := x.form(t, "/oauth/account/delete", url.Values{"page": {"1"}, "account": {"alice"}, "password": {"not-the-password"}}); w.Code != 401 || strings.Contains(w.Body.String(), x.aliceSet) {
		t.Fatal("a wrong password was not refused, or was shown the datasets:", w.Code)
	}
	w := x.form(t, "/oauth/account/delete", url.Values{"page": {"1"}, "account": {"alice"}, "password": {password}})
	match := ticketField.FindStringSubmatch(w.Body.String())
	if w.Code != 200 || match == nil || !strings.Contains(w.Body.String(), x.aliceSet) || strings.Contains(w.Body.String(), password) {
		t.Fatal("the confirmation did not list what would be erased, or carried the password:", w.Code)
	}
	if got := x.call(t, "GET", "/v1/datasets/"+x.aliceSet+"/meta", x.aliceTok, "", nil).Code; got != 200 {
		t.Fatal("something was deleted before it was confirmed:", got)
	}
	confirm := url.Values{"page": {"1"}, "ticket": {match[1]}, "decision": {"delete"}}
	if w = x.form(t, "/oauth/account/delete", confirm); w.Code != 200 || !strings.Contains(w.Body.String(), "is deleted") {
		t.Fatal("the confirmed deletion did not happen:", w.Code, w.Body.String())
	}
	if w = x.form(t, "/oauth/account/delete", confirm); w.Code != 400 {
		t.Fatal("a confirmation was spent twice:", w.Code)
	}

	// Suspended between proving the password and confirming: refused.
	w = x.form(t, "/oauth/account/delete", url.Values{"page": {"1"}, "account": {"member"}, "password": {password}})
	match = ticketField.FindStringSubmatch(w.Body.String())
	if match == nil {
		t.Fatal("no confirmation for the member:", w.Code)
	}
	if err := x.runtime.Authority.SuspendAccount("member"); err != nil {
		t.Fatal(err)
	}
	if w = x.form(t, "/oauth/account/delete", url.Values{"page": {"1"}, "ticket": {match[1]}, "decision": {"delete"}}); w.Code != 401 {
		t.Fatal("an account suspended after proving its password deleted itself:", w.Code)
	}
}
