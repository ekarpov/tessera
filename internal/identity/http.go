package identity

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ekarpov/tessera/pkg/store"
)

func (a *Authority) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /oauth/brand/logo", a.brandLogo)
	mux.HandleFunc("GET /oauth/client-logos/{client}", a.clientLogo)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", a.metadata)
	mux.HandleFunc("POST /oauth/register", a.register)
	mux.HandleFunc("GET /oauth/authorize", a.authorize)
	mux.HandleFunc("POST /oauth/authorize", a.consent)
	mux.HandleFunc("POST /oauth/token", a.token)
	mux.HandleFunc("POST /oauth/revoke", a.revocation)
	mux.HandleFunc("POST /oauth/invitations", a.invite)
	mux.HandleFunc("GET /oauth/invitation", a.redeem)
	mux.HandleFunc("POST /oauth/invitation", a.redeem)
	mux.HandleFunc("GET /oauth/account/delete", a.deletion)
	mux.HandleFunc("POST /oauth/account/delete", a.deletion)
	mux.HandleFunc("GET /oauth/account/recovery", a.recoveryRoute(false))
	mux.HandleFunc("POST /oauth/account/recovery", a.recoveryRoute(false))
	mux.HandleFunc("GET /oauth/account/recover", a.recoveryRoute(true))
	mux.HandleFunc("POST /oauth/account/recover", a.recoveryRoute(true))
	mux.HandleFunc("GET /oauth/account/password", a.passwordChange)
	mux.HandleFunc("POST /oauth/account/password", a.passwordChange)
	mux.HandleFunc("GET /oauth/account", a.describeAccount)
	mux.HandleFunc("POST /oauth/device-codes", a.deviceCodes)
	mux.HandleFunc("GET /oauth/account/installations", a.ownInstallations)
	mux.HandleFunc("DELETE /oauth/account/installations/{installation}", a.ownInstallations)
	mux.HandleFunc("POST /oauth/account/password-link", a.passwordLinks)
	mux.HandleFunc("GET /oauth/invitation/preview", a.invitationPreview)
	return mux
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// accountSuspended answers a caller who has proved it is the account — its
// password, a valid code, an unspent refresh token or its secret — that the
// account is suspended, rather than that it is signed out. Signing in again
// cannot help it; its administrator can.
const accountSuspended = "account_suspended"

func oauthError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}
func (a *Authority) metadata(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"issuer": a.issuer, "authorization_endpoint": a.issuer + "/oauth/authorize", "token_endpoint": a.issuer + "/oauth/token", "registration_endpoint": a.issuer + "/oauth/register", "revocation_endpoint": a.issuer + "/oauth/revoke", "response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code", "refresh_token", "client_credentials", InvitationGrant}, "code_challenge_methods_supported": []string{"S256"}, "token_endpoint_auth_methods_supported": []string{"none", "client_secret_basic"}})
}

// maxRateWindows bounds the memory the per-address counts take. Reaching it
// takes that many distinct addresses — or IPv6 /64 networks — inside one
// minute, which is a botnet rather than a household. A variable only so a
// test can reach it.
var maxRateWindows = 100000

