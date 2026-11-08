package identity

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"net/http"
	"strings"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// Recovery codes are how a person who forgets their password gets back in
// where nobody may set it for them.
//
// Setting another person's password is reading their data, so on a server
// where the people who administer it may not read members' data — or where a
// participant is a pseudonym with no address to send anything to — nobody
// can help a person who forgets. A recovery code is theirs alone: shown to
// them once, when their account is made or when they ask for a new one with
// their password, and kept here only as a digest. Using it sets a new
// password, signs out everything signed in as them, and replaces the code.
//
// It is 100 random bits, so a digest is enough to keep it and guessing it is
// not a question of how many attempts are allowed. Nobody else is ever shown
// one, administrators included.

// newRecoveryCode returns a code for the person and the digest to keep.
func newRecoveryCode() (string, string) {
	var raw [15]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic("tessera: no randomness for a recovery code")
	}
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw[:])
	groups := make([]string, 0, 6)
	for i := 0; i < len(encoded); i += 4 {
		groups = append(groups, encoded[i:min(i+4, len(encoded))])
	}
	code := strings.Join(groups, "-")
	return code, digest(normalRecoveryCode(code))
}

// normalRecoveryCode forgives what people do when copying a code by hand:
// case, spaces and where the dashes went.
func normalRecoveryCode(code string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '-' || r == ' ':
			return -1
		case r >= 'a' && r <= 'z':
			return r - 'a' + 'A'
		}
		return r
	}, code)
}

// IssueRecoveryCode gives a person a new recovery code, proving their current
// password, and ends the old one.
func (a *Authority) IssueRecoveryCode(ctx context.Context, account, secret string) (string, error) {
	a.mu.Lock()
	principal, ok := a.data.Principals[account]
	a.mu.Unlock()
	proved, err := a.derive(ctx, secret, principal.Salt)
	if err != nil {
		return "", err
	}
	if !ok || principal.Kind != "person" || principal.Suspended || !equal(proved, principal.Password) {
		return "", store.Refuse("authentication_failed")
	}
	code, kept := newRecoveryCode()
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.unchanged(account, principal) {
		return "", store.Refuse("authentication_failed")
	}
	principal = a.data.Principals[account]
	principal.Recovery = kept
	a.data.Principals[account] = principal
	return code, a.save()
}

// Recover sets a new password for a person holding their recovery code. It
// signs out every installation — whoever led the person to forget, or took
// the password, must not stay signed in — and replaces the code, returning
// the new one, which is shown once.
func (a *Authority) Recover(ctx context.Context, account, code, next string) (string, error) {
	if len(next) < 12 {
		return "", store.Refuse("invalid_field")
	}
	salt := random()
	derived, err := a.derive(ctx, next, salt)
	if err != nil {
		return "", err
	}
	fresh, kept := newRecoveryCode()
	a.mu.Lock()
	defer a.mu.Unlock()
	principal, ok := a.data.Principals[account]
	// A suspended account is somebody else's decision, and recovering does not
	// get round it.
	if !ok || principal.Kind != "person" || principal.Suspended || principal.Recovery == "" ||
		!equal(digest(normalRecoveryCode(code)), principal.Recovery) {
		return "", store.Refuse("authentication_failed")
	}
	for authID, auth := range a.data.Authorizations {
		if auth.Principal != account || auth.Revoked {
			continue
		}
		if err = a.revokeEverywhere(ctx, auth); err != nil {
			return "", err
		}
		auth.Revoked = true
		a.data.Authorizations[authID] = auth
	}
	principal.Salt, principal.Password, principal.Recovery = salt, derived, kept
	principal.PasswordSetBy, principal.PasswordSetAt = "", 0
	a.data.Principals[account] = principal
	a.endPendingSignIns(account)
	return fresh, a.save()
}

