package identity

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The pages a person sees: signing in, approving an application, accepting an
// invitation, and their own account. Most people meet them inside an app's
// sign-in sheet on a phone, so they are laid out for one, in the person's
// language, in plain words, and say where the person is.
//
// Pages load no scripts or fonts. The generated style is allowed by its hash;
// an optional operator logo is served only from a dedicated same-origin path.

const pageCSS = `:root{color-scheme:light dark;--bg:#fff;--fg:#1c1c1e;--muted:#6c6c70;--line:#d1d1d6;--accent:#0a64d8;--accent-fg:#fff;--alert:#b3261e;--alert-bg:#fdecea;--field:#f2f2f7}
@media (prefers-color-scheme:dark){:root{--bg:#000;--fg:#f2f2f7;--muted:#a1a1a6;--line:#38383a;--accent:#4c9bff;--accent-fg:#000;--alert:#ff8a80;--alert-bg:#3a1513;--field:#1c1c1e}}
*{box-sizing:border-box}
html{-webkit-text-size-adjust:100%;text-size-adjust:100%}
body{margin:0;background:var(--bg);color:var(--fg);font:17px/1.45 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,"Helvetica Neue",Arial,sans-serif;overflow-wrap:anywhere}
header{padding:12px 16px;border-bottom:1px solid var(--line);color:var(--muted);font-size:15px}
header strong{color:var(--fg);display:block;font-size:17px}
main{max-width:480px;margin:0 auto;padding:16px}
h1{font-size:24px;line-height:1.2;margin:8px 0 16px}
p,ul{margin:0 0 14px}
ul{padding-left:20px}
.note{color:var(--muted);font-size:15px}
.problem{background:var(--alert-bg);color:var(--alert);border-radius:10px;padding:12px 14px}
form{margin:0 0 16px}
label{display:block;margin:0 0 14px}
input[type=text],input[type=password],input:not([type]),select{display:block;width:100%;min-height:44px;margin-top:6px;padding:10px 12px;font:inherit;color:inherit;background:var(--field);border:1px solid var(--line);border-radius:10px}
label.check{display:flex;gap:12px;align-items:flex-start;min-height:44px;padding:10px 0;margin:0;border-bottom:1px solid var(--line)}
label.check input{width:22px;height:22px;margin:1px 0 0;flex:none}
.actions{display:flex;flex-direction:column;gap:10px;margin-top:20px}
button,.button{display:block;width:100%;min-height:44px;padding:11px 16px;font:inherit;font-weight:600;text-align:center;text-decoration:none;border-radius:12px;border:1px solid var(--accent);background:var(--accent);color:var(--accent-fg);cursor:pointer}
button.secondary,.button.secondary{background:transparent;color:var(--accent)}
.signin-card{max-width:440px;width:calc(100% - 32px);margin:32px auto;padding:24px;border:1px solid var(--line);border-radius:16px}.signin-card header{padding:0 0 16px;margin-bottom:20px}.signin-card h1{margin-bottom:20px}
.client-icon{display:block;width:48px;height:48px;object-fit:contain;margin-bottom:12px}summary{cursor:pointer;min-height:44px;padding:10px 0}details[open]{margin-bottom:16px}
:focus-visible{outline:3px solid var(--fg);outline-offset:3px}
code{font:600 20px/1.4 ui-monospace,Menlo,Consolas,monospace;letter-spacing:.04em;word-break:break-all}`

// pageSecurity is the asset-free default. Generated styles are hash-bound,
// forms post only here and framing is forbidden. Configured PNG endpoints
// are the only additional image sources allowed by pageHeaders.
var pageSecurity = func() string {
	sum := sha256.Sum256([]byte(pageCSS))
	return "default-src 'none'; style-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'; form-action 'self'; frame-ancestors 'none'"
}()