// allowed counts a request against its caller's budget for the minute.
//
// There is no server-wide count behind it any more. One was there to keep a
// flood from spending every processor on password derivations, and it was
// itself a budget anyone could spend: a few addresses closed sign-in for
// everybody (finding 31). The processors are protected by derive's bound on
// concurrency instead, which slows under a flood rather than closing.
func (a *Authority) allowed(r *http.Request, bucket string, max int) bool {
	key := bucket + ":" + a.clientAddress(r)
	now := time.Now()
	// Expired windows are swept every few seconds, not on every request: with
	// many addresses counted, walking them all under the lock each time was
	// its own way to slow the server down.
	if now.Sub(a.swept) > 5*time.Second {
		for key, window := range a.windows {
			if now.Sub(window.Start) > time.Minute {
				delete(a.windows, key)
			}
		}
		a.swept = now
	}
	window, known := a.windows[key]
	if known && now.Sub(window.Start) > time.Minute {
		window, known = rateWindow{}, false
	}
	// Past the bound, addresses not already counted share one window. Those
	// already counted keep their own, so a flood of new addresses does not
	// take sign-in away from anybody who was using it.
	if !known && len(a.windows) >= maxRateWindows {
		key = bucket + ":overflow"
		window = a.windows[key]
		max *= 4
	}
	if window.Count == 0 {
		window.Start = now
	}
	window.Count++
	a.windows[key] = window
	return window.Count <= max
}
func validRedirect(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Fragment != "" || u.User != nil || u.Scheme == "" {
		return false
	}
	if u.Scheme == "https" {
		return u.Hostname() != ""
	}
	if u.Scheme == "http" {
		ip := net.ParseIP(u.Hostname())
		return ip != nil && ip.IsLoopback()
	}
	return strings.Contains(u.Scheme, ".") && u.Scheme != "javascript" && u.Scheme != "data" && u.Scheme != "file"
}
func redirectMatches(registered, actual string) bool {
	if registered == actual {
		return true
	}
	a, err := url.Parse(registered)
	if err != nil {
		return false
	}
	b, err := url.Parse(actual)
	if err != nil {
		return false
	}
	ip := net.ParseIP(a.Hostname())
	if a.Scheme != "http" || ip == nil || !ip.IsLoopback() {
		return false
	}
	a.Host = a.Hostname()
	b.Host = b.Hostname()
	return a.String() == b.String()
}
func (a *Authority) register(w http.ResponseWriter, r *http.Request) {
	// The budget is spent before the body is read, so an unauthenticated
	// caller cannot make this server read for free, and the lock is dropped
	// again before the reading starts.
	a.mu.Lock()
	if len(a.data.Clients) >= maxClients {
		a.expireUnusedClients()
	}
	admitted := a.allowed(r, "register", 20) && len(a.data.Clients) < maxClients
	a.mu.Unlock()
	if !admitted {
		w.Header().Set("Retry-After", "60")
		oauthError(w, 429, "temporarily_unavailable")
		return
	}
	var request struct {
		Name      string   `json:"client_name"`
		Logo      []byte   `json:"logo_png"`
		Redirects []string `json:"redirect_uris"`
		Method    string   `json:"token_endpoint_auth_method"`
		Grants    []string `json:"grant_types"`
		Responses []string `json:"response_types"`
	}
	// Read with the lock released. This is a read from the network, of a body
	// an unauthenticated caller controls the pace of, and this lock is the one
	// every API request takes to check its bearer token: holding it here let a
	// stalled registration stop authentication, sign-in and administration for
	// the whole server. Moving the password derivation out fixed a different
	// path to the same place.
	raw, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<10))
	a.mu.Lock()
	defer a.mu.Unlock()
	// RFC 7591 requires ignoring unrecognized registration metadata. Validate
	// the JSON structure first, retaining duplicate-key and Unicode checks.
	var metadata map[string]json.RawMessage
	if readErr != nil || p.DecodeRequest(raw, &metadata, 32<<10) != nil || json.Unmarshal(raw, &request) != nil || len(request.Name) > 200 || len(request.Redirects) == 0 || len(request.Redirects) > 10 || (request.Method != "" && request.Method != "none") {
		oauthError(w, 400, "invalid_client_metadata")
		return
	}
	for _, v := range request.Grants {
		if v != "authorization_code" && v != "refresh_token" {
			oauthError(w, 400, "invalid_client_metadata")
			return
		}
	}
	for _, v := range request.Responses {
		if v != "code" {
			oauthError(w, 400, "invalid_client_metadata")
			return
		}
	}
	for _, redirect := range request.Redirects {
		if len(redirect) > 2048 || !validRedirect(redirect) {
			oauthError(w, 400, "invalid_redirect_uri")
			return
		}
	}
	logo, logoErr := sanitizeClientLogo(request.Logo)
	if logoErr != nil {
		oauthError(w, 400, "invalid_client_metadata")
		return
	}
	if a.logoBudget()+len(logo) > maxClientLogosTotal {
		oauthError(w, 429, "temporarily_unavailable")
		return
	}
	client := Client{Logo: logo, ID: random(), Name: request.Name, Redirects: request.Redirects, Registered: time.Now().Unix()}
	a.data.Clients[client.ID] = client
	if len(logo) > 0 {
		a.hasClientLogos.Store(true)
	}
	if a.save() != nil {
		oauthError(w, 503, "temporarily_unavailable")
		return
	}
	writeJSON(w, 201, map[string]any{"client_id": client.ID, "client_name": client.Name, "redirect_uris": client.Redirects, "token_endpoint_auth_method": "none", "grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"}})
}

var pkceVerifier = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)

// Two screens, because one cannot be both. The first cannot offer the datasets
// an account may reach — nobody has said which account it is yet — and a person
// who types a password and approves permissions in the same action never
// reviews what they are approving as an authenticated user.
// datasetOption is one row of the consent screen's dataset selector. Name is
// the display name where the authority knows one; it never substitutes for the
// identity, which is what the approval is actually over.
type datasetOption struct {
	ID, Name string
	// Yours is a dataset the account writes or administers, as against one
	// somebody shares with it: a person choosing needs to tell the two apart,
	// and picking another's by mistake connects them to data they cannot
	// change.
	Yours bool
	// Adds is a dataset the account only adds records to.
	Adds bool
}

var signInPage = page(`{{template "head" .}}{{template "clienticon" .}}<p>{{if .WebConsole}}{{call .T "signin.website" .Name}}{{else}}{{call .T "signin.app" .Name}}{{end}}</p><p class="note">{{call .T "signin.note"}}</p>` +
	`<form method="post" action="/oauth/authorize"><input type="hidden" name="flow" value="{{.Flow}}"><input type="hidden" name="csrf" value="{{.CSRF}}">` +
	`<label>{{call .T "account"}}<input name="username" autocomplete="username" autocapitalize="none" autocorrect="off" spellcheck="false" required></label>` +
	`<label>{{call .T "password"}}<input name="password" type="password" autocomplete="current-password" required></label>` +
	`<div class="actions"><button name="decision" value="continue">{{call .T "signin.submit"}}</button><button class="secondary" name="decision" value="deny" formnovalidate>{{call .T "cancel"}}</button></div></form>` +
	`<p class="note">{{if .WebConsole}}{{call .T "unverified.website" .Name}}{{else}}{{call .T "unverified" .Name}}{{end}} {{call .T "signin.returns" .Redirect}}</p>{{template "foot" .}}`)

