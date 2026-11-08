package security_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"github.com/ekarpov/tessera/pkg/store"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// signIn drives the real authorization flow for an account — registration,
// sign-in, consent, code exchange — and returns the access token. administer
// ticks the "administer on your behalf" box, which is the only way a token
// comes to carry that right.
func (x *tenancy) signIn(t *testing.T, account, dataset string, administer bool) string {
	t.Helper()
	return x.signInAs(t, account, password, dataset, administer)
}

// signInWith signs in with a password other than the fixture's, approving
// read and nothing else.
func (x *tenancy) signInWith(t *testing.T, account, secret, dataset string) string {
	t.Helper()
	return x.signInAs(t, account, secret, dataset, false)
}

func (x *tenancy) signInAs(t *testing.T, account, secret, dataset string, administer bool) string {
	t.Helper()
	approval := url.Values{"read": {"yes"}}
	if administer {
		approval.Set("administer", "yes")
	}
	return x.signInApproving(t, account, secret, dataset, approval)
}

// signInGrants signs in with the fixture's password, approving exactly what
// approval ticks.
func (x *tenancy) signInGrants(t *testing.T, account, dataset string, approval url.Values) string {
	t.Helper()
	return x.signInApproving(t, account, password, dataset, approval)
}

func (x *tenancy) signInApproving(t *testing.T, account, secret, dataset string, approval url.Values) string {
	t.Helper()
	return x.authorize(t, account, secret, url.Values{"dataset": {dataset}}, approval).Access
}

// authorized is what one completed authorization hands the application, and
// the approval page the person was shown on the way.
type authorized struct {
	Access, Refresh, Client, Consent string
}

// authorize drives the authorization flow with extra request parameters.
func (x *tenancy) authorize(t *testing.T, account, secret string, extra url.Values, approval url.Values) authorized {
	t.Helper()
	got, status := x.authorizeStatus(t, account, secret, extra, approval)
	if status != 303 {
		t.Fatal("approval failed", status)
	}
	return got
}

// authorizeRefused drives the flow and requires the approval to be refused.
func (x *tenancy) authorizeRefused(t *testing.T, account, secret string, extra url.Values, approval url.Values) {
	t.Helper()
	if _, status := x.authorizeStatus(t, account, secret, extra, approval); status == 303 {
		t.Fatal("an approval that should have been refused was given")
	}
}

func (x *tenancy) authorizeStatus(t *testing.T, account, secret string, extra url.Values, approval url.Values) (authorized, int) {
	t.Helper()
	serve := func(r *http.Request) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		x.runtime.Handler.ServeHTTP(w, r)
		return w
	}
	form := func(path string, values url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", path, strings.NewReader(values.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if cookie != nil {
			r.AddCookie(cookie)
		}
		return serve(r)
	}
	hidden := func(page, name string) string {
		_, after, found := strings.Cut(page, `name="`+name+`" value="`)
		if !found {
			t.Fatal("no", name, "on the page")
		}
		value, _, _ := strings.Cut(after, `"`)
		return value
	}
	registration := serve(httptest.NewRequest("POST", "/oauth/register",
		strings.NewReader(`{"client_name":"Console","redirect_uris":["https://console.example/callback"]}`)))
	var client struct {
		ID string `json:"client_id"`
	}
	if registration.Code != 201 || json.Unmarshal(registration.Body.Bytes(), &client) != nil {
		t.Fatal("could not register a client", registration.Code)
	}
	verifier := strings.Repeat("c", 43)
	hash := sha256.Sum256([]byte(verifier))
	query := url.Values{"client_id": {client.ID}, "redirect_uri": {"https://console.example/callback"}, "response_type": {"code"},
		"code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(hash[:])}, "state": {"s"}}
	for name, values := range extra {
		query[name] = values
	}
	start := serve(httptest.NewRequest("GET", "/oauth/authorize?"+query.Encode(), nil))
	if start.Code != 200 {
		t.Fatal("authorization did not start", start.Code)
	}
	page := start.Body.String()
	flow := hidden(page, "flow")
	signedIn := form("/oauth/authorize", url.Values{"flow": {flow}, "csrf": {hidden(page, "csrf")}, "username": {account},
		"password": {secret}, "decision": {"continue"}}, start.Result().Cookies()[0])
	if signedIn.Code != 200 {
		t.Fatal("sign-in failed", signedIn.Code)
	}
	consent := url.Values{"flow": {flow}, "csrf": {hidden(signedIn.Body.String(), "csrf")}, "decision": {"allow"}}
	for name, values := range approval {
		consent[name] = values
	}
	approved := form("/oauth/authorize", consent, signedIn.Result().Cookies()[0])
	if approved.Code != 303 {
		return authorized{}, approved.Code
	}
	redirect, _ := url.Parse(approved.Header().Get("Location"))
	exchanged := form("/oauth/token", url.Values{"grant_type": {"authorization_code"}, "client_id": {client.ID},
		"redirect_uri": {"https://console.example/callback"}, "code": {redirect.Query().Get("code")}, "code_verifier": {verifier}}, nil)
	var tokens struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
	}
	if exchanged.Code != 200 || json.Unmarshal(exchanged.Body.Bytes(), &tokens) != nil {
		t.Fatal("code exchange failed", exchanged.Code)
	}
	return authorized{Access: tokens.Access, Refresh: tokens.Refresh, Client: client.ID, Consent: signedIn.Body.String()}, 303
}

