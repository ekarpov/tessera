package identity

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
	"github.com/ekarpov/tessera/pkg/store/memory"
)

func authFixture(t *testing.T) (*Authority, *memory.Store, string) {
	t.Helper()
	engine, err := memory.New(p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	// The authority serves the server; each grant names its own dataset, which
	// composition resolves before any grant exists to name it with.
	provisioned, err := engine.SoleDataset(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	dataset := provisioned.ID
	a, err := Open(t.Context(), filepath.Join(t.TempDir(), "identity.json"), "https://tessera.example", engine)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.AddPerson("person", "synthetic-password-only", dataset, store.Grants{Read: true, Write: true, Schema: true, Erase: true}, false); err != nil {
		t.Fatal(err)
	}
	return a, engine, dataset
}
func formCall(a *Authority, path string, values url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	return w
}
func TestPKCERefreshRotationAndRevocation(t *testing.T) {
	a, engine, dataset := authFixture(t)
	a.data.Clients["client"] = Client{ID: "client", Name: "Unverified", Redirects: []string{"https://client.example/callback"}}
	verifier := strings.Repeat("x", 43)
	hash := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(hash[:])
	query := url.Values{"client_id": {"client"}, "redirect_uri": {"https://client.example/callback"}, "response_type": {"code"}, "code_challenge_method": {"S256"}, "code_challenge": {challenge}, "state": {"bound-state"}, "dataset": {dataset}}
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/oauth/authorize?"+query.Encode(), nil))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var id string
	var f flow
	for id, f = range a.flows {
		break
	}
	cookie := w.Result().Cookies()[0]
	// Signing in and approving are two steps now. The first establishes who is
	// here, which is what lets the second offer the datasets they may reach.
	signIn := url.Values{"flow": {id}, "csrf": {f.CSRF}, "username": {"person"}, "password": {"synthetic-password-only"}, "decision": {"continue"}}
	if bad := formCall(a, "/oauth/authorize", signIn, nil); bad.Code != 400 {
		t.Fatal("missing CSRF cookie accepted")
	}
	w = formCall(a, "/oauth/authorize", signIn, cookie)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `data-page="consent"`) {
		t.Fatal("signing in did not reach the approval screen", w.Code)
	}
	// A token that survived the first step must not be spendable at the second.
	stale := url.Values{"flow": {id}, "csrf": {f.CSRF}, "decision": {"allow"}, "read": {"yes"}, "write": {"yes"}}
	if bad := formCall(a, "/oauth/authorize", stale, cookie); bad.Code != 400 {
		t.Fatal("a sign-in token was accepted as an approval token", bad.Code)
	}
	stepTwo := a.flows[id]
	consentCookie := w.Result().Cookies()[0]
	consent := url.Values{"flow": {id}, "csrf": {stepTwo.CSRF}, "decision": {"allow"}, "read": {"yes"}, "write": {"yes"}}
	w = formCall(a, "/oauth/authorize", consent, consentCookie)
	if w.Code != 303 {
		t.Fatal(w.Code, w.Body.String())
	}
	redirect, _ := url.Parse(w.Header().Get("Location"))
	if redirect.Query().Get("state") != "bound-state" || redirect.Query().Get("iss") != a.issuer {
		t.Fatal("authorization response unbound")
	}
	exchange := url.Values{"grant_type": {"authorization_code"}, "client_id": {"client"}, "redirect_uri": {"https://client.example/callback"}, "code": {redirect.Query().Get("code")}, "code_verifier": {"wrong"}}
	if w = formCall(a, "/oauth/token", exchange, nil); w.Code != 400 {
		t.Fatal("wrong verifier accepted")
	}
	exchange.Set("code_verifier", verifier)
	w = formCall(a, "/oauth/token", exchange, nil)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var tokens map[string]any
	if json.Unmarshal(w.Body.Bytes(), &tokens) != nil {
		t.Fatal("token response")
	}
	access, err := a.Authenticate(t.Context(), tokens["access_token"].(string))
	if err != nil {
		t.Fatal(err)
	}
	// The listing is the application's own view of what it may use, which is
	// also where the dataset it was granted is named.
	held, err := engine.Datasets(t.Context(), access)
	if err != nil || len(held) != 1 {
		t.Fatal("application was granted no dataset", err)
	}
	if held[0].ID != dataset {
		t.Fatal("the grant landed on a different dataset")
	}
	if info := held[0]; !info.Access.Write || info.Access.Erase || info.Access.Schema {
		t.Fatal("application inherited unapproved privileges", info.Access)
	}
	if w = formCall(a, "/oauth/token", exchange, nil); w.Code != 400 {
		t.Fatal("code reused")
	}
	refresh := url.Values{"grant_type": {"refresh_token"}, "client_id": {"client"}, "refresh_token": {tokens["refresh_token"].(string)}}
	w = formCall(a, "/oauth/token", refresh, nil)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	var rotated map[string]any
	json.Unmarshal(w.Body.Bytes(), &rotated)
	if w = formCall(a, "/oauth/token", refresh, nil); w.Code != 400 {
		t.Fatal("stolen refresh replay accepted")
	}
	if _, err = a.Authenticate(t.Context(), rotated["access_token"].(string)); p.ErrorCode(err) != "authentication_failed" {
		t.Fatal("refresh replay did not revoke family")
	}
	// A revoked authorization holds no grant, so the dataset is concealed
	// rather than refused: it may not learn the dataset is still there.
	if _, err = engine.Info(t.Context(), access, dataset); p.ErrorCode(err) != "unavailable" {
		t.Fatal("revoked authorization retained access")
	}
	reopened, err := Open(t.Context(), a.path, a.issuer, engine)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reopened.Authenticate(t.Context(), tokens["access_token"].(string)); p.ErrorCode(err) != "authentication_failed" {
		t.Fatal("restart undid revocation")
	}
}
func TestServiceTokensKeepRecoveryIdentity(t *testing.T) {
	a, engine, dataset := authFixture(t)
	secret, err := a.AddService("service", dataset, store.Grants{Read: true, Write: true, Schema: true, Erase: true})
	if err != nil {
		t.Fatal(err)
	}
	var first store.Access
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest("POST", "/oauth/token", strings.NewReader("grant_type=client_credentials&dataset="+dataset))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.SetBasicAuth("service", secret)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		var result map[string]any
		json.Unmarshal(w.Body.Bytes(), &result)
		access, e := a.Authenticate(t.Context(), result["access_token"].(string))
		if e != nil {
			t.Fatal(e)
		}
		if i == 0 {
			first = access
		} else if first != access {
			t.Fatal("service token refresh orphaned receipts")
		}
		held, e := engine.Datasets(t.Context(), access)
		if e != nil || len(held) != 1 || access.Principal != "service" || held[0].Access.Erase {
			t.Fatal("service identity borrowed privileges", e)
		}
	}
}
func TestRedirectAndRegistrationLimits(t *testing.T) {
	for _, uri := range []string{"javascript:alert(1)", "http://example.org/callback", "https://example.org/callback#fragment"} {
		if validRedirect(uri) {
			t.Fatal("unsafe redirect")
		}
	}
	if !redirectMatches("http://127.0.0.1:1000/callback", "http://127.0.0.1:2000/callback") || redirectMatches("https://example.org/a", "https://example.org/b") {
		t.Fatal("redirect matching")
	}
}

