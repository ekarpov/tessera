package identity

import (
	"encoding/json"
	"net/http/httptest"

	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
	"github.com/ekarpov/tessera/pkg/store/sqlite"
)

func consoleConfig() ConsoleConfig {
	return ConsoleConfig{ClientID: "operator-console", Name: "Operator Console", Origin: "https://console.example", RedirectURI: "https://console.example/callback", DataGrants: []string{"read", "write", "insert"}, Administer: true}
}

func consoleLogin(t *testing.T, a *Authority, remember bool) (store.Access, map[string]any) {
	t.Helper()
	q := pageQuery("")
	q.Del("dataset")
	q.Set("client_id", "operator-console")
	q.Set("redirect_uri", consoleConfig().RedirectURI)
	w := signInRequest(t, a, q, map[string]string{"Accept": "text/html"})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `class="signin-card"`) || strings.Contains(w.Body.String(), `name="remember" value="yes" checked`) || strings.Count(w.Body.String(), "<button") != 1 {
		t.Fatal("not the compact unchecked sign-in")
	}
	var id string
	for id = range a.flows {
	}
	form := url.Values{"flow": {id}, "csrf": {a.flows[id].CSRF}, "username": {"person"}, "password": {"synthetic-password-only"}, "decision": {"continue"}, "schema": {"yes"}, "erase": {"yes"}, "create_dataset": {"yes"}}
	if remember {
		form.Set("remember", "yes")
	}
	cookie := w.Result().Cookies()[0]
	w = browserPost(a, form, cookie)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `http-equiv="refresh"`) || strings.Contains(w.Body.String(), `data-page="consent"`) {
		t.Fatal("Console did not complete in one submission", w.Code, w.Body.String())
	}
	target, _ := url.Parse(pageLink(t, w))
	if target.Query().Get("state") != "s" || target.Query().Get("iss") != a.issuer {
		t.Fatal("callback unbound")
	}
	tokens := formCall(a, "/oauth/token", url.Values{"grant_type": {"authorization_code"}, "client_id": {"operator-console"}, "redirect_uri": {consoleConfig().RedirectURI}, "code": {target.Query().Get("code")}, "code_verifier": {strings.Repeat("x", 43)}, "remember": {"yes"}}, nil)
	var result map[string]any
	if tokens.Code != 200 || json.Unmarshal(tokens.Body.Bytes(), &result) != nil {
		t.Fatal("code exchange failed", tokens.Code, tokens.Body.String())
	}
	session, ok := result["console_session"].(map[string]any)
	if !ok || session["origin"] != consoleConfig().Origin || session["keep_signed_in"] != remember {
		t.Fatal("remember not bound to code exchange", session)
	}
	age := float64(0)
	if remember {
		age = 30 * 24 * 60 * 60
	}
	if session["max_age_seconds"] != age || result["expires_in"] != float64(300) {
		t.Fatal("remember changed token lifetime")
	}
	if replay := formCall(a, "/oauth/token", url.Values{"grant_type": {"authorization_code"}, "client_id": {"operator-console"}, "redirect_uri": {consoleConfig().RedirectURI}, "code": {target.Query().Get("code")}, "code_verifier": {strings.Repeat("x", 43)}}, nil); replay.Code != 400 {
		t.Fatal("Console code reused")
	}
	if replay := browserPost(a, form, cookie); replay.Code != 400 || !strings.Contains(replay.Body.String(), `data-page="restart"`) || len(a.data.Authorizations) != 1 {
		t.Fatal("Console form replay repeated grant")
	}
	access, err := a.Authenticate(t.Context(), result["access_token"].(string))
	if err != nil {
		t.Fatal(err)
	}
	return access, result
}

