package identity

import (
	"errors"
	"html/template"
	"net/http"
	"slices"
	"time"

	p "github.com/ekarpov/tessera/pkg/protocol"
)

// The account pages are the person's own: deleting their account and changing
// their password. Tessera renders them itself, like the invitation page,
// because the password has to go from the person's browser to this server and
// nowhere else — a product that took it on its own page to pass along would
// hold it, which is exactly what signing in through this server avoids.
//
// A product may send the person here with its client_id and one of its
// registered redirect URIs as return_to; the pages then offer a way back. The
// return address is checked against the registration, as a sign-in's is, so
// these pages cannot be made to send anybody anywhere else.

var deletePage = page(`{{template "head" .}}` +
	`{{if .Done}}<p>{{call .T "delete.done" .Account}}</p>` +
	`{{if .Erased}}<p>{{call .T "delete.erased"}}</p><ul>{{range .Erased}}<li>{{.}}</li>{{end}}</ul>{{end}}` +
	`{{if .Kept}}<p><strong>{{call .T "delete.kept"}}</strong></p><ul>{{range .Kept}}<li>{{.}}</li>{{end}}</ul>{{end}}` +
	`{{else if .Ticket}}<p>{{call .T "delete.confirm" .Account}}</p>` +
	`{{if .Erases}}<p><strong>{{call .T "delete.erases"}}</strong></p><ul>{{range .Erases}}<li>{{.}}</li>{{end}}</ul>{{else}}<p>{{call .T "delete.erasesnone"}}</p>{{end}}` +
	`{{if .Keeps}}<p><strong>{{call .T "delete.keeps"}}</strong></p><ul>{{range .Keeps}}<li>{{.}}</li>{{end}}</ul>{{end}}` +
	`<form method="post" action="/oauth/account/delete"><input type="hidden" name="page" value="1"><input type="hidden" name="ticket" value="{{.Ticket}}">{{template "carry" .}}<div class="actions"><button name="decision" value="delete">{{call .T "delete.submit"}}</button></div></form>` +
	`{{else}}<p>{{call .T "delete.intro"}}</p>` +
	`<form method="post" action="/oauth/account/delete"><input type="hidden" name="page" value="1">{{template "carry" .}}` +
	`<label>{{call .T "account"}}<input name="account" autocomplete="username" autocapitalize="none" autocorrect="off" spellcheck="false" required></label>` +
	`<label>{{call .T "password"}}<input name="password" type="password" autocomplete="current-password" required></label>` +
	`<div class="actions"><button>{{call .T "continue"}}</button></div></form>{{end}}{{template "foot" .}}`)

var passwordPage = page(`{{template "head" .}}` +
	`{{if .Done}}{{if .Recovery}}<p data-account="{{.Account}}">{{call .T "password.chosen"}}</p><p>{{call .T "recovery.yours"}}</p><p><code id="recovery-code" data-recovery-code="{{.Recovery}}">{{.Recovery}}</code></p><p><strong>{{call .T "recovery.keep"}}</strong></p>{{else}}<p>{{call .T "password.done"}}</p>{{end}}` +
	`{{else if .Link}}<p data-account="{{.Account}}">{{call .T "password.first" .Account}}</p>` +
	`<form method="post" action="/oauth/account/password"><input type="hidden" name="page" value="1"><input type="hidden" name="link" value="{{.Link}}">` +
	`<label>{{call .T "password.new"}}<input name="new_password" type="password" autocomplete="new-password" minlength="12" required></label>` +
	`<div class="actions"><button>{{call .T "password.set"}}</button></div></form>{{else}}` +
	`<form method="post" action="/oauth/account/password"><input type="hidden" name="page" value="1">{{template "carry" .}}` +
	`<label>{{call .T "account"}}<input name="account" autocomplete="username" autocapitalize="none" autocorrect="off" spellcheck="false" required></label>` +
	`<label>{{call .T "password.current"}}<input name="password" type="password" autocomplete="current-password" required></label>` +
	`<label>{{call .T "password.new"}}<input name="new_password" type="password" autocomplete="new-password" minlength="12" required></label>` +
	`<div class="actions"><button>{{call .T "password.submit"}}</button></div></form>{{end}}{{template "foot" .}}`)

// deletionTicket is what a proved password buys on the deletion page: the
// right to confirm deleting this account, with exactly the erasure it was
// shown, for a few minutes, once. It keeps the password out of the
// confirmation form.
type deletionTicket struct {
	Account string
	Erases  []string
	Expires time.Time
}

const ticketLifetime = 5 * time.Minute

