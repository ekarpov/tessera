package identity

import (
	"context"
	"net/http"
	"strings"
	"time"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// Device sign-in: joining a server from an app on a phone, with no password
// and no browser.
//
// A server on a home network has its own certificate authority, and the
// system sign-in sheet trusts only the system store, so every member signing
// in through it had to install and trust a profile first. An issuer that opts
// in with DeviceRedemption lets the app redeem its invitations directly for
// an installation on the device instead. Nothing the person knows passes
// through the app, because there is nothing to know: the account has no
// password, and is reached from its devices' installations.
//
// From one installation more follow:
//
//   - another device, by a single-use code the installation asks for;
//   - a password, set on this server's own page through a single-use link,
//     for a person who wants to sign in somewhere a browser is used;
//   - after a lost phone, a rebinding: the issuer reconnects an account it
//     made this way, and which has no password, to a new device, signing
//     every other device out. Whoever holds the issuer's right can therefore
//     reach such an account, which the deployment tells the person; setting a
//     password ends it.
//
// And from the host, a single-use code for an existing account's first
// device, carrying the server rights the account holds: how a server's owner
// signs in from the same app without a browser.

// InvitationGrant is the token request that redeems an invitation, a
// rebinding or a device code on a device.
const InvitationGrant = "urn:tessera:params:oauth:grant-type:invitation"

// DeviceCode is a single-use secret that makes one more installation of an
// account: everything about it is decided when it is made.
type DeviceCode struct {
	Account string
	Expires int64
	Dataset string `json:",omitempty"`
	// Grants are the approval the new installation carries on Dataset.
	Grants                                 store.Grants
	SharedReads, Administer, CreateDataset bool `json:",omitempty"`
}

// PasswordLink is a single-use secret that lets an account with no password
// choose one, on this server's page.
type PasswordLink struct {
	Account string
	Expires int64
}

const (
	deviceCodeLifetime   = 10 * time.Minute
	hostCodeLifetime     = 24 * time.Hour
	passwordLinkLifetime = 10 * time.Minute
)

// Joined is what a device learns when it joins: its account, the dataset its
// installation names, if any, and the tokens.
type Joined struct {
	Account string
	Dataset string
	Tokens  map[string]any
}

// RedeemOnDevice redeems code, which is an invitation from an issuer that
// allows it, a rebinding, or a device code, for a new installation of client
// on this device. account is the name the person chose, where the invitation
// assigns none.
func (a *Authority) RedeemOnDevice(ctx context.Context, code, client, account string) (Joined, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if registered, ok := a.data.Clients[client]; !ok || registered.Disabled || registered.Secret != "" {
		return Joined{}, store.Refuse("authentication_failed")
	}
	key := digest(code)
	now := time.Now().Unix()
	var auth Authorization
	var principal Principal
	var err error
	if device, ok := a.data.DeviceCodes[key]; ok {
		delete(a.data.DeviceCodes, key)
		principal, ok = a.data.Principals[device.Account]
		if device.Expires <= now || !ok || principal.Kind != "person" || principal.Suspended {
			return Joined{}, store.Refuse("not_found")
		}
		if auth, err = a.newAuthorization(ctx, principal, client, device.Dataset, device.Grants, device.CreateDataset, device.Administer); err != nil {
			return Joined{}, err
		}
		if device.Administer && (principal.MayAdministerServer || len(administered(principal)) > 0) {
			auth.Administer = true
		}
		auth.SharedReads = device.SharedReads
	} else {
		invitation, ok := a.data.Invitations[key]
		if !ok || invitation.Expires <= now || !a.issuedByLive(invitation) {
			return Joined{}, store.Refuse("not_found")
		}
		issuer, ok := a.data.Issuers[invitation.Issuer]
		if !ok || issuer.Disabled || !issuer.DeviceRedemption {
			return Joined{}, store.Refuse("not_found")
		}
		var dataset string
		if invitation.Rebind {
			// Resolve before spending the invitation or revoking any device.
			// A refused rebinding leaves the member's existing access intact.
			dataset, err = a.deviceDataset(a.data.Principals[invitation.Account], invitation.Issuer, issuer)
			if err == nil {
				principal, err = a.rebind(ctx, key, invitation)
			}
		} else {
			principal, dataset, _, err = a.join(ctx, key, invitation, account, "", "")
		}
		if err != nil {
			return Joined{}, err
		}
		// What the issuer gives, less erase: like an automation token, an
		// installation nobody approved on a screen never carries it.
		requested := issuer.Grants
		requested.Erase = false
		if auth, err = a.newAuthorization(ctx, principal, client, dataset, requested, false, false); err != nil {
			return Joined{}, err
		}
		auth.SharedReads = true
		// A member who administers the dataset made for them shares it, sees
		// what was stopped and reads its usage through administration, which
		// a browser sign-in reaches by ticking it on the approval screen. The
		// person confirmed joining in the app, which the preview showed this
		// to; erase stays withheld, and administration erases nothing.
		auth.Administer = dataset != "" && administered(principal)[dataset]
	}
	a.data.Authorizations[auth.ID] = auth
	if auth.SharedReads {
		if err = a.syncSharedReads(ctx, principal.ID); err != nil {
			return Joined{}, err
		}
	}
	tokens := a.issue(auth, true)
	return Joined{Account: principal.ID, Dataset: auth.Dataset, Tokens: tokens}, a.save()
}

// deviceDataset resolves the issuer's joining target independently of reader
// grants. Template ownership survives restart and dataset erasure/replacement;
// neither display names nor a writable grant establish that identity.
// The caller holds the lock. Ambiguity is refused before rebinding mutates state.
func (a *Authority) deviceDataset(principal Principal, issuerID string, issuer Issuer) (string, error) {
	dataset := ""
	if issuer.Template == nil {
		if len(issuer.Datasets) != 1 {
			return "", store.Refuse("not_found")
		}
		dataset = issuer.Datasets[0]
	} else {
		for owned, owner := range a.data.Owners {
			if owner != principal.ID || a.data.MadeBy[owned] != issuerID {
				continue
			}
			if dataset != "" || owned == "" {
				return "", store.Refuse("not_found")
			}
			dataset = owned
		}
	}
	held, ok := principal.Datasets[dataset]
	if dataset == "" || !ok || !held.Current() {
		return "", store.Refuse("not_found")
	}
	return dataset, nil
}

// rebind reconnects an existing account to a new device: every installation
// it has is revoked, the rebinding is recorded on it, and the invitation is
// spent. The caller holds the lock and has checked the issuer allows device
// redemption.
func (a *Authority) rebind(ctx context.Context, key string, invitation Invitation) (Principal, error) {
	principal, ok := a.data.Principals[invitation.Account]
	if !ok || !rebindable(principal, invitation.Issuer) {
		return Principal{}, store.Refuse("not_found")
	}
	if err := a.revokeAll(ctx, principal.ID); err != nil {
		return Principal{}, err
	}
	principal.Rebound = &Rebinding{At: time.Now().Unix(), Issuer: invitation.Issuer}
	a.data.Principals[principal.ID] = principal
	delete(a.data.Invitations, key)
	return principal, nil
}

// revokeAll removes every installation an account has, leaving no record of
// them, and ends any sign-in it has pending. The caller holds the lock.
func (a *Authority) revokeAll(ctx context.Context, account string) error {
	for id, auth := range a.data.Authorizations {
		if auth.Principal != account {
			continue
		}
		if err := a.removeInstallation(ctx, id); err != nil {
			return err
		}
	}
	a.endPendingSignIns(account)
	return nil
}

// rebindable reports that issuer may reconnect this account to a new device:
// it made the account on a device, and nobody has set a password on it since.
func rebindable(principal Principal, issuer string) bool {
	return principal.Kind == "person" && !principal.Suspended && principal.DeviceJoined &&
		principal.InvitedBy == issuer && principal.Password == ""
}

// RebindableAs reports whether this installation may issue a rebinding for
// account now. It shares admission and account eligibility with issuance;
// callers must still use IssueRebindingAs to act on a later state.
func (a *Authority) RebindableAs(access store.Access, account string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	principal, ok := a.data.Principals[account]
	if !ok {
		return false
	}
	issuer, err := a.issuingAs(access, principal.InvitedBy)
	if err != nil {
		return false
	}
	_, err = a.rebindingAccount(principal.InvitedBy, issuer, account)
	return err == nil
}

// rebindingAccount checks issuer policy and account eligibility. The caller
// holds the lock; both overview advice and invitation issuance use this rule.
func (a *Authority) rebindingAccount(issuerID string, issuer Issuer, account string) (Principal, error) {
	if issuer.Disabled {
		return Principal{}, store.Refuse("conflict")
	}
	if !issuer.DeviceRedemption {
		return Principal{}, store.Refuse("invalid_field")
	}
	principal, ok := a.data.Principals[account]
	if !ok || !rebindable(principal, issuerID) {
		return Principal{}, store.Refuse("not_found")
	}
	return principal, nil
}

// IssueRebinding hands out an invitation that reconnects account to a new
// device, on the issuer's secret.
func (a *Authority) IssueRebinding(issuerID, secret, account string) (string, time.Time, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	issuer, ok := a.data.Issuers[issuerID]
	if !ok || issuer.Disabled || !equal(issuer.Secret, digest(secret)) {
		return "", time.Time{}, store.Refuse("authentication_failed")
	}
	return a.issueRebinding(issuerID, issuer, account, Invitation{})
}

// IssueRebindingAs is IssueRebinding on an administrator's installation.
func (a *Authority) IssueRebindingAs(access store.Access, issuerID, account string) (string, time.Time, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	issuer, err := a.issuingAs(access, issuerID)
	if err != nil {
		return "", time.Time{}, err
	}
	if issuer.Disabled {
		return "", time.Time{}, store.Refuse("conflict")
	}
	return a.issueRebinding(issuerID, issuer, account, Invitation{By: access.Authorization, ByAccount: access.Principal})
}

// issueRebinding is issuance of a rebinding once the caller is admitted. It is
// not counted against the issuer's cap: it makes no account. The caller holds
// the lock.
func (a *Authority) issueRebinding(issuerID string, issuer Issuer, account string, by Invitation) (string, time.Time, error) {
	if _, err := a.rebindingAccount(issuerID, issuer, account); err != nil {
		return "", time.Time{}, err
	}
	code := random()
	expires := time.Now().Add(invitationLifetime)
	if a.data.Invitations == nil {
		a.data.Invitations = map[string]Invitation{}
	}
	a.data.Invitations[digest(code)] = Invitation{Issuer: issuerID, Expires: expires.Unix(), Account: account, Rebind: true, By: by.By, ByAccount: by.ByAccount}
	return code, expires, a.save()
}

// DeviceCodeFor makes a single-use code for one more device of the account
// behind access, with the same approval as access's installation.
func (a *Authority) DeviceCodeFor(access store.Access) (string, time.Time, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	auth, ok := a.data.Authorizations[access.Authorization]
	if !ok || auth.Console != nil || auth.Revoked || auth.Principal != access.Principal || !a.active(auth.Principal) || a.data.Principals[auth.Principal].Kind != "person" {
		return "", time.Time{}, store.Refuse("permission_denied")
	}
	approved := store.Grants{}
	if auth.Approved != nil {
		approved = *auth.Approved
	}
	return a.addDeviceCode(DeviceCode{Account: auth.Principal, Dataset: auth.Dataset, Grants: approved,
		SharedReads: auth.SharedReads, Administer: auth.Administer, CreateDataset: auth.CreateDataset}, deviceCodeLifetime)
}

// AddHostDeviceCode makes a single-use code for an existing person's first
// device, from the host: an installation carrying the server rights the
// account holds and, when dataset names one it holds, its grants there less
// erase. It is how a server's owner signs in from an app without a browser.
// Nothing on the network can make one.
//
// With revoke, every installation the account has is revoked first, in the
// same step: a lost device stops working as its replacement's code is made.
func (a *Authority) AddHostDeviceCode(ctx context.Context, account, dataset string, revoke bool) (string, time.Time, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	principal, ok := a.data.Principals[account]
	if !ok || principal.Kind != "person" || principal.Suspended {
		return "", time.Time{}, store.Refuse("not_found")
	}
	if revoke {
		if err := a.revokeAll(ctx, account); err != nil {
			return "", time.Time{}, err
		}
	}
	code := DeviceCode{Account: account, Dataset: dataset, Administer: principal.MayAdministerServer || len(administered(principal)) > 0,
		CreateDataset: principal.MayCreateDataset}
	if dataset != "" {
		held, ok := principal.Datasets[dataset]
		if !ok {
			return "", time.Time{}, store.Refuse("not_found")
		}
		code.Grants = held
		code.Grants.Erase, code.Grants.ExpiresAt = false, nil
	} else if !principal.MayAdministerServer && !principal.MayCreateDataset {
		// An installation naming no dataset carries only server rights, and
		// administering a dataset is not one: name the dataset instead.
		return "", time.Time{}, store.Refuse("invalid_field")
	}
	return a.addDeviceCode(code, hostCodeLifetime)
}

func (a *Authority) addDeviceCode(code DeviceCode, lifetime time.Duration) (string, time.Time, error) {
	now := time.Now()
	for key, pending := range a.data.DeviceCodes {
		if pending.Expires <= now.Unix() {
			delete(a.data.DeviceCodes, key)
		}
	}
	if a.data.DeviceCodes == nil {
		a.data.DeviceCodes = map[string]DeviceCode{}
	}
	if len(a.data.DeviceCodes) >= 1000 {
		return "", time.Time{}, store.Refuse("rate_limited")
	}
	secret := random()
	expires := now.Add(lifetime)
	code.Expires = expires.Unix()
	a.data.DeviceCodes[digest(secret)] = code
	return secret, expires, a.save()
}

// PasswordLinkFor makes a single-use link for an account with no password to
// choose one, on this server's page. Only such an account may: an
// installation of an account with a password must not be able to replace it.
func (a *Authority) PasswordLinkFor(access store.Access) (string, time.Time, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	auth, ok := a.data.Authorizations[access.Authorization]
	principal := a.data.Principals[access.Principal]
	if !ok || auth.Revoked || auth.Principal != access.Principal || !a.active(principal.ID) || principal.Kind != "person" || principal.Password != "" {
		return "", time.Time{}, store.Refuse("permission_denied")
	}
	now := time.Now()
	for key, pending := range a.data.PasswordLinks {
		if pending.Expires <= now.Unix() {
			delete(a.data.PasswordLinks, key)
		}
	}
	if a.data.PasswordLinks == nil {
		a.data.PasswordLinks = map[string]PasswordLink{}
	}
	secret := random()
	expires := now.Add(passwordLinkLifetime)
	a.data.PasswordLinks[digest(secret)] = PasswordLink{Account: principal.ID, Expires: expires.Unix()}
	return secret, expires, a.save()
}

// PasswordLinkAccount names the account a live link is for, for the page.
func (a *Authority) PasswordLinkAccount(link string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	pending, ok := a.data.PasswordLinks[digest(link)]
	if !ok || pending.Expires <= time.Now().Unix() {
		return ""
	}
	return pending.Account
}

// SetPasswordByLink spends a link and sets the account's first password. The
// account's devices stay signed in: the person is choosing a password, not
// recovering from a lost one.
func (a *Authority) SetPasswordByLink(ctx context.Context, link, secret string) (string, error) {
	if len(secret) < 12 {
		return "", store.Refuse("invalid_field")
	}
	salt := random()
	derived, err := a.derive(ctx, secret, salt)
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	key := digest(link)
	pending, ok := a.data.PasswordLinks[key]
	delete(a.data.PasswordLinks, key)
	principal, exists := a.data.Principals[pending.Account]
	if !ok || pending.Expires <= time.Now().Unix() || !exists || principal.Password != "" || principal.Suspended {
		return "", store.Refuse("authentication_failed")
	}
	principal.Salt, principal.Password = salt, derived
	recovery, kept := newRecoveryCode()
	principal.Recovery = kept
	a.data.Principals[principal.ID] = principal
	return recovery, a.save()
}

// InvitationPreview is what an app shows before the person joins: where, as
// whom, and what the app will be able to do.
type InvitationPreview struct {
	Account string `json:"account,omitempty"`
	Rebind  bool   `json:"rebind,omitempty"`
	// Administer says the installation will administer the dataset joining
	// gives: share it, see what was stopped, read its usage.
	Administer  bool         `json:"administer,omitempty"`
	DisplayName string       `json:"display_name,omitempty"`
	Access      store.Grants `json:"access"`
}

// PreviewInvitation describes a live invitation an app may redeem on a device.
func (a *Authority) PreviewInvitation(code string) (InvitationPreview, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	invitation, ok := a.data.Invitations[digest(code)]
	if !ok || invitation.Expires <= time.Now().Unix() || !a.issuedByLive(invitation) {
		return InvitationPreview{}, store.Refuse("not_found")
	}
	issuer, ok := a.data.Issuers[invitation.Issuer]
	if !ok || issuer.Disabled || !issuer.DeviceRedemption {
		return InvitationPreview{}, store.Refuse("not_found")
	}
	access := issuer.Grants
	access.Erase = false
	// Holding erase on the dataset joining gives is what administers it.
	preview := InvitationPreview{Account: invitation.Account, Rebind: invitation.Rebind, Access: access, Administer: issuer.Grants.Erase}
	if named := a.displayName.Load(); named != nil {
		preview.DisplayName = *named
	}
	return preview, nil
}

// deviceToken answers the invitation grant at the token endpoint. Its own
// rate limit is the redeem page's, because a code is what it guesses.
func (a *Authority) deviceToken(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	admitted := a.allowed(r, "redeem", 20)
	a.mu.Unlock()
	if !admitted {
		w.Header().Set("Retry-After", "60")
		oauthError(w, 429, "temporarily_unavailable")
		return
	}
	joined, err := a.RedeemOnDevice(r.Context(), r.PostForm.Get("code"), r.PostForm.Get("client_id"), r.PostForm.Get("account"))
	switch code := p.ErrorCode(err); {
	case err == nil:
	case code == "authentication_failed":
		oauthError(w, 401, "invalid_client")
		return
	case code == "not_found", code == "permission_denied":
		oauthError(w, 400, "invalid_grant")
		return
	case code == "invalid_field":
		oauthError(w, 400, "invalid_request")
		return
	case code == "conflict":
		oauthError(w, 409, "account_name_unavailable")
		return
	default:
		oauthError(w, 503, "temporarily_unavailable")
		return
	}
	answer := joined.Tokens
	answer["account"] = joined.Account
	if joined.Dataset != "" {
		answer["dataset_id"] = joined.Dataset
	}
	writeJSON(w, 200, answer)
}

// bearerAccess authenticates a request's bearer token, answering it when it
// cannot.
func (a *Authority) bearerAccess(w http.ResponseWriter, r *http.Request) (store.Access, bool) {
	fields := strings.Fields(r.Header.Get("Authorization"))
	if len(fields) == 2 && strings.EqualFold(fields[0], "Bearer") {
		if access, err := a.Authenticate(r.Context(), fields[1]); err == nil {
			return access, true
		}
	}
	w.Header().Set("WWW-Authenticate", "Bearer")
	oauthError(w, 401, "invalid_token")
	return store.Access{}, false
}

// deviceCodes answers POST /oauth/device-codes: a code for one more device.
func (a *Authority) deviceCodes(w http.ResponseWriter, r *http.Request) {
	access, ok := a.bearerAccess(w, r)
	if !ok {
		return
	}
	a.mu.Lock()
	admitted := a.allowed(r, "device-code", 10)
	a.mu.Unlock()
	if !admitted {
		w.Header().Set("Retry-After", "60")
		oauthError(w, 429, "slow_down")
		return
	}
	code, expires, err := a.DeviceCodeFor(access)
	if err != nil {
		if p.ErrorCode(err) == "permission_denied" {
			oauthError(w, 403, "access_denied")
			return
		}
		oauthError(w, 503, "temporarily_unavailable")
		return
	}
	writeJSON(w, 201, map[string]any{"code": code, "expires_at": expires.UTC().Format(time.RFC3339)})
}

// passwordLinks answers POST /oauth/account/password-link.
func (a *Authority) passwordLinks(w http.ResponseWriter, r *http.Request) {
	access, ok := a.bearerAccess(w, r)
	if !ok {
		return
	}
	link, expires, err := a.PasswordLinkFor(access)
	if err != nil {
		if p.ErrorCode(err) == "permission_denied" {
			oauthError(w, 403, "access_denied")
			return
		}
		oauthError(w, 503, "temporarily_unavailable")
		return
	}
	writeJSON(w, 201, map[string]any{"url": a.issuer + "/oauth/account/password?link=" + link, "expires_at": expires.UTC().Format(time.RFC3339)})
}

// invitationPreview answers GET /oauth/invitation/preview?code=.
func (a *Authority) invitationPreview(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	admitted := a.allowed(r, "redeem", 20)
	a.mu.Unlock()
	if !admitted {
		w.Header().Set("Retry-After", "60")
		oauthError(w, 429, "temporarily_unavailable")
		return
	}
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	preview, err := a.PreviewInvitation(r.URL.Query().Get("code"))
	if err != nil {
		oauthError(w, 404, "invalid_grant")
		return
	}
	writeJSON(w, 200, preview)
}