// The dedicated operator Console is authentication under a configured website
// policy, not a consent form with hidden or automatically submitted choices.
var consoleSignInPage = page(`{{template "head" .}}<form method="post" action="/oauth/authorize"><input type="hidden" name="flow" value="{{.Flow}}"><input type="hidden" name="csrf" value="{{.CSRF}}">` +
	`<label>{{call .T "account"}}<input name="username" value="{{.Username}}" autocomplete="username" autocapitalize="none" autocorrect="off" spellcheck="false" required></label>` +
	`<label>{{call .T "password"}}<input name="password" type="password" autocomplete="current-password" required></label>` +
	`<label class="check"><input type="checkbox" name="remember" value="yes"{{if .Remember}} checked{{end}}>{{call .T "console.remember"}}</label>` +
	`<div class="actions"><button name="decision" value="continue">{{call .T "signin.submit"}}</button></div></form>{{template "foot" .}}`)

func signInTemplate(f flow) *template.Template {
	if f.Console {
		return consoleSignInPage
	}
	return signInPage
}

var consentPage = page(`{{template "head" .}}{{template "clienticon" .}}<p>{{call .T "consent.signedin" .Principal}}</p><p>{{.Summary}}</p>` +
	`<form method="post" action="/oauth/authorize"><input type="hidden" name="flow" value="{{.Flow}}"><input type="hidden" name="csrf" value="{{.CSRF}}">` +
	`{{if .Named}}<p>{{call .T "consent.data"}} <strong>{{.NamedLabel}}</strong></p><input type="hidden" name="dataset" value="{{.Named}}">` +
	`{{else if .Datasets}}<label>{{call .T "consent.which"}}<select name="dataset"{{if not .OffersShared}} required{{end}}>{{if .OffersShared}}<option value="">{{call $.T "consent.none"}}</option>{{end}}` +
	`{{range .Datasets}}<option value="{{.ID}}">{{if .Name}}{{.Name}}{{else}}{{.ID}}{{end}} — {{if .Yours}}{{call $.T "consent.yours"}}{{else if .Adds}}{{call $.T "consent.adds"}}{{else}}{{call $.T "consent.sharedwith"}}{{end}}</option>{{end}}</select></label>` +
	`{{else}}<p>{{call .T "consent.nodata"}}</p>{{end}}` +
	`{{if .MissingHints}}<p class="note" role="status">{{call .T "consent.missing"}} <strong>{{.MissingHints}}</strong></p>{{end}}` +
	`<details><summary>{{call .T "consent.change"}}</summary><p class="note">{{call .T "consent.choices"}}</p>{{if or .Named .Datasets}}<p>{{call .T "consent.can"}}</p>` +
	`<label class="check"><input type="checkbox" name="read" value="yes"{{if .Defaults.Read}} checked{{end}}>{{call .T "consent.read"}}</label>` +
	`<label class="check"><input type="checkbox" name="write" value="yes"{{if .Defaults.Write}} checked{{end}}>{{call .T "consent.write"}}</label>` +
	`<label class="check"><input type="checkbox" name="schema" value="yes">{{call .T "consent.schema"}}</label>` +
	`<label class="check"><input type="checkbox" name="erase" value="yes">{{call .T "consent.erase"}}</label>` +
	`<label class="check"><input type="checkbox" name="insert" value="yes"{{if .Defaults.Insert}} checked{{end}}>{{call .T "consent.insert"}}</label>{{template "shared" .}}` +
	`{{else if .OffersShared}}{{template "shared" .}}{{end}}` +
	`{{if .MayAdminister}}<label class="check"><input type="checkbox" name="administer" value="yes">{{call .T "consent.administer"}}</label><p class="note">{{call .T "consent.administer.note"}}</p>{{end}}` +
	`{{if .MayCreate}}<p>{{call .T "consent.create.intro"}}</p><label class="check"><input type="checkbox" name="create_dataset" value="yes">{{call .T "consent.create"}}</label><p class="note">{{call .T "consent.create.note"}}</p>{{end}}` +
	`</details><div class="actions">{{if or .Named .Datasets .OffersShared .MayCreate .MayAdminister}}<button name="decision" value="allow">{{call .T "consent.allow"}}</button>{{end}}<button class="secondary" name="decision" value="deny" formnovalidate>{{call .T "cancel"}}</button></div></form>` +
	`<p class="note">{{if .WebConsole}}{{call .T "unverified.website" .Name}}{{else}}{{call .T "unverified" .Name}}{{end}} {{call .T "signin.returns" .Redirect}}</p>{{template "foot" .}}` +
	`{{define "shared"}}{{if .OffersShared}}<label class="check"><input type="checkbox" name="shared_reads" value="yes">{{call .T "consent.shared"}}</label><p class="note">{{call .T "consent.shared.note" .SharedNow}}</p>{{end}}{{end}}`)

