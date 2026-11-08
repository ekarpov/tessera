package identity

import (
	"net/http"
	"net/url"
	"time"

	p "github.com/ekarpov/tessera/pkg/protocol"
)

// invite hands an issuer one invitation. The issuer authenticates with its own
// identifier and secret, as a confidential client does at the token endpoint.
//
// The answer carries the code and a link to redeem it. The link puts the code
// in the query, where a link a person follows from an email has to put it;
// every response here is sent with no-referrer, so it is not handed on to
// another origin, and the code is single-use and expires.
func (a *Authority) invite(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	admitted := a.allowed(r, "invite", 60)
	a.mu.Unlock()
	if !admitted {
		w.Header().Set("Retry-After", "60")
		oauthError(w, 429, "temporarily_unavailable")
		return
	}
	id, secret, ok := r.BasicAuth()
	if !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="invitations"`)
		oauthError(w, 401, "invalid_client")
		return
	}
	// An issuer that assigns names may name the account; otherwise one is
	// generated. Read from a form body, which is all this endpoint accepts.
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if r.ParseForm() != nil {
		oauthError(w, 400, "invalid_request")
		return
	}
	// A rebinding names the existing account it reconnects to a new device.
	if rebind := r.PostForm.Get("rebind"); rebind != "" {
		code, expires, err := a.IssueRebinding(id, secret, rebind)
		switch code := p.ErrorCode(err); {
		case err == nil:
		case code == "authentication_failed":
			oauthError(w, 401, "invalid_client")
			return
		case code == "invalid_field", code == "not_found":
			oauthError(w, 403, "invitation_refused")
			return
		default:
			oauthError(w, 503, "temporarily_unavailable")
			return
		}
		writeJSON(w, 201, map[string]any{"invitation": code, "account": rebind, "rebind": true, "expires_at": expires.UTC().Format(time.RFC3339)})
		return
	}
	code, account, expires, err := a.IssueInvitation(id, secret, r.PostForm.Get("account"))
	// Success first: ErrorCode answers internal_error for a nil error rather
	// than an empty string, so a switch whose success case is "" never takes it.
	switch code := p.ErrorCode(err); {
	case err == nil:
	case code == "authentication_failed":
		oauthError(w, 401, "invalid_client")
		return
	case code == "invalid_field":
		oauthError(w, 400, "invalid_request")
		return
	case code == "conflict" && account == "" && r.PostForm.Get("account") != "":
		oauthError(w, 409, "account_name_unavailable")
		return
	case code == "conflict":
		// The cap is spent, or an assigned name is unavailable. Not retryable
		// as sent: waiting does not raise a cap or free a name.
		oauthError(w, 403, "invitation_refused")
		return
	default:
		oauthError(w, 503, "temporarily_unavailable")
		return
	}
	answer := map[string]any{"invitation": code, "redeem_url": a.RedeemURL(code), "expires_at": expires.UTC().Format(time.RFC3339)}
	if account != "" {
		answer["account"] = account
	}
	writeJSON(w, 201, answer)
}

var redeemPage = page(`{{template "head" .}}` +
	`{{if .Done}}<p data-account="{{.Account}}">{{call .T "redeem.done" .Account}}</p>{{template "recovery" .}}` +
	`{{if .Return}}<div class="actions"><a class="button" href="{{.Return}}">{{call .T "redeem.continue" .ClientName}}</a></div><p class="note">{{call .T "unverified" .ClientName}}</p>{{end}}</main></body></html>` +
	`{{else}}<p>{{call .T "redeem.intro"}}</p>` +
	`<form method="post" action="/oauth/invitation"><input type="hidden" name="code" value="{{.Code}}">{{template "carry" .}}` +
	`{{if .Assigned}}<p data-account="{{.Assigned}}">{{call .T "redeem.assigned" .Assigned}}</p>{{else}}<label>{{call .T "account"}}<input name="account" autocomplete="username" autocapitalize="none" autocorrect="off" spellcheck="false" required></label>{{end}}` +
	`<label>{{call .T "password.new"}}<input name="password" type="password" autocomplete="new-password" minlength="12" required></label>` +
	`<div class="actions"><button>{{call .T "redeem.submit"}}</button></div></form>{{template "foot" .}}{{end}}` +
	`{{define "recovery"}}<p>{{call .T "recovery.yours"}}</p><p><code id="recovery-code" data-recovery-code="{{.Recovery}}">{{.Recovery}}</code></p><p><strong>{{call .T "recovery.keep"}}</strong></p>{{end}}`)

// RedeemURL is where the person an invitation is for goes to redeem it.
func (a *Authority) RedeemURL(code string) string {
	return a.issuer + "/oauth/invitation?" + url.Values{"code": {code}}.Encode()
}

// redeem shows the invitation form and creates the account from it.
func (a *Authority) redeem(w http.ResponseWriter, r *http.Request) {
	// The page's address carries the invitation code, and a way back into an
	// application is a link off it: pageHeaders sends no referrer.
	a.pageHeaders(w)
	render := func(values map[string]any) {
		v := a.view(r, "redeem", "redeem.title", values)
		if problem, ok := v["Problem"].(string); ok && problem != "" {
			v["Problem"] = v["T"].(func(string, ...any) string)(problem)
		}
		_ = redeemPage.Execute(w, v)
	}
	if r.Method == http.MethodGet {
		code := r.URL.Query().Get("code")
		render(with(a.returnLink(r), map[string]any{"Code": code, "Assigned": a.InvitationAccount(code)}))
		return
	}
	a.mu.Lock()
	admitted := a.allowed(r, "redeem", 20)
	a.mu.Unlock()
	if !admitted {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(429)
		render(map[string]any{"Problem": "attempts"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	if err := r.ParseForm(); err != nil {
		w.WriteHeader(400)
		render(map[string]any{"Problem": "redeem.unreadable"})
		return
	}
	// A product that sent the person here may be returned to, when the
	// address it gave is one of its registered redirect URIs — the rule the
	// account pages follow.
	back := a.returnLink(r)
	code, account := r.PostForm.Get("code"), r.PostForm.Get("account")
	// Read before redeeming: the invitation, and the name it assigns, are gone
	// once it is used.
	assigned := a.InvitationAccount(code)
	if assigned != "" {
		account = assigned
	}
	recovery, err := a.RedeemInvitation(r.Context(), code, account, r.PostForm.Get("password"))
	problem := ""
	status := 201
	switch code := p.ErrorCode(err); {
	case err == nil:
	case code == "invalid_field":
		status, problem = 400, "redeem.invalid"
	case code == "conflict":
		status, problem = 409, "redeem.taken"
	case code == "not_found":
		// One answer for an unknown, used and expired invitation: which it was
		// would tell whoever holds a guessed code that it had once been real.
		status, problem = 404, "redeem.gone"
	default:
		status, problem = 503, "redeem.failed"
	}
	w.WriteHeader(status)
	if problem != "" {
		render(with(back, map[string]any{"Code": code, "Problem": problem, "Assigned": assigned}))
		return
	}
	render(with(back, map[string]any{"Done": true, "Account": account, "Recovery": recovery}))
}

// with lays a page's own values over the way back it may offer.
func with(back, values map[string]any) map[string]any {
	merged := map[string]any{}
	for k, v := range back {
		merged[k] = v
	}
	for k, v := range values {
		merged[k] = v
	}
	return merged
}