func TestConsoleCurrentAuthorityAndRemember(t *testing.T) {
	for _, remember := range []bool{false, true} {
		t.Run(map[bool]string{true: "remember", false: "session"}[remember], func(t *testing.T) {
			a, engine, dataset := authFixture(t)
			if err := a.SetConsoles([]ConsoleConfig{consoleConfig()}); err != nil {
				t.Fatal(err)
			}
			access, tokens := consoleLogin(t, a, remember)
			info, err := engine.Info(t.Context(), access, dataset)
			if err != nil || !info.Access.Read || !info.Access.Write || info.Access.Schema || info.Access.Erase {
				t.Fatal("unexpected Console grants", err, info.Access)
			}
			if !a.data.Authorizations[access.Authorization].Administer {
				t.Fatal("account-held administration missing")
			}
			if err := a.GrantDataset("person", dataset, store.Grants{Read: true}); err != nil {
				t.Fatal(err)
			}
			info, err = engine.Info(t.Context(), access, dataset)
			if err != nil || info.Access.Write || a.data.Authorizations[access.Authorization].Administer {
				t.Fatal("role withdrawal left authority", err, info.Access)
			}
			principal := a.data.Principals["person"]
			principal.Suspended = true
			a.data.Principals["person"] = principal
			if _, err = a.Authenticate(t.Context(), tokens["access_token"].(string)); p.ErrorCode(err) != "authentication_failed" {
				t.Fatal("suspension bypassed")
			}
			principal.Suspended = false
			a.data.Principals["person"] = principal
			refresh := formCall(a, "/oauth/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {"operator-console"}, "refresh_token": {tokens["refresh_token"].(string)}, "remember": {"yes"}}, nil)
			if refresh.Code != 200 || strings.Contains(refresh.Body.String(), "console_session") {
				t.Fatal("refresh changed persistence choice")
			}
			if err = a.revoke(t.Context(), access.Authorization); err != nil {
				t.Fatal(err)
			}
			if _, err = a.Authenticate(t.Context(), tokens["access_token"].(string)); p.ErrorCode(err) != "authentication_failed" {
				t.Fatal("revocation bypassed")
			}
		})
	}
}

func TestConsolePolicyCannotBeClaimedOrExpanded(t *testing.T) {
	a, _, dataset := authFixture(t)
	a.data.Clients["ordinary"] = Client{ID: "ordinary", Name: "Operator Console", Redirects: []string{consoleConfig().RedirectURI}}
	bad := consoleConfig()
	bad.ClientID = "ordinary"
	if a.SetConsoles([]ConsoleConfig{bad}) == nil {
		t.Fatal("dynamically registered client promoted")
	}
	for _, mutate := range []func(*ConsoleConfig){
		func(c *ConsoleConfig) { c.Origin = "http://console.example" },
		func(c *ConsoleConfig) { c.Origin += "/" },
		func(c *ConsoleConfig) { c.RedirectURI = "https://attacker.example/callback" },
		func(c *ConsoleConfig) { c.RedirectURI += "#fragment" },
		func(c *ConsoleConfig) { c.DataGrants = []string{"erase"} },
		func(c *ConsoleConfig) { c.DataGrants = []string{"schema"} },
		func(c *ConsoleConfig) { c.DataGrants = []string{"read", "read"} },
	} {
		c := consoleConfig()
		mutate(&c)
		if a.SetConsoles([]ConsoleConfig{c}) == nil {
			t.Fatal("unsafe Console policy accepted")
		}
	}
	if err := a.SetConsoles([]ConsoleConfig{consoleConfig()}); err != nil {
		t.Fatal(err)
	}
	q := pageQuery(dataset)
	q.Set("client_id", "ordinary")
	q.Set("redirect_uri", consoleConfig().RedirectURI)
	q.Set("first_party", "true")
	q.Set("origin", consoleConfig().Origin)
	q.Set("scope", "read write administer")
	w := signInRequest(t, a, q, nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), `class="signin-card"`) || strings.Contains(w.Body.String(), `name="remember"`) {
		t.Fatal("untrusted client gained Console treatment")
	}
	var id string
	for id = range a.flows {
	}
	w = formCall(a, "/oauth/authorize", url.Values{"flow": {id}, "csrf": {a.flows[id].CSRF}, "username": {"person"}, "password": {"synthetic-password-only"}, "decision": {"continue"}, "remember": {"yes"}}, w.Result().Cookies()[0])
	if w.Code != 200 || !strings.Contains(w.Body.String(), `data-page="consent"`) {
		t.Fatal("ordinary client skipped consent")
	}
	for _, callback := range []string{"https://console.example/other", "https://console.example:443/callback", "https://console.example/callback?extra=1"} {
		q := pageQuery("")
		q.Del("dataset")
		q.Set("client_id", "operator-console")
		q.Set("redirect_uri", callback)
		if w := signInRequest(t, a, q, nil); w.Code != 400 {
			t.Fatal("inexact Console redirect accepted", callback)
		}
	}
}