func (a *Authority) authorize(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.allowed(r, "authorize", 60) {
		a.flowError(w, r, 429, "temporarily_unavailable", "attempts")
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		a.flowError(w, r, 400, "invalid_request", "error.link")
		return
	}
	for _, values := range query {
		if len(values) != 1 {
			a.flowError(w, r, 400, "invalid_request", "error.link")
			return
		}
	}
	client, ok := a.data.Clients[query.Get("client_id")]
	if !ok || client.Secret != "" {
		a.flowError(w, r, 400, "invalid_client", "error.link")
		return
	}
	redirect := query.Get("redirect_uri")
	matched := false
	for _, value := range client.Redirects {
		if redirectMatches(value, redirect) {
			matched = true
		}
	}
	if !matched {
		a.flowError(w, r, 400, "invalid_request", "error.link")
		return
	}
	_, firstParty := a.console(client.ID, redirect)
	if client.OperatorConsole && !firstParty {
		a.flowError(w, r, 400, "invalid_client", "error.link")
		return
	}
	if firstParty && (query.Has("dataset") || query.Has("scope") || query.Has("shared_reads")) {
		a.flowError(w, r, 400, "invalid_request", "error.link")
		return
	}
	// An approval that does not say which dataset it is for is not consent, but
	// the request is not the only thing that can say it. A named dataset is
	// shown; an unnamed one is chosen by the person from what their account may
	// reach, which they can only be offered after they sign in.
	//
	// This is why a client cannot be required to name one: GET /v1/datasets is
	// authenticated, so before a token exists there is no way to learn an
	// identity, and on a first connection the dataset was minted for them and
	// they have never seen it.
	dataset := query.Get("dataset")
	if dataset != "" && p.ValidateIdentifier(p.DatasetIdentifier, dataset) != nil {
		a.flowError(w, r, 400, "invalid_request", "error.link")
		return
	}
	// shared_reads asks that the approval offer reading everything shared
	// with the account, alone or beside a dataset; the page then says both
	// things the approval would cover. It is true or absent.
	sharedReads := query.Get("shared_reads") == "true"
	if query.Has("shared_reads") && !sharedReads {
		a.flowError(w, r, 400, "invalid_request", "error.link")
		return
	}
	// Scope names grant hints only. Reject unknown names rather than silently
	// substituting safe defaults for an unsupported request.
	if len(query.Get("scope")) > 256 {
		a.flowError(w, r, 400, "invalid_scope", "error.link")
		return
	}
	for _, grant := range strings.Split(query.Get("scope"), " ") {
		switch grant {
		case "", "read", "write", "insert", "schema", "erase", "administer", "create_dataset", "shared_reads":
		default:
			a.flowError(w, r, 400, "invalid_scope", "error.link")
			return
		}
	}
	challenge := query.Get("code_challenge")
	decoded, e := base64.RawURLEncoding.DecodeString(challenge)
	if query.Get("response_type") != "code" || query.Get("code_challenge_method") != "S256" || e != nil || len(decoded) != 32 || query.Get("state") == "" || len(query.Get("state")) > 2048 {
		a.flowError(w, r, 400, "invalid_request", "error.link")
		return
	}
	for id, f := range a.flows {
		if time.Now().After(f.Expires) {
			delete(a.flows, id)
		}
	}
	if len(a.flows) >= 1000 {
		a.flowError(w, r, 429, "temporarily_unavailable", "attempts")
		return
	}
	id, csrf := random(), random()
	a.flows[id] = flow{ForceConsent: query.Get("prompt") == "consent", Console: firstParty, Client: client.ID, Redirect: redirect, Challenge: challenge, State: query.Get("state"), CSRF: csrf, Dataset: dataset, SharedReads: sharedReads, Scope: query.Get("scope"), HasScope: query.Has("scope"), Expires: time.Now().Add(5 * time.Minute)}
	a.rememberAuthorization(id, a.flows[id])
	http.SetCookie(w, &http.Cookie{Name: "tessera-flow", Value: csrf, Path: "/oauth/authorize", Secure: strings.HasPrefix(a.issuer, "https://"), HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 300})
	a.pageHeaders(w)
	_ = signInTemplate(a.flows[id]).Execute(w, a.view(r, "signin", "signin.title", map[string]any{"ClientLogo": clientLogoPath(client), "ClientID": client.ID, "Name": client.Name, "Redirect": redirect, "Flow": id, "CSRF": csrf}))
}
func parseForm(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") || r.ParseForm() != nil {
		oauthError(w, 400, "invalid_request")
		return false
	}
	for _, values := range r.PostForm {
		if len(values) != 1 {
			oauthError(w, 400, "invalid_request")
			return false
		}
	}
	return true
}
func (a *Authority) consent(w http.ResponseWriter, r *http.Request) {
	if !a.parseFlowForm(w, r) {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.allowed(r, "login", 10) {
		w.Header().Set("Retry-After", "60")
		a.flowError(w, r, 429, "temporarily_unavailable", "attempts")
		return
	}
	f, ok := a.flows[r.PostForm.Get("flow")]
	if !ok {
		f = a.recoveries[r.PostForm.Get("flow")]
	}
	cookie, err := r.Cookie("tessera-flow")
	if f.Client == "" || time.Now().After(f.Expires.Add(authorizationRecoveryWindow)) || err != nil || !equal(cookie.Value, f.CSRF) || !equal(r.PostForm.Get("csrf"), f.CSRF) {
		a.flowError(w, r, 400, "invalid_request", "error.expired")
		return
	}
	if !ok || time.Now().After(f.Expires) {
		delete(a.flows, r.PostForm.Get("flow"))
		a.restartAuthorization(w, r, f)
		return
	}
	target, _ := url.Parse(f.Redirect)
	q := target.Query()
	q.Set("state", f.State)
	q.Set("iss", a.issuer)
	if r.PostForm.Get("decision") == "deny" {
		delete(a.flows, r.PostForm.Get("flow"))
		q.Set("error", "access_denied")
		target.RawQuery = q.Encode()
		a.completeAuthorization(w, r, f, target)
		return
	}
	// Step one: nobody has signed in on this flow yet, so this submission is a
	// sign-in and authorizes nothing. The account it establishes is what makes
	// the next screen able to offer the datasets it may reach.
	if f.Principal == "" {
		flowID := r.PostForm.Get("flow")
		principal, ok := a.data.Principals[r.PostForm.Get("username")]
		// Derived with the lock released. This is a deliberate 600k-iteration
		// derivation and this lock is the one every API request takes to check
		// its bearer token, so holding it here stalled unrelated traffic for
		// as long as the derivation ran — from an unauthenticated form post.
		a.mu.Unlock()
		derived, busy := a.derive(r.Context(), r.PostForm.Get("password"), principal.Salt)
		valid := busy == nil && equal(derived, principal.Password)
		a.mu.Lock()
		if busy != nil {
			w.Header().Set("Retry-After", "5")
			a.flowError(w, r, 503, "temporarily_unavailable", "busy")
			return
		}
		// The flow is read again rather than carried across the gap: another
		// request may have spent or expired it while the derivation ran, and a
		// sign-in must not complete against a flow that is no longer the one
		// it started from.
		current, live := a.flows[flowID]
		if !live || !equal(current.CSRF, f.CSRF) || current.Principal != "" {
			a.flowError(w, r, 400, "invalid_request", "error.expired")
			return
		}
		f = current
		// Suspension is told to the person, and only once they have proved
		// who they are: here, by the password the account holds now. Refused
		// as an ordinary failure, their application sent them to sign in
		// again — which cannot help, when what they need is their
		// administrator.
		if current := a.data.Principals[principal.ID]; ok && valid && current.Kind == "person" && current.Suspended &&
			current.Salt == principal.Salt && current.Password == principal.Password {
			a.flowError(w, r, 403, accountSuspended, "error.suspended")
			return
		}
		if !ok || principal.Kind != "person" || !valid || !a.unchanged(principal.ID, principal) || r.PostForm.Get("decision") != "continue" {
			// A person who mistyped is shown the same form again, on the
			// same flow, with what went wrong; the rate limit above still
			// counts every attempt.
			if wantsPage(r) {
				client := a.data.Clients[f.Client]
				a.pageHeaders(w)
				w.WriteHeader(401)
				v := a.viewLocked(r, "signin", "signin.title", map[string]any{"ClientLogo": clientLogoPath(client), "ClientID": client.ID, "Name": client.Name, "Redirect": f.Redirect, "Flow": flowID, "CSRF": f.CSRF})
				v["Problem"] = v["T"].(func(string, ...any) string)("signin.wrong")
				v["Remember"] = f.Console && r.PostForm.Get("remember") == "yes"
				v["Username"] = r.PostForm.Get("username")
				_ = signInTemplate(f).Execute(w, v)
				return
			}
			oauthError(w, 401, "access_denied")
			return
		}
		if f.Console {
			remember := r.PostForm.Get("remember")
			if remember != "" && remember != "yes" {
				a.flowError(w, r, 400, "invalid_request", "error.link")
				return
			}
			principal = a.data.Principals[principal.ID]
			// Spend authentication before grant setup, including on partial
			// failure. Recovery starts fresh and never repeats an invitation.
			delete(a.flows, flowID)
			auth, e := a.newConsoleAuthorization(r.Context(), principal, f)
			if e != nil {
				a.flowError(w, r, 503, "temporarily_unavailable", "busy")
				return
			}
			a.approveRemember(w, r, f, auth, q, target, remember == "yes")
			return
		}
		// A token that survived step one must not be spendable at step two, so
		// each step gets its own and the cookie follows it.
		// Grants may have changed while password derivation released the lock.
		principal = a.data.Principals[principal.ID]
		f.Principal = principal.ID
		client := a.data.Clients[f.Client]
		offered := []datasetOption{}
		if f.Dataset == "" {
			names := map[string]string{}
			if held, e := a.engine.Provisioned(r.Context()); e == nil {
				for _, d := range held {
					names[d.ID] = d.DisplayName
				}
			}
			for id, held := range principal.Datasets {
				if !(held.Read || held.Write || held.Schema || held.Erase || held.Insert) {
					continue
				}
				yours := held.Write || held.Schema || held.Erase
				// A dataset the account only reads is covered by reading what
				// is shared, where that is offered, and listing it beside the
				// account's own invites choosing it by mistake.
				if f.SharedReads && !yours && !held.Insert {
					continue
				}
				offered = append(offered, datasetOption{ID: id, Name: names[id], Yours: yours, Adds: !yours && !held.Read && held.Insert})
			}
			// The account's own first, then by name, then by identity: an order
			// a person can learn, and the same every time.
			sort.Slice(offered, func(i, j int) bool {
				if offered[i].Yours != offered[j].Yours {
					return offered[i].Yours
				}
				if offered[i].Name != offered[j].Name {
					return offered[i].Name < offered[j].Name
				}
				return offered[i].ID < offered[j].ID
			})
		}
		namedLabel := f.Dataset
		if f.Dataset != "" {
			if held, e := a.engine.Provisioned(r.Context()); e == nil {
				for _, d := range held {
					if d.ID == f.Dataset && d.DisplayName != "" {
						namedLabel = d.DisplayName
					}
				}
			}
		}
		if auth, reused, e := a.reuseConsent(r.Context(), f, principal, offered, flowID); reused {
			if e != nil {
				a.flowError(w, r, 503, "temporarily_unavailable", "busy")
				return
			}
			a.approve(w, r, f, auth, q, target)
			return
		}
		f.Principal, f.CSRF = principal.ID, random()
		a.flows[r.PostForm.Get("flow")] = f
		a.rememberAuthorization(r.PostForm.Get("flow"), f)
		http.SetCookie(w, &http.Cookie{Name: "tessera-flow", Value: f.CSRF, Path: "/oauth/authorize", Secure: strings.HasPrefix(a.issuer, "https://"), HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 300})
		defaults, missing := consentDefaults(f, principal, offered)
		a.pageHeaders(w)
		_ = consentPage.Execute(w, a.viewLocked(r, "consent", "consent.title", map[string]any{"ClientLogo": clientLogoPath(client), "ClientID": client.ID, "Name": client.Name, "Redirect": f.Redirect, "NamedLabel": namedLabel,
			"Flow": r.PostForm.Get("flow"), "CSRF": f.CSRF, "Principal": principal.ID,
			"Defaults": defaults, "MissingHints": missing, "Summary": consentSummary(client.Name, defaults),
			"Named": f.Dataset, "Datasets": offered, "MayCreate": principal.MayCreateDataset,
			"MayAdminister": principal.MayAdministerServer || len(administered(principal)) > 0,
			"OffersShared":  f.SharedReads, "SharedNow": len(sharedWith(principal))}))
		return
	}

	// Step two: the person is known, so this submission is the approval.
	principal := a.data.Principals[f.Principal]
	if r.PostForm.Get("decision") != "allow" {
		a.flowError(w, r, 401, "access_denied", "error.denied")
		return
	}
	// Reading what is shared is approved only where the application asked
	// for it and the person ticked it, whatever else the form carried; it is
	// never the default.
	shared := f.SharedReads && r.PostForm.Get("shared_reads") == "yes"
	// A named dataset is the one shown; otherwise the person chose, and the
	// choice has to be one they actually hold — a crafted form must not be
	// able to name a dataset that was never on the screen.
	dataset := f.Dataset
	if dataset == "" {
		dataset = r.PostForm.Get("dataset")
	}
	// Reading what is shared, and nothing else: no dataset of their own, no
	// right on the server.
	if shared && dataset == "" && !(r.PostForm.Get("create_dataset") == "yes" && principal.MayCreateDataset) &&
		!(r.PostForm.Get("administer") == "yes" && principal.MayAdministerServer) {
		// Consume before any grant side effect, including a partial failure.
		// Another submission must recover through a fresh authorization attempt.
		delete(a.flows, r.PostForm.Get("flow"))
		auth, err := a.newSharedReadsAuthorization(r.Context(), principal, f.Client)
		if err != nil {
			a.flowError(w, r, 503, "temporarily_unavailable", "busy")
			return
		}
		a.approve(w, r, f, auth, q, target)
		return
	}
	// Approving creation alone is the one case with no dataset, and it is what
	// a person holding none can approve. Otherwise the chosen dataset has to
	// be one they actually hold, whatever the submitted form said.
	if dataset != "" {
		if held, ok := principal.Datasets[dataset]; !ok || !(held.Read || held.Write || held.Schema || held.Erase || held.Insert) {
			a.flowError(w, r, 401, "access_denied", "error.denied")
			return
		}
	} else if creates := r.PostForm.Get("create_dataset") == "yes" && principal.MayCreateDataset; !creates &&
		// A server administrator holding no data approves administration
		// alone: the installation reaches no dataset, only accounts.
		!(r.PostForm.Get("administer") == "yes" && principal.MayAdministerServer) {
		a.flowError(w, r, 401, "access_denied", "error.denied")
		return
	} else if r.PostForm.Get("create_dataset") == "yes" && !principal.MayCreateDataset {
		a.flowError(w, r, 401, "access_denied", "error.denied")
		return
	}
	grants := store.Grants{Read: r.PostForm.Get("read") == "yes", Write: r.PostForm.Get("write") == "yes", Schema: r.PostForm.Get("schema") == "yes", Erase: r.PostForm.Get("erase") == "yes", Insert: r.PostForm.Get("insert") == "yes"}
	delete(a.flows, r.PostForm.Get("flow"))
	auth, err := a.newAuthorization(r.Context(), principal, f.Client, dataset, grants, r.PostForm.Get("create_dataset") == "yes", r.PostForm.Get("administer") == "yes")
	if err != nil {
		a.flowError(w, r, 503, "temporarily_unavailable", "busy")
		return
	}
	// Only for an account that administers something, whatever the form said.
	if r.PostForm.Get("administer") == "yes" && (principal.MayAdministerServer || len(administered(principal)) > 0) {
		auth.Administer = true
		a.data.Authorizations[auth.ID] = auth
	}
	// Beside its own dataset, where both were asked for and approved: one
	// sign-in for a person's data and everything shared with them.
	if shared {
		auth.SharedReads = true
		a.data.Authorizations[auth.ID] = auth
		if a.syncSharedReads(r.Context(), principal.ID) != nil {
			a.flowError(w, r, 503, "temporarily_unavailable", "busy")
			return
		}
	}
	a.approve(w, r, f, auth, q, target)
}

// approve hands the application its code for an approved installation. The
// caller holds the lock.
func (a *Authority) approve(w http.ResponseWriter, r *http.Request, f flow, auth Authorization, q url.Values, target *url.URL) {
	a.approveRemember(w, r, f, auth, q, target, false)
}

func (a *Authority) approveRemember(w http.ResponseWriter, r *http.Request, f flow, auth Authorization, q url.Values, target *url.URL, remember bool) {
	auth.ConsentRedirect = f.Redirect
	a.data.Authorizations[auth.ID] = auth
	code := random()
	a.data.Codes[digest(code)] = Code{Remember: remember, Authorization: auth.ID, Client: f.Client, Redirect: f.Redirect, Challenge: f.Challenge, Expires: time.Now().Add(time.Minute).Unix()}
	delete(a.flows, r.PostForm.Get("flow"))
	if a.save() != nil {
		a.flowError(w, r, 503, "temporarily_unavailable", "busy")
		return
	}
	q.Set("code", code)
	target.RawQuery = q.Encode()
	a.completeAuthorization(w, r, f, target)
}
func (a *Authority) token(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r) {
		return
	}
	if r.PostForm.Get("grant_type") == InvitationGrant {
		a.deviceToken(w, r)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.allowed(r, "token", 120) {
		oauthError(w, 429, "temporarily_unavailable")
		return
	}
	var auth Authorization
	var remember *bool
	var err error
	refresh := true
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		key := digest(r.PostForm.Get("code"))
		code, ok := a.data.Codes[key]
		verifier := r.PostForm.Get("code_verifier")
		hash := sha256.Sum256([]byte(verifier))
		challenge := base64.RawURLEncoding.EncodeToString(hash[:])
		if !ok || code.Expires <= time.Now().Unix() || code.Client != r.PostForm.Get("client_id") || code.Redirect != r.PostForm.Get("redirect_uri") || !pkceVerifier.MatchString(verifier) || !equal(code.Challenge, challenge) {
			oauthError(w, 400, "invalid_grant")
			return
		}
		if !a.active(a.data.Authorizations[code.Authorization].Principal) {
			oauthError(w, 400, accountSuspended)
			return
		}
		auth = a.data.Authorizations[code.Authorization]
		if auth.Console != nil {
			choice := code.Remember
			remember = &choice
		}
		delete(a.data.Codes, key)
	case "refresh_token":
		key := digest(r.PostForm.Get("refresh_token"))
		token, ok := a.data.Tokens[key]
		auth = a.data.Authorizations[token.Authorization]
		// Suspension is checked before the token is marked used. Refused
		// afterwards, a refresh attempted during a suspension spent the token,
		// the attempt after reinstatement read as reuse, and reuse detection
		// revoked the installation for good — so an application retrying in
		// the background was permanently cut off by something meant to be
		// reversible.
		if !ok || token.Kind != "refresh" || token.Expires <= time.Now().Unix() || auth.Client != r.PostForm.Get("client_id") || auth.Revoked {
			oauthError(w, 400, "invalid_grant")
			return
		}
		// A valid refresh token, not spent by being refused, is proof enough
		// to be told the account is suspended rather than signed out.
		if !a.active(auth.Principal) {
			oauthError(w, 400, accountSuspended)
			return
		}
		if token.Used {
			_ = a.revoke(r.Context(), auth.ID)
			oauthError(w, 400, "invalid_grant")
			return
		}
		token.Used = true
		a.data.Tokens[key] = token
	case "client_credentials":
		clientID, secret, ok := r.BasicAuth()
		client, exists := a.data.Clients[clientID]
		if !ok || !exists || client.Disabled || client.Secret == "" || !equal(client.Secret, digest(secret)) {
			oauthError(w, 401, "invalid_client")
			return
		}
		principal := a.data.Principals[client.Principal]
		// Before any authorization is created for it: a suspended service
		// should not accumulate installations it cannot use.
		if !a.active(principal.ID) {
			oauthError(w, 400, accountSuspended)
			return
		}
		// A service acting as itself meets no approval screen, so the dataset
		// comes from the request — always, with no exception for a storage that
		// currently holds one, and none for a credential that has asked before.
		//
		// Inferring it when there is a single possible answer is a latent
		// transition: a credential provisioned against one dataset keeps
		// working until somebody creates a second, and then fails at a
		// moment unrelated to any change in the service holding it. That
		// is tolerable for a person at a terminal who reads the error and
		// retries; it is a bad property for a deployed credential.
		dataset := r.PostForm.Get("dataset")
		if dataset == "" {
			oauthError(w, 400, "invalid_request")
			return
		}
		// Reuse is per dataset. Matching on the credential alone handed the
		// second request the first request's authorization, so a service that
		// named one dataset was served another and a service that named none
		// was served anything — which made the requirement above hold only
		// until the first token had been issued.
		for _, existing := range a.data.Authorizations {
			if existing.Client == clientID && existing.Principal == principal.ID && existing.Dataset == dataset && !existing.Revoked {
				auth = existing
				break
			}
		}
		if auth.ID == "" {
			// A service acting as itself carries create where its account holds
			// it, and never carries erase for the reason an automation token
			// does not: it outlives the moment anybody chose it.
			auth, err = a.newAuthorization(r.Context(), principal, clientID, dataset, store.Grants{Read: true, Write: true, Schema: true, Insert: true}, principal.MayCreateDataset, false)
		}
		if err != nil {
			// A refusal is the settled answer and retrying it earns the same
			// one, so only a storage that failed is reported as retryable.
			// Naming a dataset the account does not hold is the common case
			// here and it is permanent.
			if code := p.ErrorCode(err); code == "permission_denied" || code == "not_found" || code == "unavailable" {
				oauthError(w, 400, "invalid_grant")
				return
			}
			oauthError(w, 503, "temporarily_unavailable")
			return
		}
		refresh = false
	default:
		oauthError(w, 400, "unsupported_grant_type")
		return
	}
	if auth.ID == "" || auth.Revoked || !a.active(auth.Principal) {
		oauthError(w, 400, "invalid_grant")
		return
	}
	if auth.Console != nil {
		auth, err = a.syncConsole(r.Context(), auth)
		if err != nil {
			oauthError(w, 400, "invalid_grant")
			return
		}
	}
	// A shared-reads installation's reach follows the account's shares, and
	// every token it is issued brings it up to date, so a share made by a
	// route that did not do so is picked up within one access token.
	if auth.SharedReads {
		if a.syncSharedReads(r.Context(), auth.Principal) != nil {
			oauthError(w, 503, "temporarily_unavailable")
			return
		}
	}
	result := a.issue(auth, refresh)
	if remember != nil && auth.Console != nil {
		age := 0
		if *remember {
			age = 30 * 24 * 60 * 60
		}
		result["console_session"] = map[string]any{"origin": auth.Console.Origin, "keep_signed_in": *remember, "max_age_seconds": age}
	}
	if a.save() != nil {
		oauthError(w, 503, "temporarily_unavailable")
		return
	}
	writeJSON(w, 200, result)
}
func (a *Authority) revocation(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r) {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.allowed(r, "revoke", 120) {
		oauthError(w, 429, "temporarily_unavailable")
		return
	}
	token, ok := a.data.Tokens[digest(r.PostForm.Get("token"))]
	if !ok {
		w.WriteHeader(200)
		return
	}
	auth := a.data.Authorizations[token.Authorization]
	clientID := r.PostForm.Get("client_id")
	if basic, secret, has := r.BasicAuth(); has {
		client := a.data.Clients[basic]
		if client.Secret == "" || !equal(client.Secret, digest(secret)) {
			oauthError(w, 401, "invalid_client")
			return
		}
		clientID = basic
	}
	if clientID != auth.Client {
		oauthError(w, 401, "invalid_client")
		return
	}
	if a.revoke(r.Context(), auth.ID) != nil {
		oauthError(w, 503, "temporarily_unavailable")
		return
	}
	w.WriteHeader(200)
}