func TestRegistrationMetadataExtensions(t *testing.T) {
	a, _, _ := authFixture(t)
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"extension", `{"redirect_uris":["https://client.example/callback"],"software_id":"example","custom_metadata":{"a":1}}`, 201},
		{"duplicate", `{"redirect_uris":["https://client.example/callback"],"software_id":"one","software_id":"two"}`, 400},
		{"invalid_redirect", `{"redirect_uris":["http://client.example/callback"],"software_id":"example"}`, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			a.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/oauth/register", strings.NewReader(tc.body)))
			if w.Code != tc.want {
				t.Fatalf("got %d, want %d", w.Code, tc.want)
			}
		})
	}
}

// The dataset is chosen at consent when the request did not name one, which is
// the only order that works: GET /v1/datasets is authenticated, so before a
// token exists a client cannot learn an identity to name — and on a first
// connection the dataset was minted for the person and they have never seen it.
func TestAPersonChoosesTheDatasetWhenTheClientCannotName(t *testing.T) {
	a, engine, dataset := authFixture(t)
	a.data.Clients["client"] = Client{ID: "client", Name: "Unverified", Redirects: []string{"https://client.example/callback"}}
	verifier := strings.Repeat("x", 43)
	hash := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(hash[:])

	// No dataset named: the request is accepted, where it used to be refused.
	query := url.Values{"client_id": {"client"}, "redirect_uri": {"https://client.example/callback"},
		"response_type": {"code"}, "code_challenge_method": {"S256"}, "code_challenge": {challenge},
		"state": {"bound-state"}}
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/oauth/authorize?"+query.Encode(), nil))
	if w.Code != 200 {
		t.Fatal("an authorization naming no dataset was refused", w.Code)
	}
	if strings.Contains(w.Body.String(), `name="erase"`) {
		t.Fatal("the sign-in screen offered grants before anyone signed in")
	}
	var id string
	var f flow
	for id, f = range a.flows {
		break
	}
	cookie := w.Result().Cookies()[0]
	w = formCall(a, "/oauth/authorize", url.Values{"flow": {id}, "csrf": {f.CSRF},
		"username": {"person"}, "password": {"synthetic-password-only"}, "decision": {"continue"}}, cookie)
	if w.Code != 200 {
		t.Fatal("sign-in refused", w.Code)
	}
	// The person is offered what their account may reach, and chooses.
	if !strings.Contains(w.Body.String(), dataset) || !strings.Contains(w.Body.String(), "<select name=\"dataset\"") {
		t.Fatal("the approval screen offered no choice of dataset")
	}

	// A dataset this account does not hold is refused however the form was
	// built, so a crafted submission cannot name one never shown.
	second := a.flows[id]
	consentCookie := w.Result().Cookies()[0]
	if bad := formCall(a, "/oauth/authorize", url.Values{"flow": {id}, "csrf": {second.CSRF},
		"decision": {"allow"}, "dataset": {"s-elsewhere"}, "read": {"yes"}}, consentCookie); bad.Code != 401 {
		t.Fatal("a dataset the account does not hold was accepted", bad.Code)
	}
	second = a.flows[id]
	if w = formCall(a, "/oauth/authorize", url.Values{"flow": {id}, "csrf": {second.CSRF},
		"decision": {"allow"}, "dataset": {dataset}, "read": {"yes"}}, consentCookie); w.Code != 303 {
		t.Fatal("the chosen dataset was not authorized", w.Code, w.Body.String())
	}
	held := 0
	for _, auth := range a.data.Authorizations {
		if auth.Dataset == dataset {
			held++
		}
	}
	if held != 1 {
		t.Fatal("the approval did not produce exactly one authorization over the chosen dataset", held)
	}
	_ = engine
}