func TestConsoleViewsAndAccountIsolation(t *testing.T) {
	engine, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "datasets.db"), p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	owner := store.Access{Principal: "fixture-owner", Authorization: "fixture-owner"}
	types := []p.Meta{{ID: "test.visible"}, {ID: "test.private"}}
	own, err := engine.CreateDataset(t.Context(), owner, "Own", p.DevelopmentLimits(), types, "", [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	private, err := engine.CreateDataset(t.Context(), owner, "Other person's private records", p.DevelopmentLimits(), types, "", [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	shared, err := engine.CreateDataset(t.Context(), owner, "Shared dashboard", p.DevelopmentLimits(), types, "", [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	a, err := Open(t.Context(), filepath.Join(t.TempDir(), "identity.json"), "https://tessera.example", engine)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.AddPerson("person", "synthetic-password-only", own.DatasetID, store.Grants{Read: true, Write: true, Erase: true}, false); err != nil {
		t.Fatal(err)
	}
	if err = a.AddPerson("other", "synthetic-password-only", private.DatasetID, store.Grants{Read: true, Write: true, Erase: true}, false); err != nil {
		t.Fatal(err)
	}
	if err = a.GrantDataset("person", shared.DatasetID, store.Grants{Read: true}); err != nil {
		t.Fatal(err)
	}
	principal := a.data.Principals["person"]
	principal.SharedTypes = map[string][]string{shared.DatasetID: {"test.visible"}}
	a.data.Principals["person"] = principal
	if err = a.SetConsoles([]ConsoleConfig{consoleConfig()}); err != nil {
		t.Fatal(err)
	}
	access, tokens := consoleLogin(t, a, false)
	list, err := engine.Datasets(t.Context(), access)
	if err != nil || len(list) != 2 {
		t.Fatal("Console cannot select datasets through normal navigation", err, len(list))
	}
	if _, err = engine.Info(t.Context(), access, private.DatasetID); p.ErrorCode(err) != "unavailable" {
		t.Fatal("another person's private dataset exposed", err)
	}
	for _, typ := range []string{"test.visible", "test.private"} {
		err = engine.Transact(t.Context(), access, shared.DatasetID, func(tx store.Tx) error { _, e := tx.Query(typ, p.Query{}, 10); return e })
		if (typ == "test.visible" && err != nil) || (typ == "test.private" && err == nil) {
			t.Fatal("Console broadened a type-scoped share", typ, err)
		}
	}
	expiry := time.Now().Add(-time.Minute)
	if err = a.GrantDataset("person", own.DatasetID, store.Grants{Read: true, Write: true, Erase: true, ExpiresAt: &expiry}); err != nil {
		t.Fatal(err)
	}
	if _, err = engine.Info(t.Context(), access, own.DatasetID); p.ErrorCode(err) != "unavailable" {
		t.Fatal("expired role survived", err)
	}
	reopened, err := Open(t.Context(), a.path, a.issuer, engine)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reopened.Authenticate(t.Context(), tokens["access_token"].(string)); p.ErrorCode(err) != "authentication_failed" {
		t.Fatal("missing operator policy retained Console session", err)
	}
}

func TestConsoleWithdrawalAndPolicyChanges(t *testing.T) {
	a, engine, dataset := authFixture(t)
	cfg := consoleConfig()
	if err := a.SetConsoles([]ConsoleConfig{cfg}); err != nil {
		t.Fatal(err)
	}
	access, tokens := consoleLogin(t, a, false)
	if _, _, err := a.DeviceCodeFor(access); p.ErrorCode(err) != "permission_denied" {
		t.Fatal("Console policy transferred to device code", err)
	}
	cfg.DataGrants = []string{"read"}
	cfg.Administer = false
	if err := a.SetConsoles([]ConsoleConfig{cfg}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Authenticate(t.Context(), tokens["access_token"].(string)); err != nil {
		t.Fatal(err)
	}
	info, err := engine.Info(t.Context(), access, dataset)
	if err != nil || info.Access.Write || a.data.Authorizations[access.Authorization].Administer {
		t.Fatal("operator ceiling not enforced")
	}
	if err = a.WithdrawGrants(t.Context(), access.Authorization); err != nil {
		t.Fatal(err)
	}
	if _, err = a.Authenticate(t.Context(), tokens["access_token"].(string)); err != nil {
		t.Fatal("withdrawal revoked credentials", err)
	}
	if _, err = engine.Info(t.Context(), access, dataset); p.ErrorCode(err) != "unavailable" {
		t.Fatal("withdrawn Console regained grants", err)
	}
	if err = a.SetConsoles(nil); err != nil {
		t.Fatal(err)
	}
	if _, err = a.Authenticate(t.Context(), tokens["access_token"].(string)); p.ErrorCode(err) != "authentication_failed" {
		t.Fatal("disabled Console kept access", err)
	}
}

func TestConsoleCSRFAndRecovery(t *testing.T) {
	a, _, _ := authFixture(t)
	if err := a.SetConsoles([]ConsoleConfig{consoleConfig()}); err != nil {
		t.Fatal(err)
	}
	q := pageQuery("")
	q.Del("dataset")
	q.Set("client_id", "operator-console")
	q.Set("redirect_uri", consoleConfig().RedirectURI)
	w := signInRequest(t, a, q, map[string]string{"Accept": "text/html"})
	cookie := w.Result().Cookies()[0]
	var id string
	for id = range a.flows {
	}
	form := url.Values{"flow": {id}, "csrf": {a.flows[id].CSRF}, "username": {"person"}, "password": {"wrong"}, "remember": {"yes"}, "decision": {"continue"}}
	if bad := browserPost(a, form, nil); bad.Code != 400 {
		t.Fatal("missing Console CSRF cookie accepted")
	}
	altered := form.Clone()
	altered.Set("csrf", "wrong")
	if bad := browserPost(a, altered, cookie); bad.Code != 400 {
		t.Fatal("wrong Console CSRF accepted")
	}
	w = browserPost(a, form, cookie)
	if w.Code != 401 || !strings.Contains(w.Body.String(), `name="remember" value="yes" checked`) || !strings.Contains(w.Body.String(), `value="person"`) || strings.Contains(w.Body.String(), `value="wrong"`) {
		t.Fatal("credential retry lost choice or echoed password")
	}
	f := a.flows[id]
	f.Expires = time.Now().Add(-time.Minute)
	a.flows[id] = f
	a.recoveries[id] = f
	form.Set("password", "synthetic-password-only")
	form.Set("origin", "https://attacker.example")
	w = browserPost(a, form, cookie)
	if w.Code != 400 || !strings.Contains(w.Body.String(), `data-page="restart"`) {
		t.Fatal("expired Console did not require fresh sign-in")
	}
	restart := pageLink(t, w)
	target, _ := url.Parse(restart)
	if target.Query().Get("redirect_uri") != consoleConfig().RedirectURI || target.Query().Has("remember") || target.Query().Has("origin") {
		t.Fatal("recovery not bound to Console transaction")
	}
	w = httptest.NewRecorder()
	a.Handler().ServeHTTP(w, httptest.NewRequest("GET", restart, nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `class="signin-card"`) || strings.Contains(w.Body.String(), `name="remember" value="yes" checked`) || len(a.data.Authorizations) != 0 {
		t.Fatal("recovery reused authority or persistence choice")
	}
}

func TestPublicRegistrationCannotProvisionConsole(t *testing.T) {
	a, _, _ := authFixture(t)
	if err := a.SetConsoles([]ConsoleConfig{consoleConfig()}); err != nil {
		t.Fatal(err)
	}
	body := `{"client_id":"operator-console","client_name":"Operator Console","redirect_uris":["https://console.example/callback"],"application_type":"web","operator_console":true,"first_party":true,"origin":"https://console.example"}`
	r := httptest.NewRequest("POST", "/oauth/register", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	var registered struct {
		ID string `json:"client_id"`
	}
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &registered) != nil || registered.ID == "operator-console" {
		t.Fatal("public registration claimed operator identity")
	}
	q := pageQuery("")
	q.Del("dataset")
	q.Set("client_id", registered.ID)
	q.Set("redirect_uri", consoleConfig().RedirectURI)
	w = signInRequest(t, a, q, nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), `class="signin-card"`) || strings.Contains(w.Body.String(), `name="remember"`) {
		t.Fatal("public registration claimed first-party flow")
	}
}