// pageBase holds what every page shares: the head, the place it is on, and
// the way back to an application. Pages are clones of it.
var pageBase = template.Must(template.New("base").Parse(`{{define "head"}}<!doctype html><html lang="{{.Lang}}"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta name="color-scheme" content="light dark"><meta name="referrer" content="no-referrer"><title>{{.Title}}</title>{{if .Refresh}}<meta http-equiv="refresh" content="{{.Refresh}}">{{end}}<style>{{.Style}}</style></head><body data-page="{{.Page}}">{{if not .Compact}}{{template "place" .}}{{end}}<main{{if .Compact}} class="signin-card"{{end}}>{{if .Compact}}{{template "place" .}}{{end}}<h1>{{.Title}}</h1>{{if .Problem}}<p class="problem" role="alert">{{.Problem}}</p>{{end}}{{end}}` +
	`{{define "clienticon"}}{{if .ClientLogo}}<img class="client-icon" src="{{.ClientLogo}}" alt="">{{end}}{{end}}` +
	`{{define "place"}}<header>{{if .Logo}}<img src="/oauth/brand/logo" alt="">{{end}}{{if .Place}}<strong>{{.Place}}</strong>{{end}}{{.Host}}</header>{{end}}` +
	`{{define "foot"}}{{if .Return}}<div class="actions"><a class="button" href="{{.Return}}">{{call .T "return" .ClientName}}</a></div><p class="note">{{if .WebConsole}}{{call .T "unverified.website" .ClientName}}{{else}}{{call .T "unverified" .ClientName}}{{end}}</p>{{end}}</main></body></html>{{end}}` +
	`{{define "carry"}}{{if .Return}}<input type="hidden" name="client_id" value="{{.ClientID}}"><input type="hidden" name="return_to" value="{{.Return}}">{{end}}{{end}}`))

// page parses one page over the shared parts.
func page(body string) *template.Template {
	return template.Must(template.Must(pageBase.Clone()).Parse(body))
}

// pageLanguages are the languages the pages are written in, English first as
// the one every other falls back to.
var pageLanguages = []string{"en"}

// language picks the page language from Accept-Language: the highest-weighted
// tag whose primary language is one the pages are written in, or English.
func language(r *http.Request) string {
	type choice struct {
		tag    string
		weight float64
		order  int
	}
	var choices []choice
	for i, part := range strings.Split(r.Header.Get("Accept-Language"), ",") {
		fields := strings.Split(strings.TrimSpace(part), ";")
		tag := strings.ToLower(strings.TrimSpace(fields[0]))
		weight := 1.0
		for _, f := range fields[1:] {
			if v, ok := strings.CutPrefix(strings.TrimSpace(f), "q="); ok {
				if parsed, err := strconv.ParseFloat(v, 64); err == nil {
					weight = parsed
				}
			}
		}
		if tag != "" && weight > 0 {
			choices = append(choices, choice{tag, weight, i})
		}
	}
	sort.SliceStable(choices, func(i, j int) bool { return choices[i].weight > choices[j].weight })
	for _, c := range choices {
		primary, _, _ := strings.Cut(c.tag, "-")
		for _, l := range pageLanguages {
			if primary == l {
				return l
			}
		}
	}
	return "en"
}

// text is the page wording in one language, falling back to English for a
// message not yet written in it.
func text(lang string) func(key string, args ...any) string {
	return func(key string, args ...any) string {
		s, ok := catalogue[lang][key]
		if !ok {
			s = catalogue["en"][key]
		}
		if len(args) > 0 {
			return fmt.Sprintf(s, args...)
		}
		return s
	}
}

// SetDisplayName gives the pages the deployment's name, the one discovery
// publishes, to show beside the host. It is the operator's claim, shown next to
// the address rather than instead of it.
func (a *Authority) SetDisplayName(name string) {
	a.displayName.Store(&name)
}

