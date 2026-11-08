package security_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"html"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/ekarpov/tessera/internal/identity"
	"github.com/ekarpov/tessera/pkg/conformance"
	"github.com/ekarpov/tessera/pkg/store"
)

// The dedicated website exception must not turn account administration into
// data ownership. Exercise the actual HTTP API, including concealed data and
// the existing self-deletion guard, rather than just token flags.
func TestFirstPartyConsolePrivacyAndSensitiveOperations(t *testing.T) {
	x := tenants(t)
	_, object, _ := x.seed(t, x.bobTok, x.bobSet, x.bobStore)
	x.seed(t, x.aliceTok, x.aliceSet, x.aliceStore)
	cfg := identity.ConsoleConfig{ClientID: "operator-console", Name: "Console", Origin: "https://console.example", RedirectURI: "https://console.example/callback", DataGrants: []string{"read", "write", "insert"}, Administer: true}
	if err := x.runtime.Authority.SetConsoles([]identity.ConsoleConfig{cfg}); err != nil {
		t.Fatal(err)
	}
	token := x.consoleSignIn(t, "alice", cfg)
	var w *httptest.ResponseRecorder
	if w = x.call(t, "GET", "/v1/datasets/"+x.bobSet+"/meta", token, "", nil); w.Code != 404 || !strings.Contains(w.Body.String(), "unavailable") {
		t.Fatal("owner learned peer's private dataset", w.Code)
	}
	if w = x.call(t, "GET", "/v1/datasets/"+x.bobSet+"/objects/security.record/"+object, token, "", nil); w.Code != 404 || !strings.Contains(w.Body.String(), "unavailable") {
		t.Fatal("owner read peer records", w.Code)
	}
	page := x.call(t, "POST", "/v1/datasets/"+x.aliceSet+"/objects/security.record:page", token, `{"query":{},"page_size":1}`, nil)
	var started store.PageStart
	if page.Code != 200 || json.Unmarshal(page.Body.Bytes(), &started) != nil || started.Cursor == "" {
		t.Fatal("Console page did not start", page.Code)
	}
	if next := x.call(t, "POST", "/v1/datasets/"+x.aliceSet+"/pages/"+started.Cursor+":next", token, "", nil); next.Code != 200 {
		t.Fatal("unchanged Console authority invalidated its page", next.Code)
	}
	if w = x.admin(t, "GET", "/datasets/"+x.aliceSet+"/readers", token, ""); w.Code != 200 {
		t.Fatal("account-held administration missing", w.Code)
	}
	if w = x.admin(t, "POST", "/accounts/alice/delete", token, ""); w.Code != 403 {
		t.Fatal("Console bypassed sensitive self-deletion guard", w.Code)
	}
	if w = x.call(t, "GET", "/v1/datasets/"+x.aliceSet+"/meta", token, "", nil); w.Code != 200 {
		t.Fatal("refused destruction changed data", w.Code)
	}
	if err := x.runtime.Authority.GrantDataset("alice", x.aliceSet, store.Grants{Read: true}); err != nil {
		t.Fatal(err)
	}
	if w = x.admin(t, "GET", "/datasets/"+x.aliceSet+"/readers", token, ""); w.Code != 403 {
		t.Fatal("role withdrawal left administration", w.Code)
	}
	if err := x.runtime.Authority.SuspendAccount("alice"); err != nil {
		t.Fatal(err)
	}
	if w = x.call(t, "GET", "/v1/datasets", token, "", nil); w.Code != 401 {
		t.Fatal("suspended Console authenticated", w.Code)
	}
}

func (x *tenancy) consoleSignIn(t *testing.T, account string, cfg identity.ConsoleConfig) string {
	t.Helper()
	verifier := strings.Repeat("x", 43)
	hash := sha256.Sum256([]byte(verifier))
	q := url.Values{"client_id": {cfg.ClientID}, "redirect_uri": {cfg.RedirectURI}, "response_type": {"code"}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(hash[:])}, "state": {"bound"}}
	w := httptest.NewRecorder()
	x.runtime.Handler.ServeHTTP(w, httptest.NewRequest("GET", "/oauth/authorize?"+q.Encode(), nil))
	values := url.Values{"username": {account}, "password": {password}, "decision": {"continue"}}
	for _, name := range []string{"flow", "csrf"} {
		match := regexp.MustCompile(`name="` + name + `" value="([^"]+)"`).FindStringSubmatch(w.Body.String())
		if len(match) != 2 {
			t.Fatal("missing sign-in binding")
		}
		values.Set(name, html.UnescapeString(match[1]))
	}
	request := httptest.NewRequest("POST", "/oauth/authorize", strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(w.Result().Cookies()[0])
	w = httptest.NewRecorder()
	x.runtime.Handler.ServeHTTP(w, request)
	if w.Code != 303 {
		t.Fatal("one-submit sign-in did not finish", w.Code)
	}
	target, _ := url.Parse(w.Header().Get("Location"))
	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {cfg.ClientID}, "redirect_uri": {cfg.RedirectURI}, "code": {target.Query().Get("code")}, "code_verifier": {verifier}}
	request = httptest.NewRequest("POST", "/oauth/token", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	x.runtime.Handler.ServeHTTP(w, request)
	var tokens struct {
		Access string `json:"access_token"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &tokens) != nil {
		t.Fatal("Console exchange failed")
	}
	return tokens.Access
}

// Reuse the importable sharing contract suite with tokens issued by the
// Console path, proving exact-type privacy and contribution boundaries.
func TestFirstPartyConsoleSharingConformance(t *testing.T) {
	x := tenants(t)
	cfg := identity.ConsoleConfig{ClientID: "operator-console", Name: "Console", Origin: "https://console.example", RedirectURI: "https://console.example/callback", DataGrants: []string{"read", "write", "insert"}, Administer: true}
	if err := x.runtime.Authority.SetConsoles([]identity.ConsoleConfig{cfg}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(x.runtime.Handler)
	defer server.Close()
	fixture := conformance.CoreFixture{URL: server.URL, Client: server.Client(), Token: x.aliceTok,
		Share: func(ctx context.Context, types []string) (string, error) {
			if err := x.runtime.Authority.SetReader(ctx, "alice", x.aliceSet, "console-reader", types, nil, password); err != nil {
				return "", err
			}
			return x.consoleSignIn(t, "console-reader", cfg), nil
		},
		Contribute: func(ctx context.Context, types []string) (string, error) {
			if err := x.runtime.Authority.SetContributor(ctx, "alice", x.aliceSet, "console-contributor", types, nil, password); err != nil {
				return "", err
			}
			return x.consoleSignIn(t, "console-contributor", cfg), nil
		},
	}
	if err := conformance.CheckSharing(t.Context(), fixture); err != nil {
		t.Fatal(err)
	}
}