// One address knocking past its own limit must not spend anybody else's.
// This was first a fix to a server-wide ceiling that counted refused
// arrivals; that ceiling is gone (finding 31), and the property stays.
func TestOneAddressCannotSpendTheSharedRateBudget(t *testing.T) {
	a, _, _ := authFixture(t)
	from := func(ip string) *http.Request {
		r := httptest.NewRequest("POST", "/oauth/token", nil)
		r.RemoteAddr = ip + ":40000"
		return r
	}
	for i := 0; i < 200; i++ {
		a.AdminRateLimited(from("198.51.100.7"), "probe", 10)
	}
	if a.AdminRateLimited(from("203.0.113.9"), "probe", 10) {
		t.Fatal("one address knocking after its own limit locked out every other address")
	}
}

// Verifying a password costs a deliberate 600k-iteration derivation. It ran
// while holding the lock that every API request takes to check its bearer
// token, so an unauthenticated form post stalled ordinary traffic for as long
// as the derivation took.
//
// One derivation runs while token checks are timed against it. Held, some one
// check waits out most of the derivation; released, every check is orders of
// magnitude shorter than it. Both sides are measured in the same run, so
// there is no duration here for a slow or loaded machine to spoil — only the
// ratio between two things that moved together.
func TestAPasswordDerivationDoesNotHoldTheLockEveryRequestNeeds(t *testing.T) {
	atProductionCost(t)
	a, _, dataset := authFixture(t)
	bearer, err := a.AddAutomation(t.Context(), "person", dataset)
	if err != nil {
		t.Fatal(err)
	}
	deriving := make(chan struct{})
	began := time.Now()
	go func() {
		defer close(deriving)
		a.AuthenticateAdministrator("person", "synthetic-wrong-password")
	}()
	worst, served := time.Duration(0), 0
	for {
		select {
		case <-deriving:
			derivation := time.Since(began)
			if served == 0 {
				t.Fatal("no token check ran while the derivation did")
			}
			if worst > derivation/4 {
				t.Fatal("a token check waited on a password derivation", worst, "of", derivation)
			}
			return
		default:
		}
		at := time.Now()
		if _, err = a.Authenticate(t.Context(), bearer); err != nil {
			t.Fatal(err)
		}
		if waited := time.Since(at); waited > worst {
			worst = waited
		}
		served++
	}
}

// Registration reads a body an unauthenticated caller controls the pace of.
// It read it holding the lock every API request takes to check its bearer
// token, so one stalled upload stopped authentication for the whole server.
func TestAStalledRegistrationDoesNotHoldTheLockEveryRequestNeeds(t *testing.T) {
	a, _, dataset := authFixture(t)
	bearer, err := a.AddAutomation(t.Context(), "person", dataset)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	body := &heldBody{entered: entered, release: release, inner: strings.NewReader(`{"redirect_uris":["https://client.example/callback"]}`)}
	request := httptest.NewRequest("POST", "/oauth/register", body)
	request.Header.Set("Content-Type", "application/json")
	registered := make(chan struct{})
	go func() {
		defer close(registered)
		a.Handler().ServeHTTP(httptest.NewRecorder(), request)
	}()
	<-entered
	// The registration is inside the handler with its body half-read. A token
	// check must not be waiting on it.
	done := make(chan error, 1)
	go func() {
		_, e := a.Authenticate(context.Background(), bearer)
		done <- e
	}()
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("a token check waited on a registration body that had not finished arriving")
	}
	close(release)
	<-registered
}

