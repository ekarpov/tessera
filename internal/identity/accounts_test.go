package identity

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ekarpov/tessera/pkg/store"
)

// consent drives the authorization-code flow for an account and returns the
// token response, or the status the flow stopped at.
func consent(t *testing.T, a *Authority, account, secret, dataset string) (map[string]any, int) {
	t.Helper()
	if _, ok := a.data.Clients["device"]; !ok {
		a.data.Clients["device"] = Client{ID: "device", Name: "Device", Redirects: []string{"https://device.example/callback"}}
	}
	verifier := strings.Repeat("d", 43)
	hash := sha256.Sum256([]byte(verifier))
	query := url.Values{"prompt": {"consent"}, "client_id": {"device"}, "redirect_uri": {"https://device.example/callback"}, "response_type": {"code"},
		"code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(hash[:])}, "state": {"s"}, "dataset": {dataset}}
	before := map[string]bool{}
	for existing := range a.flows {
		before[existing] = true
	}
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/oauth/authorize?"+query.Encode(), nil))
	if w.Code != 200 {
		return nil, w.Code
	}
	// The flow this request created, not any unfinished one: a refused sign-in
	// leaves its flow behind, and picking by ranging over the map chose the
	// stale one about half the time.
	var id string
	for candidate := range a.flows {
		if !before[candidate] {
			id = candidate
		}
	}
	w = formCall(a, "/oauth/authorize", url.Values{"flow": {id}, "csrf": {a.flows[id].CSRF}, "username": {account},
		"password": {secret}, "decision": {"continue"}}, w.Result().Cookies()[0])
	if w.Code != 200 {
		return nil, w.Code
	}
	w = formCall(a, "/oauth/authorize", url.Values{"flow": {id}, "csrf": {a.flows[id].CSRF}, "decision": {"allow"}, "read": {"yes"}},
		w.Result().Cookies()[0])
	if w.Code != 303 {
		return nil, w.Code
	}
	redirect, _ := url.Parse(w.Header().Get("Location"))
	w = formCall(a, "/oauth/token", url.Values{"grant_type": {"authorization_code"}, "client_id": {"device"},
		"redirect_uri": {"https://device.example/callback"}, "code": {redirect.Query().Get("code")}, "code_verifier": {verifier}}, nil)
	var tokens map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &tokens) != nil {
		return nil, w.Code
	}
	return tokens, 200
}

// Suspension covers the ways an account obtains new credentials, not only the
// credentials it already has: a refresh token mints access tokens, and signing
// in starts a new approval. Checking bearer tokens alone would leave a
// suspended account able to keep refreshing, with every refreshed token then
// refused — a server that says yes and then no.
func TestASuspendedAccountCannotRefreshOrSignIn(t *testing.T) {
	a, _, dataset := authFixture(t)
	const secret = "synthetic-member-password"
	if err := a.AddPerson("member", secret, dataset, store.Grants{Read: true}, false); err != nil {
		t.Fatal(err)
	}
	tokens, code := consent(t, a, "member", secret, dataset)
	if code != 200 {
		t.Fatal("the fixture's approval did not complete", code)
	}
	if err := a.SuspendAccount("member"); err != nil {
		t.Fatal(err)
	}
	refresh := url.Values{"grant_type": {"refresh_token"}, "client_id": {"device"}, "refresh_token": {tokens["refresh_token"].(string)}}
	// Refused, and told why: whoever holds a valid refresh token is the
	// account, and "sign in again" would send it somewhere that cannot help.
	if w := formCall(a, "/oauth/token", refresh, nil); w.Code != 400 || !strings.Contains(w.Body.String(), `"account_suspended"`) {
		t.Fatal("a suspended account's refresh was not refused as suspended", w.Code, w.Body.String())
	}
	if _, code = consent(t, a, "member", secret, dataset); code != 403 {
		t.Fatal("a suspended account's sign-in was not refused as suspended", code)
	}
	// Without the password, nothing is said about suspension.
	if _, code = consent(t, a, "member", "not-the-password-at-all", dataset); code != 401 {
		t.Fatal("a wrong password was told the account is suspended", code)
	}
	// Nor with a refresh token that is not valid.
	bogus := url.Values{"grant_type": {"refresh_token"}, "client_id": {"device"}, "refresh_token": {"not-a-refresh-token"}}
	if w := formCall(a, "/oauth/token", bogus, nil); strings.Contains(w.Body.String(), "account_suspended") {
		t.Fatal("an invalid refresh token was told the account is suspended")
	}
	if err := a.ReinstateAccount("member"); err != nil {
		t.Fatal(err)
	}
	if _, code = consent(t, a, "member", secret, dataset); code != 200 {
		t.Fatal("a reinstated account could not sign in", code)
	}
	// The refresh token an application tried while suspended must still work
	// afterwards. A refusal that also spent it would make the next attempt look
	// like reuse, and reuse detection revokes the installation for good — so an
	// app retrying in the background during a suspension would be permanently
	// cut off by it, which is not what reinstating promises.
	if w := formCall(a, "/oauth/token", refresh, nil); w.Code != 200 {
		t.Fatal("a refresh token tried during a suspension no longer works after reinstatement", w.Code, w.Body.String())
	}
}

// A reset password is the one that signs in, the old one no longer does, and
// the account's existing sign-ins survive: a forgotten password is not a
// compromised one.
func TestAResetPasswordSignsInAndTheOldOneDoesNot(t *testing.T) {
	a, _, dataset := authFixture(t)
	const before, after = "synthetic-forgotten-password", "synthetic-replacement-password"
	if err := a.AddPerson("member", before, dataset, store.Grants{Read: true}, false); err != nil {
		t.Fatal(err)
	}
	existing, err := a.AddAutomation(t.Context(), "member", dataset)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.ResetPassword("member", "short", "admin"); err == nil {
		t.Fatal("a password under twelve characters was accepted")
	}
	if err = a.ResetPassword("member", after, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, code := consent(t, a, "member", before, dataset); code != 401 {
		t.Fatal("the old password still signs in", code)
	}
	if _, code := consent(t, a, "member", after, dataset); code != 200 {
		t.Fatal("the new password does not sign in", code)
	}
	if _, err = a.Authenticate(t.Context(), existing); err != nil {
		t.Fatal("resetting a password signed the account out of an existing sign-in", err)
	}
}