// returnLink reads a product's way back from the request, and keeps it only
// when it is one of that product's registered redirect URIs.
func (a *Authority) returnLink(r *http.Request) map[string]any {
	clientID, target := r.FormValue("client_id"), r.FormValue("return_to")
	if clientID == "" || target == "" {
		return map[string]any{}
	}
	a.mu.Lock()
	client, ok := a.data.Clients[clientID]
	a.mu.Unlock()
	if !ok || client.Disabled {
		return map[string]any{}
	}
	for _, registered := range client.Redirects {
		if redirectMatches(registered, target) {
			// Marked as a URL the page may link to, because it is one: it is
			// exactly a redirect this client registered, and registration
			// refuses javascript:, data: and file:. Left unmarked, the template
			// replaced a private-use scheme — what an application registers —
			// with a dead link, so the way back never worked for an app.
			return map[string]any{"ClientID": clientID, "Return": template.URL(target), "ClientName": client.Name}
		}
	}
	return map[string]any{}
}

// deletion routes the deletion endpoint: the page when a browser asks for it,
// the JSON API otherwise.
func (a *Authority) deletion(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		a.pageHeaders(w)
		_ = deletePage.Execute(w, a.view(r, "delete", "delete.title", a.returnLink(r)))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	if err := r.ParseForm(); err != nil {
		oauthError(w, 400, "invalid_request")
		return
	}
	if r.PostForm.Get("page") != "1" {
		a.deleteOwnAccount(w, r)
		return
	}
	a.pageHeaders(w)
	view := a.view(r, "delete", "delete.title", a.returnLink(r))
	say := view["T"].(func(string, ...any) string)
	show := func(status int, problem string) {
		if problem != "" {
			view["Problem"] = say(problem)
		}
		w.WriteHeader(status)
		_ = deletePage.Execute(w, view)
	}
	if ticket := r.PostForm.Get("ticket"); ticket != "" {
		a.confirmDeletion(r, ticket, view, show)
		return
	}
	a.mu.Lock()
	admitted := a.allowed(r, "account-delete", 10)
	a.mu.Unlock()
	if !admitted {
		w.Header().Set("Retry-After", "60")
		show(429, "attempts")
		return
	}
	account := r.PostForm.Get("account")
	a.mu.Lock()
	principal, ok := a.data.Principals[account]
	a.mu.Unlock()
	derived, err := a.derive(r.Context(), r.PostForm.Get("password"), principal.Salt)
	if err != nil {
		w.Header().Set("Retry-After", "5")
		show(503, "busy")
		return
	}
	if !ok || principal.Kind != "person" || principal.Suspended || !equal(derived, principal.Password) {
		show(401, "delete.mismatch")
		return
	}
	erases := a.DeletionErases(account)
	names := a.datasetNames(r)
	token := random()
	a.mu.Lock()
	now := time.Now()
	if a.tickets == nil {
		a.tickets = map[string]deletionTicket{}
	}
	for key, t := range a.tickets {
		if now.After(t.Expires) {
			delete(a.tickets, key)
		}
	}
	if !a.unchanged(account, principal) {
		a.mu.Unlock()
		show(401, "delete.mismatch")
		return
	}
	a.tickets[digest(token)] = deletionTicket{Account: account, Erases: erases, Expires: now.Add(ticketLifetime)}
	current := a.data.Principals[account]
	a.mu.Unlock()
	keeps := []string{}
	for dataset, g := range current.Datasets {
		if dataset != "" && (g.Read || g.Write || g.Schema || g.Erase) && !slices.Contains(erases, dataset) {
			keeps = append(keeps, names(dataset))
		}
	}
	slices.Sort(keeps)
	shown := make([]string, 0, len(erases))
	for _, dataset := range erases {
		shown = append(shown, names(dataset))
	}
	view["Account"], view["Ticket"], view["Erases"], view["Keeps"] = account, token, shown, keeps
	show(200, "")
}

// confirmDeletion spends a ticket. The erasure is the one the ticket was shown
// with; if what the account would erase has changed since, nothing happens and
// the person starts again, rather than confirming something they did not see.
func (a *Authority) confirmDeletion(r *http.Request, token string, view map[string]any, show func(int, string)) {
	a.mu.Lock()
	ticket, ok := a.tickets[digest(token)]
	delete(a.tickets, digest(token))
	// Suspended since the password was proved is suspended: the ticket was
	// the person's standing then, not a way round a decision made after it.
	suspended := a.data.Principals[ticket.Account].Suspended
	a.mu.Unlock()
	if ok && suspended {
		show(401, "delete.suspended")
		return
	}
	if !ok || time.Now().After(ticket.Expires) || r.PostForm.Get("decision") != "delete" {
		show(400, "delete.expired")
		return
	}
	names := a.datasetNames(r)
	kept, err := a.DeleteAccount(r.Context(), ticket.Account, ticket.Erases)
	var refusal *p.Error
	switch {
	case err == nil:
	case errors.As(err, &refusal) && refusal.Details["reason"] == "erasure_required":
		show(409, "delete.changed")
		return
	case errors.As(err, &refusal) && refusal.Details["reason"] == "last_server_administrator":
		show(409, "delete.lastadmin")
		return
	case p.ErrorCode(err) == "not_found":
		show(410, "delete.gone")
		return
	default:
		show(503, "delete.failed")
		return
	}
	erased := make([]string, 0, len(ticket.Erases))
	for _, dataset := range ticket.Erases {
		erased = append(erased, names(dataset))
	}
	keptNames := make([]string, 0, len(kept))
	for _, k := range kept {
		keptNames = append(keptNames, names(k.Dataset))
	}
	view["Done"], view["Account"], view["Erased"], view["Kept"] = true, ticket.Account, erased, keptNames
	show(200, "")
}

