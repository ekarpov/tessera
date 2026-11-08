package identity

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	p "github.com/ekarpov/tessera/pkg/protocol"
)

// deleteOwnAccount lets a person delete their own account with their own
// password.
//
// It asks for the password rather than accepting a bearer token: a token is
// held by an installation, often with far less than the person's authority,
// and an installation able to read one dataset must not be able to end the
// account and erase everything it administers. Typing the password is the
// person, now.
//
// The datasets erased are named the same way the administration surface names
// them: exactly those the person is the last administrator of, confirmed in
// erase. They are listed back only to a caller who has already proved the
// password.
func (a *Authority) deleteOwnAccount(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	admitted := a.allowed(r, "account-delete", 10)
	a.mu.Unlock()
	if !admitted {
		w.Header().Set("Retry-After", "60")
		oauthError(w, 429, "slow_down")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	if err := r.ParseForm(); err != nil {
		oauthError(w, 400, "invalid_request")
		return
	}
	// An account joined on a device has no password to type, so its own
	// installation is how the person proves they are deleting it. Only such an
	// account: one with a password is deleted with the password, for the
	// reason above.
	if r.Header.Get("Authorization") != "" {
		access, ok := a.bearerAccess(w, r)
		if !ok {
			return
		}
		a.mu.Lock()
		principal := a.data.Principals[access.Principal]
		a.mu.Unlock()
		if principal.Kind != "person" || principal.Password != "" {
			oauthError(w, 403, "access_denied")
			return
		}
		a.finishDeletion(w, r, principal.ID, principal)
		return
	}
	account := r.PostForm.Get("account")
	a.mu.Lock()
	principal, ok := a.data.Principals[account]
	a.mu.Unlock()
	// Derived with the lock released, as sign-in does: this is a deliberate
	// 600k-iteration derivation and the lock is the one every request takes.
	derived, err := a.derive(r.Context(), r.PostForm.Get("password"), principal.Salt)
	if err != nil {
		w.Header().Set("Retry-After", "5")
		oauthError(w, 503, "temporarily_unavailable")
		return
	}
	valid := equal(derived, principal.Password)
	if !ok || principal.Kind != "person" || !valid || principal.Suspended {
		oauthError(w, 401, "access_denied")
		return
	}
	a.finishDeletion(w, r, account, principal)
}

// finishDeletion deletes an account whose holder has been proved, erasing the
// datasets the request confirms.
func (a *Authority) finishDeletion(w http.ResponseWriter, r *http.Request, account string, principal Principal) {
	erase := append([]string{}, r.PostForm["erase"]...)
	slices.Sort(erase)
	erase = slices.Compact(erase)
	kept, err := a.DeleteAccountProved(r.Context(), account, erase, principal)
	if err == nil {
		// What stayed is named as plainly as what went: an empty erasure
		// otherwise reads the same whether the person held nothing or held
		// records that remain in somebody else's keeping.
		writeJSON(w, 200, map[string]any{"deleted": account, "erased": erase, "kept": kept})
		return
	}
	var refusal *p.Error
	if errors.As(err, &refusal) && refusal.Code == "conflict" {
		switch refusal.Details["reason"] {
		case "erasure_required":
			writeJSON(w, 409, map[string]any{"error": "erasure_required", "datasets": refusal.Details["datasets"]})
			return
		case "last_server_administrator":
			oauthError(w, 409, "last_server_administrator")
			return
		}
	}
	if code := p.ErrorCode(err); code == "not_found" || code == "authentication_failed" {
		// Deleted by a concurrent request after the password was checked.
		oauthError(w, 401, "access_denied")
		return
	}
	oauthError(w, 503, "temporarily_unavailable")
}

// describeAccount tells a client who a bearer token belongs to and what that
// account holds on the server, so a product can say "signed in as" and decide
// what to offer without inferring either from a refusal.
//
// It is deployment information, not the record protocol, which never names an
// account. Any valid token may ask, because it asks only about its own
// account; nothing here names a dataset or another account.
func (a *Authority) describeAccount(w http.ResponseWriter, r *http.Request) {
	fields := strings.Fields(r.Header.Get("Authorization"))
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
		w.Header().Set("WWW-Authenticate", "Bearer")
		oauthError(w, 401, "invalid_token")
		return
	}
	access, err := a.Authenticate(r.Context(), fields[1])
	if err != nil {
		w.Header().Set("WWW-Authenticate", "Bearer")
		oauthError(w, 401, "invalid_token")
		return
	}
	a.mu.Lock()
	principal := a.data.Principals[access.Principal]
	auth := a.data.Authorizations[access.Authorization]
	a.mu.Unlock()
	answer := map[string]any{
		"account": principal.ID,
		"kind":    principal.Kind,
		// What the account holds on the server, whatever this installation
		// was approved for.
		"server_administrator": principal.Kind == "person" && principal.MayAdministerServer,
		"may_create_datasets":  principal.MayCreateDataset,
		// What this installation was approved for.
		"installation": map[string]bool{"administer": auth.Administer, "create_dataset": auth.CreateDataset, "shared_reads": auth.SharedReads},
	}
	// Whoever set a password can sign in with it, so a password somebody else
	// set is said so until the person chooses their own.
	// An account joined on a device has no password, and its issuer can
	// reconnect it to another device until it has one; that is said, with
	// the last time it happened.
	answer["has_password"] = principal.Password != ""
	if principal.Rebound != nil {
		answer["rebound"] = map[string]any{"issuer": principal.Rebound.Issuer, "at": time.Unix(principal.Rebound.At, 0).UTC().Format(time.RFC3339)}
	}
	if principal.PasswordSetBy != "" {
		answer["password_set_by"] = map[string]any{"account": principal.PasswordSetBy, "at": time.Unix(principal.PasswordSetAt, 0).UTC().Format(time.RFC3339)}
	}
	writeJSON(w, 200, answer)
}