// heldBody is a request body that stops once it has been entered, standing in
// for a client that is sending slowly.
type heldBody struct {
	entered, release chan struct{}
	inner            io.Reader
	waited           bool
}

func (h *heldBody) Read(p []byte) (int, error) {
	if !h.waited {
		h.waited = true
		close(h.entered)
		<-h.release
	}
	return h.inner.Read(p)
}

// Adding an existing account to a dataset needs no password — that is what
// makes one account a member of two. It also means an administrator can
// manufacture the membership, so nothing about that account may become
// administrable merely because it now appears on the attacker's dataset.
//
// A server-level approval carries no dataset, and its reach — every dataset
// it has created — lies outside any one of them. It was briefly administrable
// by whoever administered a dataset its holder belonged to, which the
// manufactured membership made anybody.
func TestAManufacturedMembershipReachesNoServerLevelApproval(t *testing.T) {
	a, _, dataset := authFixture(t)
	if err := a.AddPerson("victim", "synthetic-password-only", "", store.Grants{}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := a.AddAutomation(t.Context(), "victim", ""); err != nil {
		t.Fatal(err)
	}
	target := ""
	for id, auth := range a.data.Authorizations {
		if auth.Principal == "victim" && auth.Dataset == "" && auth.CreateDataset {
			target = id
		}
	}
	if target == "" {
		t.Fatal("fixture should hold a server-level approval")
	}
	// The attacker administers a dataset, and puts the victim on it. No
	// password, and none needed: this is the ordinary way to share.
	if err := a.AddPerson("victim", "ignored-by-an-existing-account", dataset, store.Grants{Read: true}, false); err != nil {
		t.Fatal(err)
	}
	if may, err := a.MayAdminister(t.Context(), "person", target); err != nil || may {
		t.Fatal("a membership the administrator arranged gave them a server-level credential", err)
	}
	for _, entry := range a.AdminOverview(t.Context(), "person").Authorizations {
		if entry.ID == target {
			t.Fatal("a server-level approval was reported on a dataset administrator's page")
		}
	}
	// And the dataset approvals on their own dataset remain theirs to act on,
	// so this did not simply refuse everything.
	own, err := a.AddAutomation(t.Context(), "person", dataset)
	if err != nil || own == "" {
		t.Fatal(err)
	}
	reachable := false
	for _, entry := range a.AdminOverview(t.Context(), "person").Authorizations {
		if may, e := a.MayAdminister(t.Context(), "person", entry.ID); e == nil && may {
			reachable = true
		}
	}
	if !reachable {
		t.Fatal("an administrator can no longer act on their own dataset's approvals")
	}
}

// A registry from a newer release is refused rather than read and saved back
// without what it carried. That is the defect this version number exists for,
// in the direction that can still happen: a build that cannot name a member
// cannot preserve it, and the previous release dropped a member it could not
// name without anything noticing.
func TestARegistryFromANewerReleaseIsRefused(t *testing.T) {
	engine, err := memory.New(p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "identity.json")
	newer := `{"Version":99,"Principals":{},"Clients":{},"Authorizations":{},"Tokens":{},"Codes":{}}`
	if err = os.WriteFile(path, []byte(newer), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Open(t.Context(), path, "https://tessera.example", engine); err == nil {
		t.Fatal("a registry from a newer release was accepted, and the next save would have stripped it")
	}
	// A registry this build wrote opens, and one written before versions
	// existed is read as it stands rather than refused.
	for _, content := range []string{
		`{"Version":1,"Principals":{},"Clients":{},"Authorizations":{},"Tokens":{},"Codes":{}}`,
		`{"Principals":{},"Clients":{},"Authorizations":{},"Tokens":{},"Codes":{}}`,
	} {
		if err = os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err = Open(t.Context(), path, "https://tessera.example", engine); err != nil {
			t.Fatal("a readable registry was refused", err)
		}
	}
}

// When the last backup was taken outlives a restart: an administrator told
// "no backup" after one would doubt the backups there are.
func TestTheLastBackupIsRememberedAcrossARestart(t *testing.T) {
	engine, err := memory.New(p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "identity.json")
	a, err := Open(t.Context(), path, "https://tessera.example", engine)
	if err != nil {
		t.Fatal(err)
	}
	if !a.LastBackup().IsZero() {
		t.Fatal("a server that never backed up reports a backup")
	}
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err = a.RecordBackup(at); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(t.Context(), path, "https://tessera.example", engine)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.LastBackup().Equal(at) {
		t.Fatal("the last backup was forgotten across a restart:", reopened.LastBackup(), at)
	}
}
