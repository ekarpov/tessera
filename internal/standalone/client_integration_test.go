package standalone

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

type oauthTokens struct {
	Access  string `json:"access_token"`
	Refresh string `json:"refresh_token"`
	Type    string `json:"token_type"`
	Expires int    `json:"expires_in"`
}

// TestClientAuthenticationIntegration uses the actual standalone composition and
// SQLite over trusted local TLS. It is a backend gate, not a Swift consumer.
func TestClientAuthenticationIntegration(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(nil)
	origin := "https://" + srv.Listener.Addr().String()
	config := Config{Issuer: origin, DataDir: dir, Engine: "sqlite", EncryptedStorage: true}
	runtime, err := Open(t.Context(), config)
	if err != nil {
		srv.Close()
		t.Fatal(err)
	}
	srv.Config.Handler = runtime.Handler
	srv.StartTLS()
	defer func() {
		srv.Close()
		if runtime != nil {
			if err := runtime.Close(); err != nil {
				t.Error(err)
			}
		}
	}()
	// This client trusts only the test server certificate; TLS verification stays on.
	client := srv.Client()
	client.Timeout = 10 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client.Jar, err = cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	const person = "synthetic-owner"
	const password = "synthetic-only-password"
	if err := runtime.Authority.AddPerson(person, password, sole(t, runtime.Engine), store.Grants{Read: true, Write: true, Schema: true, Erase: true}, false); err != nil {
		t.Fatal(err)
	}
	secret, err := runtime.Authority.AddService("synthetic-service", sole(t, runtime.Engine), store.Grants{Read: true, Write: true, Schema: true})
	if err != nil {
		t.Fatal(err)
	}

	// Never print bodies, URLs with authorization queries, credentials or callbacks.
	call := func(method, path, body, contentType, bearer, key, basicUser, basicPassword string, want int) ([]byte, http.Header) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, origin+path, strings.NewReader(body))
		if err != nil {
			t.Fatal("construct HTTP request")
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		if basicUser != "" {
			req.SetBasicAuth(basicUser, basicPassword)
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal("HTTP request failed")
		}
		defer res.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(res.Body, 2<<20))
		if err != nil || res.StatusCode != want {
			t.Fatalf("%s: expected HTTP %d, got %d (read error: %t)", method, want, res.StatusCode, err != nil)
		}
		return raw, res.Header
	}
	decode := func(raw []byte, v any) {
		t.Helper()
		if json.Unmarshal(raw, v) != nil {
			t.Fatal("invalid JSON response")
		}
	}
	form := func(path string, values url.Values, want int) ([]byte, http.Header) {
		t.Helper()
		return call("POST", path, values.Encode(), "application/x-www-form-urlencoded", "", "", "", "", want)
	}
	api := func(method, path, body, bearer, key string, want int) ([]byte, http.Header) {
		t.Helper()
		return call(method, path, body, "application/json", bearer, key, "", "", want)
	}
	oauthError := func(raw []byte, want string) {
		t.Helper()
		var v struct {
			Error string `json:"error"`
		}
		decode(raw, &v)
		if v.Error != want {
			t.Fatalf("expected OAuth error %s", want)
		}
	}
	protocolError := func(raw []byte, want string) {
		t.Helper()
		var v struct {
			Error p.Error `json:"error"`
		}
		decode(raw, &v)
		if v.Error.Code != want {
			t.Fatalf("expected protocol error %s, got %s", want, v.Error.Code)
		}
	}
	tokenReply := func(raw []byte, headers http.Header, refresh bool) oauthTokens {
		t.Helper()
		var v oauthTokens
		decode(raw, &v)
		if v.Access == "" || (v.Refresh != "") != refresh || v.Type != "Bearer" || v.Expires != 300 || headers.Get("Cache-Control") != "no-store" {
			t.Fatal("invalid token response or cache policy")
		}
		return v
	}
	raw, _ := api("GET", "/.well-known/tessera", "", "", "", 200)
	var discovery struct {
		AuthorizationServers []string `json:"authorization_servers"`
		AuthMethods          []string `json:"auth_methods"`
	}
	decode(raw, &discovery)
	if len(discovery.AuthorizationServers) != 1 || discovery.AuthorizationServers[0] != origin || strings.Join(discovery.AuthMethods, ",") != "oauth2,bearer_token,client_credentials" {
		t.Fatal("standalone discovery mismatch")
	}
	raw, _ = api("GET", "/.well-known/oauth-authorization-server", "", "", "", 200)
	var metadata map[string]json.RawMessage
	decode(raw, &metadata)
	for key, path := range map[string]string{"issuer": "", "authorization_endpoint": "/oauth/authorize", "token_endpoint": "/oauth/token", "registration_endpoint": "/oauth/register", "revocation_endpoint": "/oauth/revoke"} {
		var value string
		decode(metadata[key], &value)
		if value != origin+path {
			t.Fatal("metadata endpoint mismatch")
		}
	}
	var methods []string
	decode(metadata["code_challenge_methods_supported"], &methods)
	if len(methods) != 1 || methods[0] != "S256" {
		t.Fatal("PKCE metadata mismatch")
	}
	const callback = "com.example.tessera:/oauth/callback"
	raw, _ = api("POST", "/oauth/register", `{"client_name":"Synthetic Swift installation","redirect_uris":["`+callback+`"],"token_endpoint_auth_method":"none","grant_types":["authorization_code","refresh_token"],"response_types":["code"]}`, "", "", 201)
	var registration struct {
		ID     string `json:"client_id"`
		Secret string `json:"client_secret"`
	}
	decode(raw, &registration)
	if registration.ID == "" || registration.Secret != "" {
		t.Fatal("expected public client")
	}
	raw, _ = api("GET", "/v1/datasets", "", registration.ID, "", 401)
	protocolError(raw, "authentication_failed")
	hidden := func(raw []byte, name string) string {
		t.Helper()
		match := regexp.MustCompile(`name="` + name + `" value="([^"]+)"`).FindSubmatch(raw)
		if len(match) != 2 {
			t.Fatal("missing form field")
		}
		return string(match[1])
	}
	const verifier = "synthetic-verifier-with-at-least-forty-three-characters-123456789"
	hash := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(hash[:])
	authorize := func(redirect, state string, want int) []byte {
		t.Helper()
		query := url.Values{"prompt": {"consent"}, "client_id": {registration.ID}, "redirect_uri": {redirect}, "response_type": {"code"}, "state": {state}, "code_challenge": {challenge}, "code_challenge_method": {"S256"}, "dataset": {sole(t, runtime.Engine)}}
		raw, _ := api("GET", "/oauth/authorize?"+query.Encode(), "", "", "", want)
		return raw
	}
	raw = authorize(callback+"/wrong", "wrong-callback", 400)
	oauthError(raw, "invalid_request")
	signIn := func(state string, grants bool, negative bool) oauthTokens {
		t.Helper()
		// Two screens: signing in establishes the account, and only then can
		// the approval offer the datasets that account may reach.
		page := authorize(callback, state, 200)
		if !bytes.Contains(page, []byte(`data-page="signin"`)) || !bytes.Contains(page, []byte(`name="username"`)) {
			t.Fatal("missing sign-in screen")
		}
		if bytes.Contains(page, []byte("name=\"read\"")) {
			t.Fatal("the sign-in screen asked for grants before anyone had signed in")
		}
		entry := url.Values{"flow": {hidden(page, "flow")}, "csrf": {hidden(page, "csrf")}, "username": {person}, "password": {password}, "decision": {"continue"}}
		if negative {
			saved := entry.Get("csrf")
			entry.Set("csrf", "incorrect")
			raw, _ := form("/oauth/authorize", entry, 400)
			oauthError(raw, "invalid_request")
			entry.Set("csrf", saved)
		}
		approval, _ := form("/oauth/authorize", entry, 200)
		if !bytes.Contains(approval, []byte(`data-page="consent"`)) || !bytes.Contains(approval, []byte("Signed in as")) {
			t.Fatal("signing in did not reach the approval screen")
		}
		values := url.Values{"flow": {hidden(approval, "flow")}, "csrf": {hidden(approval, "csrf")}, "decision": {"allow"}}
		if dataset := hidden(approval, "dataset"); dataset != "" {
			values.Set("dataset", dataset)
		}
		if grants {
			values.Set("read", "yes")
			values.Set("write", "yes")
			values.Set("schema", "yes")
		}
		_, headers := form("/oauth/authorize", values, 303)
		target, err := url.Parse(headers.Get("Location"))
		if err != nil {
			t.Fatal("invalid callback")
		}
		query := target.Query()
		target.RawQuery = ""
		if target.String() != callback || query.Get("state") != state || query.Get("iss") != origin || query.Get("code") == "" {
			t.Fatal("callback binding mismatch")
		}
		exchange := url.Values{"grant_type": {"authorization_code"}, "client_id": {registration.ID}, "redirect_uri": {callback}, "code": {query.Get("code")}, "code_verifier": {verifier}}
		if negative {
			exchange.Set("redirect_uri", callback+"/wrong")
			raw, _ := form("/oauth/token", exchange, 400)
			oauthError(raw, "invalid_grant")
			exchange.Set("redirect_uri", callback)
			exchange.Set("code_verifier", strings.Repeat("x", 64))
			raw, _ = form("/oauth/token", exchange, 400)
			oauthError(raw, "invalid_grant")
			exchange.Set("code_verifier", verifier)
		}
		raw, headers := form("/oauth/token", exchange, 200)
		tokens := tokenReply(raw, headers, true)
		raw, _ = form("/oauth/token", exchange, 400)
		oauthError(raw, "invalid_grant")
		return tokens
	}
	a := signIn("installation-a", true, true)
	b := signIn("installation-b", true, false)
	c := signIn("installation-c-no-grants", false, false)
	listing := func(token string, count int) string {
		t.Helper()
		raw, _ := api("GET", "/v1/datasets", "", token, "", 200)
		var v struct {
			Datasets []struct {
				ID     string `json:"id"`
				Access struct {
					Erase bool `json:"erase"`
				} `json:"access"`
			} `json:"datasets"`
		}
		decode(raw, &v)
		if len(v.Datasets) != count {
			t.Fatal("unexpected dataset visibility")
		}
		if count == 0 {
			return ""
		}
		if v.Datasets[0].Access.Erase {
			t.Fatal("consent silently inherited erase")
		}
		return v.Datasets[0].ID
	}
	dataset := listing(a.Access, 1)
	if listing(b.Access, 1) != dataset {
		t.Fatal("installations did not discover same dataset")
	}
	listing(c.Access, 0)
	base := "/v1/datasets/" + dataset
	api("POST", base+"/meta", `{"type":{"id":"com.example.authnote","fields":[{"id":"body","meta_id":"com.example.authnote","type":"text"}]}}`, a.Access, "", 201)
	const transaction = `{"operations":[{"op":"insert","type":"com.example.authnote","fields":{"body":{"type":"text","value":"synthetic authentication gate"}}}]}`
	const key = "aabbccdd-0000-4000-8000-000000000101"
	statusPath := base + "/operations/" + key + ":status"
	// Discard the success body as if the response were lost.
	api("POST", base+":transact", transaction, a.Access, key, 200)
	refresh := func(tokens oauthTokens, want int) oauthTokens {
		t.Helper()
		raw, h := form("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {registration.ID}, "refresh_token": {tokens.Refresh}}, want)
		if want != 200 {
			oauthError(raw, "invalid_grant")
			return oauthTokens{}
		}
		next := tokenReply(raw, h, true)
		if next.Refresh == tokens.Refresh || next.Access == tokens.Access {
			t.Fatal("tokens did not rotate")
		}
		return next
	}
	a = refresh(a, 200)
	raw, _ = api("POST", statusPath, "", a.Access, "", 200)
	var receipt store.Receipt
	decode(raw, &receipt)
	var result p.TransactionResult
	decode(receipt.Result, &result)
	if receipt.State != "committed" || receipt.ResultWithheld || len(result.Results) != 1 || len(result.Results[0].Objects) != 1 || receipt.Watermark == "" {
		t.Fatal("refreshed owner lost receipt")
	}
	objectID := result.Results[0].Objects[0].ID
	raw, _ = api("GET", base+"/objects/com.example.authnote/"+objectID, "", a.Access, "", 200)
	var record p.RecordSet
	decode(raw, &record)
	if len(record.Records) != 1 || record.Records[0].Version == "" || len(record.Records[0].Values) != 1 || record.Records[0].Values[0].Payload() != "synthetic authentication gate" {
		t.Fatal("authenticated record round trip failed")
	}
	// A definitive transaction failure is owner-bound too.
	const failedKey = "aabbccdd-0000-4000-8000-000000000103"
	const failedTransaction = `{"operations":[{"op":"define_type","meta":{"id":"com.example.authnote"}}]}`
	failure, _ := api("POST", base+":transact", failedTransaction, a.Access, failedKey, 409)
	raw, _ = api("POST", base+"/operations/"+failedKey+":status", "", a.Access, "", 200)
	var failed store.Receipt
	decode(raw, &failed)
	if failed.State != "failed" || !bytes.Equal(failed.Error, failure) {
		t.Fatal("failed receipt lost original error")
	}
	api("POST", base+"/operations/"+failedKey+":status", "", b.Access, "", 403)
	raw, h := api("POST", base+":transact", transaction, a.Access, key, 200)
	if h.Get("Idempotency-Replayed") != "true" || !bytes.Equal(raw, receipt.Result) {
		t.Fatal("refresh changed exact replay")
	}
	raw, _ = api("POST", base+":transact", transaction+" ", a.Access, key, 409)
	protocolError(raw, "conflict")
	raw, _ = api("POST", statusPath, "", b.Access, "", 403)
	protocolError(raw, "permission_denied")
	raw, _ = api("POST", base+":transact", transaction, b.Access, key, 403)
	protocolError(raw, "permission_denied")
	raw, _ = api("POST", base+"/operations/aabbccdd-0000-4000-8000-000000000199:status", "", c.Access, "", 200)
	var unknown store.Receipt
	decode(raw, &unknown)
	if unknown.State != "unknown" || unknown.Watermark != "" {
		t.Fatal("unknown requires grants or invents observation")
	}
	api("POST", statusPath, "", "", "", 401)
	// Revoke one installation of the same registered client through the public endpoint.
	form("/oauth/revoke", url.Values{"client_id": {registration.ID}, "token": {b.Refresh}}, 200)
	api("GET", "/v1/datasets", "", b.Access, "", 401)
	api("POST", statusPath, "", b.Access, "", 401)
	refresh(b, 400)
	listing(a.Access, 1)
	// Reuse compromises only C's family, including its newly rotated access token.
	oldC := c
	c = refresh(c, 200)
	refresh(oldC, 400)
	api("GET", "/v1/datasets", "", c.Access, "", 401)
	refresh(c, 400)
	listing(a.Access, 1)
	// Grant-only withdrawal now goes through the administration surface over
	// HTTPS, like every other step here, rather than reaching into the engine.
	// It deliberately keeps the credentials valid while removing dataset access.
	access, err := runtime.Authority.Authenticate(t.Context(), a.Access)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := call("GET", "/admin/authorizations", "", "", "", "", person, password, 200)
	_, afterToken, found := strings.Cut(string(page), `name="csrf" value="`)
	if !found {
		t.Fatal("administration page carried no CSRF token")
	}
	csrf, _, _ := strings.Cut(afterToken, `"`)
	withdrawal := url.Values{"csrf": {csrf}, "authorization": {access.Authorization}, "action": {"withdraw"}}
	// This client does not follow redirects, so the outcome is read back from
	// the page: it must now report a withdrawal, not an active installation.
	call("POST", "/admin/authorizations", withdrawal.Encode(), "application/x-www-form-urlencoded", "", "", person, password, 303)
	page, _ = call("GET", "/admin/authorizations", "", "", "", "", person, password, 200)
	if !strings.Contains(string(page), "grants withdrawn") {
		t.Fatal("administration did not report the withdrawal")
	}
	listing(a.Access, 0)
	checkWithheld := func(raw []byte) {
		t.Helper()
		var v store.Receipt
		decode(raw, &v)
		if v.State != "committed" || !v.ResultWithheld || len(v.Result) != 0 || len(v.Objects) != 1 || v.Objects[0].ObjectID != objectID || v.Watermark != receipt.Watermark {
			t.Fatal("withdrawn owner lost identity or received body")
		}
	}
	raw, _ = api("POST", statusPath, "", a.Access, "", 200)
	checkWithheld(raw)
	raw, h = api("POST", base+":transact", transaction, a.Access, key, 200)
	checkWithheld(raw)
	if h.Get("Idempotency-Replayed") != "true" {
		t.Fatal("identity-only replay missing marker")
	}
	// The grant is gone, so the dataset is concealed: 404 unavailable rather
	// than 403, which would confirm it is still there.
	api("GET", base+"/objects/com.example.authnote/"+objectID, "", a.Access, "", 404)
	a = refresh(a, 200)
	listing(a.Access, 0)
	raw, _ = api("POST", statusPath, "", a.Access, "", 200)
	checkWithheld(raw)

	serviceToken := func(want int) oauthTokens {
		t.Helper()
		raw, h := call("POST", "/oauth/token", "grant_type=client_credentials&dataset="+sole(t, runtime.Engine), "application/x-www-form-urlencoded", "", "", "synthetic-service", secret, want)
		if want != 200 {
			oauthError(raw, "invalid_client")
			return oauthTokens{}
		}
		return tokenReply(raw, h, false)
	}
	service := serviceToken(200)
	const serviceKey = "aabbccdd-0000-4000-8000-000000000102"
	servicePath := base + "/operations/" + serviceKey + ":status"
	original, _ := api("POST", base+":transact", transaction, service.Access, serviceKey, 200)
	service = serviceToken(200)
	raw, _ = api("POST", servicePath, "", service.Access, "", 200)
	var serviceReceipt store.Receipt
	decode(raw, &serviceReceipt)
	if serviceReceipt.State != "committed" || !bytes.Equal(original, serviceReceipt.Result) {
		t.Fatal("service renewal lost receipt ownership")
	}
	raw, _ = api("POST", statusPath, "", service.Access, "", 403)
	protocolError(raw, "permission_denied")

	// Close and reopen the real runtime and TLS listener at the same issuer origin.
	tlsConfig := srv.TLS.Clone()
	address := srv.Listener.Addr().String()
	srv.Close()
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	runtime, err = Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	srv = httptest.NewUnstartedServer(runtime.Handler)
	srv.Listener.Close()
	srv.Listener = listener
	srv.TLS = tlsConfig
	srv.StartTLS()
	service = serviceToken(200)
	raw, h = api("POST", base+":transact", transaction, service.Access, serviceKey, 200)
	if h.Get("Idempotency-Replayed") != "true" || !bytes.Equal(raw, original) {
		t.Fatal("restart broke service identity continuity")
	}
	raw, _ = api("POST", base+"/objects/com.example.authnote:query", `{"query":{}}`, service.Access, "", 200)
	var records p.RecordSet
	decode(raw, &records)
	if len(records.Records) != 2 {
		t.Fatal("recovery or replay duplicated a write")
	}
	a = refresh(a, 200)
	listing(a.Access, 0)
	raw, _ = api("POST", statusPath, "", a.Access, "", 200)
	checkWithheld(raw)
	api("GET", "/v1/datasets", "", b.Access, "", 401)
	refresh(b, 400)
	refresh(c, 400)
	call("POST", "/oauth/revoke", url.Values{"token": {service.Access}}.Encode(), "application/x-www-form-urlencoded", "", "", "synthetic-service", secret, 200)
	api("POST", servicePath, "", service.Access, "", 401)
	serviceToken(401)
	t.Log("Standalone TLS OAuth, consent, PKCE refusals, rotation/reuse, isolated revocation, administrative grant withdrawal, owner recovery and service continuity passed; independent Swift consumer pending")
}