func (x *tenancy) admin(t *testing.T, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, "/admin/api/v1"+path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	x.runtime.Handler.ServeHTTP(w, r)
	return w
}

// A token administers only when the person approving it said so. An
// administrator's own dashboard, approved to read, did not consent to adding
// people or deleting accounts and must not be able to.
func TestAnAdministratorsTokenAdministersOnlyWhenApprovedTo(t *testing.T) {
	x := tenants(t)
	reading := x.signIn(t, x.alice, x.aliceSet, false)
	if got := x.admin(t, "GET", "/overview", reading, "").Code; got != 403 {
		t.Fatal("a token approved without the administer right reached the administration API:", got)
	}
	if got := x.admin(t, "GET", "/overview", x.aliceTok, "").Code; got != 403 {
		t.Fatal("an automation token reached the administration API:", got)
	}
	if got := x.admin(t, "GET", "/overview", "not-a-token", "").Code; got != 401 {
		t.Fatal("an unknown token was not refused as unauthenticated:", got)
	}
	administering := x.signIn(t, x.alice, x.aliceSet, true)
	w := x.admin(t, "GET", "/overview", administering, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), x.aliceSet) || !strings.Contains(w.Body.String(), `"usage"`) {
		t.Fatal("the administration overview did not answer with the administrator's dataset and its usage:", w.Code, w.Body.String())
	}
}

// Adding a reader names its types in the same request, so there is no moment
// at which the new account reads the whole dataset. Replacing the list and
// withdrawing the reader work by account, not by an installation identifier
// nobody sees.
func TestAReaderIsAddedWithItsTypesAndNeverReadsMore(t *testing.T) {
	x := tenants(t)
	shared, private := "security.shared", "security.private"
	for _, typeID := range []string{shared, private} {
		if w := x.call(t, "POST", "/v1/datasets/"+x.aliceSet+"/meta", x.aliceTok,
			`{"type":{"id":"`+typeID+`","name":"R","fields":[{"id":"n","meta_id":"`+typeID+`","type":"integer"}]}}`, nil); w.Code != 201 {
			t.Fatal("could not define", typeID, w.Code)
		}
	}
	admin := x.signIn(t, x.alice, x.aliceSet, true)
	if w := x.admin(t, "PUT", "/datasets/"+x.aliceSet+"/readers/relative", admin,
		`{"types":["`+shared+`"],"password":"`+password+`"}`); w.Code != 204 {
		t.Fatal("could not add a reader:", w.Code, w.Body.String())
	}
	reads := func(token, typeID string) int {
		return x.call(t, "POST", "/v1/datasets/"+x.aliceSet+"/objects/"+typeID+":query", token, `{"query":{"fields":["n"]}}`, nil).Code
	}
	// The reader's very first sign-in is already scoped.
	first := x.signIn(t, "relative", x.aliceSet, false)
	if reads(first, shared) != 200 || reads(first, private) < 400 {
		t.Fatal("a reader's first sign-in was not limited to its types")
	}
	listed := x.admin(t, "GET", "/datasets/"+x.aliceSet+"/readers", admin, "")
	if listed.Code != 200 || !strings.Contains(listed.Body.String(), `"relative"`) || !strings.Contains(listed.Body.String(), shared) {
		t.Fatal("the reader is not listed with its types:", listed.Body.String())
	}
	// Replacing the list.
	if w := x.admin(t, "PUT", "/datasets/"+x.aliceSet+"/readers/relative", admin, `{"types":["`+private+`"]}`); w.Code != 204 {
		t.Fatal("could not replace a reader's types:", w.Code)
	}
	if reads(first, private) != 200 || reads(first, shared) < 400 {
		t.Fatal("replacing the type list did not reach the reader's existing sign-in")
	}
	// Withdrawing the reader.
	if w := x.admin(t, "DELETE", "/datasets/"+x.aliceSet+"/readers/relative", admin, ""); w.Code != 204 {
		t.Fatal("could not remove a reader:", w.Code)
	}
	if reads(first, private) < 400 {
		t.Fatal("a removed reader still reads")
	}
	// A writer is not a reader, and cannot be turned into one by accident.
	if w := x.admin(t, "PUT", "/datasets/"+x.aliceSet+"/readers/"+x.alice, admin, `{"types":["`+shared+`"]}`); w.Code != 409 {
		t.Fatal("an account that writes was turned into a reader:", w.Code)
	}
}

