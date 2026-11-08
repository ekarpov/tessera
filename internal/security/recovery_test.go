package security_test

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/ekarpov/tessera/internal/identity"
	"github.com/ekarpov/tessera/pkg/store"
)

// The code is marked for a product's own tests to find, on every page that
// shows one.
var shownCode = regexp.MustCompile(`data-recovery-code="([A-Z0-9-]+)"`)

// A person who forgets their password gets back in with the recovery code
// shown to them once when they made their account, and nobody else can do it
// for them. Using it signs out everything signed in as them and replaces it.
func TestAForgottenPasswordIsRecoveredWithTheCodeShownOnce(t *testing.T) {
	x := tenants(t)
	secret, err := x.runtime.Authority.AddIssuer(t.Context(), identity.Issuer{ID: "portal", Datasets: []string{x.aliceSet}, Grants: store.Grants{Read: true}})
	if err != nil {
		t.Fatal(err)
	}
	code, _, _ := x.inviteNaming(t, "portal", secret, "")
	r := httptest.NewRequest("POST", "/oauth/invitation", strings.NewReader(url.Values{"code": {code}, "account": {"forgetful"}, "password": {password}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	page := httptest.NewRecorder()
	x.runtime.Handler.ServeHTTP(page, r)
	shown := shownCode.FindStringSubmatch(page.Body.String())
	if page.Code != 201 || shown == nil {
		t.Fatal("the new account's recovery code was not shown:", page.Code)
	}
	signedIn := x.signInWith(t, "forgetful", password, x.aliceSet)

	recover := func(code, next string) *httptest.ResponseRecorder {
		return x.form(t, "/oauth/account/recover", url.Values{"account": {"forgetful"}, "code": {code}, "new_password": {next}})
	}
	if w := recover("AAAA-BBBB-CCCC-DDDD-EEEE-FFFF", "a-new-password-here"); w.Code != 401 {
		t.Fatal("a wrong recovery code set a password:", w.Code)
	}
	// Copied by hand: lower case, spaces for dashes.
	typed := strings.ToLower(strings.ReplaceAll(shown[1], "-", " "))
	w := recover(typed, "a-new-password-here")
	var answer struct {
		Recovery string `json:"recovery_code"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &answer); err != nil || w.Code != 200 || answer.Recovery == "" || answer.Recovery == shown[1] {
		t.Fatal("the recovery code did not recover the account, or was not replaced:", w.Code, w.Body.String())
	}
	if got := x.call(t, "GET", "/v1/datasets/"+x.aliceSet+"/meta", signedIn, "", nil).Code; got != 401 {
		t.Fatal("a sign-in from before the recovery survived it:", got)
	}
	x.signInWith(t, "forgetful", "a-new-password-here", x.aliceSet)
	if w = recover(shown[1], "yet-another-password"); w.Code != 401 {
		t.Fatal("a used recovery code worked a second time:", w.Code)
	}

	// Asked for anew with the password, the old code ends.
	w = x.form(t, "/oauth/account/recovery", url.Values{"account": {"forgetful"}, "password": {"a-new-password-here"}})
	var reissued struct {
		Recovery string `json:"recovery_code"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &reissued); err != nil || w.Code != 200 || reissued.Recovery == "" {
		t.Fatal("a new recovery code could not be had with the password:", w.Code)
	}
	if w = recover(answer.Recovery, "yet-another-password"); w.Code != 401 {
		t.Fatal("a replaced recovery code still worked:", w.Code)
	}
	if w = x.form(t, "/oauth/account/recovery", url.Values{"account": {"forgetful"}, "password": {"wrong password here"}}); w.Code != 401 {
		t.Fatal("a recovery code was issued without the password:", w.Code)
	}

	// Suspension is somebody else's decision; recovering does not get round it.
	if err = x.runtime.Authority.SuspendAccount("forgetful"); err != nil {
		t.Fatal(err)
	}
	if w = recover(reissued.Recovery, "yet-another-password"); w.Code != 401 {
		t.Fatal("a suspended account recovered itself:", w.Code)
	}
}

// An account an administrator made has no code until its person asks for one
// with their password; no administrator surface ever shows one.
func TestAnAccountWithoutACodeCannotBeRecovered(t *testing.T) {
	x := tenants(t)
	if err := x.runtime.Authority.AddPerson("made", password, x.aliceSet, store.Grants{Read: true}, false); err != nil {
		t.Fatal(err)
	}
	if w := x.form(t, "/oauth/account/recover", url.Values{"account": {"made"}, "code": {""}, "new_password": {"a-new-password-here"}}); w.Code != 401 {
		t.Fatal("an account with no recovery code was recovered with an empty one:", w.Code)
	}
}