// view starts a page's values: its language and wording, and where the person
// is. It takes no lock, so a handler holding a.mu may call it.
func (a *Authority) view(r *http.Request, name, title string, values map[string]any) map[string]any {
	lang := language(r)
	t := text(lang)
	place := ""
	if named := a.displayName.Load(); named != nil {
		place = *named
	}
	host := a.issuer
	if u, err := url.Parse(a.issuer); err == nil && u.Host != "" {
		host = u.Host
	}
	css := pageCSS
	logo := false
	if b := a.brand.Load(); b != nil {
		place, css, logo = b.name, b.css, len(b.logo) > 0
	}
	v := map[string]any{"Style": template.CSS(css), "Logo": logo, "Lang": lang, "T": t, "Page": name, "Title": t(title), "Place": place, "Host": host}
	for k, value := range values {
		v[k] = value
	}
	if name == "signin" && a.brand.Load() != nil {
		v["Title"] = fmt.Sprintf("Sign in to %s", place)
	}
	if a.webConsole(fmt.Sprint(v["ClientID"]), fmt.Sprint(v["Redirect"])) {
		if name == "complete" {
			v["Title"] = t("complete.website.title")
		}
		// All permission explanations use the same destination wording; their
		// meaning and individual consent choices do not change.
		v["T"] = func(key string, args ...any) string {
			switch key {
			case "signin.note", "consent.change", "consent.can", "consent.missing", "consent.administer.note", "consent.create.note", "consent.shared.note":
				key = "website." + key
			}
			return t(key, args...)
		}
		v["WebConsole"] = true
	}
	if _, firstParty := a.console(fmt.Sprint(v["ClientID"]), fmt.Sprint(v["Redirect"])); firstParty {
		v["WebConsole"] = true
		if name == "signin" {
			v["Compact"] = true
			v["Title"] = t("signin.title")
		}
	}
	return v
}

// viewLocked is view, named for the handlers that call it holding a.mu.
func (a *Authority) viewLocked(r *http.Request, name, title string, values map[string]any) map[string]any {
	return a.view(r, name, title, values)
}

// parseFlowForm is parseForm for a sign-in step, whose failure a person may
// be looking at.
func (a *Authority) parseFlowForm(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") || r.ParseForm() != nil {
		a.flowError(w, r, 400, "invalid_request", "error.expired")
		return false
	}
	for _, values := range r.PostForm {
		if len(values) != 1 {
			a.flowError(w, r, 400, "invalid_request", "error.expired")
			return false
		}
	}
	return true
}

func (a *Authority) pageHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	policy := pageSecurity
	if b := a.brand.Load(); b != nil {
		policy = b.policy
	}
	if a.hasClientLogos.Load() {
		if strings.Contains(policy, "; img-src ") {
			policy += " " + a.issuer + "/oauth/client-logos/"
		} else {
			policy += "; img-src " + a.issuer + "/oauth/client-logos/"
		}
	}
	w.Header().Set("Content-Security-Policy", policy)
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Vary", "Accept-Language")
}

// wantsPage reports whether the caller is a browser showing a person this
// response, rather than an application reading it.
func wantsPage(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

var errorPage = page(`{{template "head" .}}<p>{{.Message}}</p>{{template "foot" .}}`)

// flowError answers a failed step of signing in. A browser is shown a page
// saying what to do next, in the person's language; an application reading
// the response gets the same status and OAuth error it always did.
func (a *Authority) flowError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	if !wantsPage(r) {
		oauthError(w, status, code)
		return
	}
	a.pageHeaders(w)
	v := a.view(r, "error", "error.title", nil)
	v["Message"] = v["T"].(func(string, ...any) string)(message)
	w.WriteHeader(status)
	_ = errorPage.Execute(w, v)
}

var consoleCompletePage = page(`{{template "head" .}}<p>{{call .T "console.finishing"}}</p>{{template "foot" .}}`)

var authorizationCompletePage = page(`{{template "head" .}}{{template "clienticon" .}}<p>{{if .WebConsole}}{{call .T "complete.website"}}{{else}}{{call .T "complete.note"}}{{end}}</p>{{template "foot" .}}`)