// Standing is the administrator's, dataset by dataset and account by account,
// exactly as on the pages; and what belongs to the server is for the server's
// administrator only.
func TestTheAdministrationAPIActsWithinTheAdministratorsStanding(t *testing.T) {
	x := tenants(t)
	aliceAdmin := x.signIn(t, x.alice, x.aliceSet, true)
	bobAdmin := x.signIn(t, x.bob, x.bobSet, true)
	if w := x.admin(t, "PUT", "/datasets/"+x.aliceSet+"/readers/intruder", bobAdmin,
		`{"types":["security.anything"],"password":"`+password+`"}`); w.Code != 403 {
		t.Fatal("an administrator of one dataset added a reader to another:", w.Code)
	}
	if w := x.admin(t, "GET", "/datasets/"+x.aliceSet+"/readers", bobAdmin, ""); w.Code != 403 {
		t.Fatal("an administrator of one dataset listed another's readers:", w.Code)
	}
	if w := x.admin(t, "POST", "/accounts", aliceAdmin,
		`{"account":"member","password":"`+password+`","dataset":"`+x.aliceSet+`","grants":{"read":true}}`); w.Code != 201 {
		t.Fatal("could not add an account:", w.Code, w.Body.String())
	}
	if w := x.admin(t, "POST", "/accounts", aliceAdmin,
		`{"account":"stray","password":"`+password+`","dataset":"`+x.bobSet+`","grants":{"read":true}}`); w.Code != 403 {
		t.Fatal("an account was added to a dataset its administrator does not hold:", w.Code)
	}
	if w := x.admin(t, "POST", "/accounts/member/suspend", aliceAdmin, ""); w.Code != 204 {
		t.Fatal("could not suspend an account:", w.Code)
	}
	if w := x.admin(t, "POST", "/accounts/member/suspend", bobAdmin, ""); w.Code != 403 {
		t.Fatal("an administrator acted on an account in a dataset they do not administer:", w.Code)
	}
	if w := x.admin(t, "POST", "/accounts/"+x.alice+"/delete", aliceAdmin, ""); w.Code != 403 {
		t.Fatal("an administrator acted on their own account:", w.Code)
	}
	for _, serverOnly := range []struct{ method, path, body string }{
		{"PUT", "/datasets/" + x.aliceSet + "/limits", `{"max_page_size":10}`},
		{"POST", "/issuers", `{"id":"x","datasets":["` + x.aliceSet + `"],"grants":{"read":true}}`},
		{"POST", "/backups", ""},
	} {
		if w := x.admin(t, serverOnly.method, serverOnly.path, aliceAdmin, serverOnly.body); w.Code != 403 {
			t.Fatal("a dataset administrator reached a server operation:", serverOnly.path, w.Code)
		}
	}
}

// The server's administrator can change a dataset's limits and configure an
// issuer through the API; the refusals above are only half of the rule.
func TestTheServerAdministratorManagesLimitsAndIssuers(t *testing.T) {
	x := tenants(t)
	if err := x.runtime.Authority.AddServerAdministrator("operator", password, x.aliceSet,
		store.Grants{Read: true, Write: true, Schema: true, Erase: true}, false); err != nil {
		t.Fatal(err)
	}
	operator := x.signIn(t, "operator", x.aliceSet, true)
	w := x.admin(t, "PUT", "/datasets/"+x.aliceSet+"/limits", operator, `{"max_materialized_bytes":134217728}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"max_materialized_bytes":134217728`) {
		t.Fatal("the server administrator could not raise a dataset's limit:", w.Code, w.Body.String())
	}
	// Lengthened, then an attempt to bring it back: the second is a real
	// shortening of a window clients were promised.
	if w = x.admin(t, "PUT", "/datasets/"+x.aliceSet+"/limits", operator, `{"idempotency_retention_seconds":172800}`); w.Code != 200 {
		t.Fatal("the retention window could not be lengthened:", w.Code)
	}
	if w = x.admin(t, "PUT", "/datasets/"+x.aliceSet+"/limits", operator, `{"idempotency_retention_seconds":100000}`); w.Code != 409 {
		t.Fatal("a promised retention window was shortened through the API:", w.Code)
	}
	w = x.admin(t, "POST", "/issuers", operator, `{"id":"portal","datasets":["`+x.aliceSet+`"],"grants":{"read":true}}`)
	if w.Code != 201 || !strings.Contains(w.Body.String(), `"secret"`) {
		t.Fatal("the server administrator could not configure an issuer:", w.Code, w.Body.String())
	}
	w = x.admin(t, "POST", "/issuers", operator, `{"id":"study","datasets":["`+x.aliceSet+`"],"grants":{"read":true},"assign_names":true}`)
	var configured struct {
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &configured); err != nil || w.Code != 201 {
		t.Fatal("an issuer assigning names could not be configured:", w.Code)
	}
	if _, assigned, status := x.inviteNaming(t, "study", configured.Secret, "p-1"); status != 201 || assigned != "p-1" {
		t.Fatal("an issuer configured through the API to assign names could not:", status, assigned)
	}
}