// SetTrustedProxies names the addresses whose X-Forwarded-For this server
// believes. Behind a reverse proxy every request arrives from the proxy, so
// without this the per-address limits are one budget for the whole server —
// anyone could spend it, and ten wrong passwords a minute would stop every
// account signing in. Empty by default, because a server exposed directly
// that believed the header would let a caller name any address it liked and
// never be limited at all.
func (a *Authority) SetTrustedProxies(proxies []netip.Prefix) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.proxies = append([]netip.Prefix(nil), proxies...)
}

// clientAddress is the address a request's limits are counted against: the
// connection's, unless that is a trusted proxy, in which case the right-most
// X-Forwarded-For entry that is not itself a trusted proxy. Right-most,
// because every entry to its left was written by whoever sent the request to
// the first proxy, and that is the caller whose address is in question.
func (a *Authority) clientAddress(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	if !a.trusted(peer) {
		return bucketAddress(peer)
	}
	var hops []string
	for _, header := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(header, ",")...)
	}
	for i := len(hops) - 1; i >= 0; i-- {
		hop := strings.TrimSpace(hops[i])
		addr, err := netip.ParseAddr(hop)
		if err != nil {
			// Not an address, so not a proxy this server trusts: whatever
			// wrote it is the client, and its text is only a bucket key.
			return "unparsed:" + hop
		}
		if !a.trusted(addr) {
			return bucketAddress(addr)
		}
	}
	// Every hop is a trusted proxy, or there were none: the request came
	// from the proxies themselves.
	return bucketAddress(peer)
}