// completeAuthorization ends a browser form submission here, rather than
// redirecting that submission outside form-action 'self'. The explicit link
// starts a new, user-initiated navigation, including for native app schemes.
// f.Redirect was matched to the client's registration before the flow existed;
// target is that URI with only the OAuth response parameters added by consent.
// Never accept a posted return address here. The caller holds a.mu.
func (a *Authority) completeAuthorization(w http.ResponseWriter, r *http.Request, f flow, target *url.URL) {
	if !wantsPage(r) {
		http.Redirect(w, r, target.String(), http.StatusSeeOther)
		return
	}
	if f.Console {
		if _, ok := a.console(f.Client, f.Redirect); !ok {
			a.flowError(w, r, 400, "invalid_request", "error.link")
			return
		}
		a.pageHeaders(w)
		// A same-origin response ends the POST. The subsequent document's
		// navigation is not a cross-origin form redirect under form-action.
		_ = consoleCompletePage.Execute(w, a.viewLocked(r, "complete", "console.finishing", map[string]any{
			"Refresh": "0;url=" + target.String(), "Return": template.URL(target.String()),
			"ClientName": a.data.Clients[f.Client].Name, "ClientLogo": clientLogoPath(a.data.Clients[f.Client]), "ClientID": f.Client, "Redirect": f.Redirect,
		}))
		return
	}
	a.pageHeaders(w)
	_ = authorizationCompletePage.Execute(w, a.viewLocked(r, "complete", "complete.title", map[string]any{
		"ClientLogo": clientLogoPath(a.data.Clients[f.Client]), "ClientID": f.Client, "Redirect": f.Redirect,
		"Return":     template.URL(target.String()), // Trusted only after registered redirect matching.
		"ClientName": a.data.Clients[f.Client].Name,
	}))
}

// Recovery never repeats consent, grants, or code issuance. It offers a fresh
// authorization GET, which revalidates registration and requires both steps.
const authorizationRecoveryWindow = 5 * time.Minute
const maxAuthorizationRecoveries = 1000

func (a *Authority) rememberAuthorization(id string, f flow) {
	if a.recoveries == nil {
		a.recoveries = make(map[string]flow)
	}
	now := time.Now()
	for key, saved := range a.recoveries {
		if now.After(saved.Expires.Add(authorizationRecoveryWindow)) {
			delete(a.recoveries, key)
		}
	}
	if _, exists := a.recoveries[id]; !exists && len(a.recoveries) >= maxAuthorizationRecoveries {
		var oldest string
		for key, saved := range a.recoveries {
			if oldest == "" || saved.Expires.Before(a.recoveries[oldest].Expires) {
				oldest = key
			}
		}
		delete(a.recoveries, oldest)
	}
	f.Principal = "" // A retry cannot carry authentication into another attempt.
	a.recoveries[id] = f
}

var authorizationRestartPage = page(`{{template "head" .}}{{template "clienticon" .}}<p>{{if .WebConsole}}{{call .T "restart.website"}}{{else}}{{call .T "restart.note"}}{{end}}</p><div class="actions"><a class="button" href="{{.Restart}}">{{call .T "restart.title"}}</a></div>{{template "foot" .}}`)

// Called only after the original flow cookie and form CSRF have been verified.
// No raw submitted return URL or authorization query is used.
func (a *Authority) restartAuthorization(w http.ResponseWriter, r *http.Request, f flow) {
	if !wantsPage(r) {
		a.flowError(w, r, http.StatusBadRequest, "invalid_request", "error.expired")
		return
	}
	query := url.Values{
		"client_id": {f.Client}, "redirect_uri": {f.Redirect}, "state": {f.State},
		"response_type": {"code"}, "code_challenge_method": {"S256"}, "code_challenge": {f.Challenge},
	}
	if f.Dataset != "" {
		query.Set("dataset", f.Dataset)
	}
	if f.ForceConsent {
		query.Set("prompt", "consent")
	}
	if f.HasScope {
		query.Set("scope", f.Scope)
	}
	if f.SharedReads {
		query.Set("shared_reads", "true")
	}
	a.pageHeaders(w)
	w.WriteHeader(http.StatusBadRequest)
	_ = authorizationRestartPage.Execute(w, a.viewLocked(r, "restart", "restart.title", map[string]any{
		"ClientLogo": clientLogoPath(a.data.Clients[f.Client]), "ClientID": f.Client, "Redirect": f.Redirect,
		"Restart": "/oauth/authorize?" + query.Encode(),
	}))
}