var recoveryPage = page(`{{template "head" .}}` +
	`{{if .Code}}{{if .Recover}}<p>{{call .T "recover.done"}}</p>{{end}}<p>{{call .T "recovery.yours"}}</p><p><code id="recovery-code" data-recovery-code="{{.Code}}">{{.Code}}</code></p><p><strong>{{call .T "recovery.keep"}}</strong></p><p class="note">{{call .T "recovery.used"}}</p>` +
	`{{else if .Recover}}<p>{{call .T "recover.intro"}}</p>` +
	`<form method="post" action="/oauth/account/recover"><input type="hidden" name="page" value="1">{{template "carry" .}}` +
	`<label>{{call .T "account"}}<input name="account" autocomplete="username" autocapitalize="none" autocorrect="off" spellcheck="false" required></label>` +
	`<label>{{call .T "recovery.code"}}<input name="code" autocomplete="off" autocapitalize="characters" spellcheck="false" required></label>` +
	`<label>{{call .T "password.new"}}<input name="new_password" type="password" autocomplete="new-password" minlength="12" required></label>` +
	`<div class="actions"><button>{{call .T "recover.submit"}}</button></div></form>` +
	`{{else}}<p>{{call .T "recovery.intro"}}</p>` +
	`<form method="post" action="/oauth/account/recovery"><input type="hidden" name="page" value="1">{{template "carry" .}}` +
	`<label>{{call .T "account"}}<input name="account" autocomplete="username" autocapitalize="none" autocorrect="off" spellcheck="false" required></label>` +
	`<label>{{call .T "password"}}<input name="password" type="password" autocomplete="current-password" required></label>` +
	`<div class="actions"><button>{{call .T "recovery.submit"}}</button></div></form>{{end}}{{template "foot" .}}`)

// recoveryRoute serves both recovery endpoints, as pages for a browser and as
// JSON otherwise: /oauth/account/recovery issues a new code for a person who
// knows their password, /oauth/account/recover uses one.
func (a *Authority) recoveryRoute(recover bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		page := r.Method == http.MethodGet
		if !page {
			r.Body = http.MaxBytesReader(w, r.Body, 8192)
			if err := r.ParseForm(); err != nil {
				oauthError(w, 400, "invalid_request")
				return
			}
			page = r.PostForm.Get("page") == "1"
		}
		title := "recovery.title"
		if recover {
			title = "recover.title"
		}
		view := a.view(r, "recovery", title, a.returnLink(r))
		say := view["T"].(func(string, ...any) string)
		view["Recover"] = recover
		respond := func(status int, code, problem, recovery string) {
			if !page {
				if status == 200 {
					writeJSON(w, 200, map[string]any{"recovery_code": recovery})
					return
				}
				oauthError(w, status, code)
				return
			}
			a.pageHeaders(w)
			if problem != "" {
				view["Problem"] = say(problem)
			}
			view["Code"] = recovery
			w.WriteHeader(status)
			_ = recoveryPage.Execute(w, view)
		}
		if r.Method == http.MethodGet {
			respond(200, "", "", "")
			return
		}
		a.mu.Lock()
		admitted := a.allowed(r, "recovery", 10)
		a.mu.Unlock()
		if !admitted {
			w.Header().Set("Retry-After", "60")
			respond(429, "slow_down", "attempts", "")
			return
		}
		var recovery string
		var err error
		if recover {
			recovery, err = a.Recover(r.Context(), r.PostForm.Get("account"), r.PostForm.Get("code"), r.PostForm.Get("new_password"))
		} else {
			recovery, err = a.IssueRecoveryCode(r.Context(), r.PostForm.Get("account"), r.PostForm.Get("password"))
		}
		switch code := p.ErrorCode(err); {
		case err == nil:
			respond(200, "", "", recovery)
		case code == "invalid_field":
			respond(400, "invalid_request", "short", "")
		case code == "authentication_failed":
			respond(401, "access_denied", "recovery.mismatch", "")
		default:
			w.Header().Set("Retry-After", "5")
			respond(503, "temporarily_unavailable", "failed", "")
		}
	}
}