func (a *Authority) trusted(addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, prefix := range a.proxies {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// bucketAddress is what a caller's limits are counted against. An IPv6
// caller is counted by its /64, because that is what one subscriber is
// given: counted per address, one household could present a fresh address
// with every request and never be limited.
func bucketAddress(addr netip.Addr) string {
	addr = addr.Unmap()
	if addr.Is6() {
		return netip.PrefixFrom(addr, 64).Masked().String()
	}
	return addr.String()
}

// maxClients bounds the registry's clients, which is rewritten whole on every
// change.
const maxClients = 10000

// unusedClientLifetime is how long a self-registered client may go without
// any sign-in before it can be removed to make room.
const unusedClientLifetime = time.Hour

// expireUnusedClients removes self-registered clients nothing uses: no
// authorization names them, no sign-in is in progress with them, and they
// registered more than unusedClientLifetime ago. It runs only when the
// registry is full.
//
// Registration is open, so without this the bound on clients was a budget
// spent once and never returned: one address at the registration limit
// filled it in hours, and after that no application could register again.
// An application that registered and never signed anybody in loses nothing
// it was using; it is told invalid_client and registers again.
func (a *Authority) expireUnusedClients() {
	used := map[string]bool{}
	for _, auth := range a.data.Authorizations {
		used[auth.Client] = true
	}
	for _, f := range a.flows {
		used[f.Client] = true
	}
	cutoff := time.Now().Add(-unusedClientLifetime).Unix()
	for id, client := range a.data.Clients {
		if client.Principal == "" && client.Secret == "" && !used[id] && client.Registered < cutoff {
			delete(a.data.Clients, id)
		}
	}
}