// datasetNames describes a dataset to its person by name and identity. A
// storage that cannot be listed just now still yields the identity.
func (a *Authority) datasetNames(r *http.Request) func(string) string {
	names := map[string]string{}
	if held, err := a.engine.Provisioned(r.Context()); err == nil {
		for _, d := range held {
			names[d.ID] = d.DisplayName
		}
	}
	return func(id string) string {
		if name := names[id]; name != "" {
			return name + " (" + id + ")"
		}
		return id
	}
}

// passwordChange routes the password endpoint: the page for a browser, the JSON API
// otherwise.
func (a *Authority) passwordChange(w http.ResponseWriter, r *http.Request) {
	page := r.Method == http.MethodGet
	if !page {
		r.Body = http.MaxBytesReader(w, r.Body, 8192)
		if err := r.ParseForm(); err != nil {
			oauthError(w, 400, "invalid_request")
			return
		}
		page = r.PostForm.Get("page") == "1"
	}
	view := a.view(r, "password", "password.title", a.returnLink(r))
	say := view["T"].(func(string, ...any) string)
	respond := func(status int, code, problem string) {
		if !page {
			if status == 200 {
				writeJSON(w, 200, map[string]any{"changed": true})
				return
			}
			oauthError(w, status, code)
			return
		}
		a.pageHeaders(w)
		if problem != "" {
			view["Problem"] = say(problem)
		}
		view["Done"] = status == 200
		w.WriteHeader(status)
		_ = passwordPage.Execute(w, view)
	}
	// A link from an account with no password sets its first one: the person
	// types it here, on this server's page, never in the app that asked.
	if link := r.FormValue("link"); link != "" {
		a.firstPassword(w, r, link, view, say)
		return
	}
	if r.Method == http.MethodGet {
		a.pageHeaders(w)
		_ = passwordPage.Execute(w, view)
		return
	}
	a.mu.Lock()
	admitted := a.allowed(r, "password-change", 10)
	a.mu.Unlock()
	if !admitted {
		w.Header().Set("Retry-After", "60")
		respond(429, "slow_down", "attempts")
		return
	}
	err := a.ChangeOwnPassword(r.Context(), r.PostForm.Get("account"), r.PostForm.Get("password"), r.PostForm.Get("new_password"))
	switch code := p.ErrorCode(err); {
	case err == nil:
		respond(200, "", "")
	case code == "invalid_field":
		respond(400, "invalid_request", "short")
	case code == "authentication_failed":
		respond(401, "access_denied", "password.mismatch")
	default:
		w.Header().Set("Retry-After", "5")
		respond(503, "temporarily_unavailable", "failed")
	}
}

// firstPassword serves the password page reached through a link: the form,
// then the account's first password and its recovery code.
func (a *Authority) firstPassword(w http.ResponseWriter, r *http.Request, link string, view map[string]any, say func(string, ...any) string) {
	a.pageHeaders(w)
	account := a.PasswordLinkAccount(link)
	if account == "" {
		view["Problem"] = say("password.linkgone")
		w.WriteHeader(404)
		_ = passwordPage.Execute(w, view)
		return
	}
	view["Account"], view["Link"] = account, link
	if r.Method == http.MethodGet {
		_ = passwordPage.Execute(w, view)
		return
	}
	a.mu.Lock()
	admitted := a.allowed(r, "password-change", 10)
	a.mu.Unlock()
	if !admitted {
		w.Header().Set("Retry-After", "60")
		view["Problem"] = say("attempts")
		w.WriteHeader(429)
		_ = passwordPage.Execute(w, view)
		return
	}
	recovery, err := a.SetPasswordByLink(r.Context(), link, r.PostForm.Get("new_password"))
	switch code := p.ErrorCode(err); {
	case err == nil:
		view["Done"], view["Recovery"] = true, recovery
	case code == "invalid_field":
		view["Problem"] = say("short")
		w.WriteHeader(400)
	case code == "authentication_failed":
		delete(view, "Link")
		view["Problem"] = say("password.linkgone")
		w.WriteHeader(404)
	default:
		w.Header().Set("Retry-After", "5")
		view["Problem"] = say("failed")
		w.WriteHeader(503)
	}
	_ = passwordPage.Execute(w, view)
}
