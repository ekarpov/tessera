package identity

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// Invitations: an account created by the person it is for, without an operator
// typing it in.
//
// An operator configures an issuer — a service, such as a signup portal, that
// may hand out invitations — and the person redeems one here, choosing their
// own account name and password. The password never passes through the issuer,
// and what the new account may do is decided when the issuer is configured,
// not when an invitation is issued. So compromising an issuer lets an attacker
// invite people into exactly what the operator allowed that issuer, and no
// further.
//
// Two shapes, because two real deployments need different ones:
//
//   - Datasets: the invitee joins existing datasets with fixed grants. A
//     household inviting a member into its shared dataset.
//   - Template: each redemption creates a new dataset, installs a fixed set of
//     types in the same transaction, makes a named account its administrator
//     and gives the invitee fixed grants in it. A study giving each
//     participant a dataset of their own.
//
// A template issuer must carry a cap. Each redemption creates a dataset, so an
// uncapped template issuer is an unbounded right to consume storage — the way a
// compromised portal would take a server down rather than read from it.

// Issuer is what an operator allows one inviting service to do.
type Issuer struct {
	ID, Secret string
	// Datasets, when set, are the existing datasets an invitee joins.
	Datasets []string `json:",omitempty"`
	// Template, when set, makes each redemption create a dataset.
	Template *Template `json:",omitempty"`
	// Grants are what the invitee holds in each dataset it joins or is given.
	Grants store.Grants
	// Cap bounds how many invitations this issuer may ever hand out. Zero
	// means unbounded, which only a Datasets issuer may be.
	Cap, Issued int
	Disabled    bool
	// AssignNames makes the issuer, not the invitee, choose the account name —
	// named at issuance, or generated when the issuer names none. A study
	// needs this for pseudonymity: a participant choosing their own name can
	// type their real one. The password still never passes through the issuer.
	AssignNames bool `json:",omitempty"`
	// ModerateSharing lets the server's administrator end or narrow what the
	// accounts this issuer makes share from the datasets it makes them —
	// never read them, never delete, and the account is told who stopped
	// what and when. It is the issuer's, set when the operator configures it
	// and not changeable afterwards, so whoever joins through it joins under
	// it and nobody who joined some other way ever is.
	ModerateSharing bool `json:",omitempty"`
	// DeviceRedemption lets an app redeem this issuer's invitations directly
	// for an installation on the device, with no password and no browser.
	// See devices.go. Like moderation it is fixed when the issuer is made.
	DeviceRedemption bool `json:",omitempty"`
}

// Template describes the dataset each redemption creates.
type Template struct {
	// Administrator is the account that administers every dataset created
	// this way — a study's researcher. It must already exist.
	Administrator string
	DisplayName   string
	Types         []p.Meta
	// Readers are given read on every dataset this template creates, in the
	// same step that creates it — a study's analysis service reading every
	// participant, or a household's service reading what each member shares.
	// Only the operator names them, in the issuer's policy; nothing an issuer
	// sends at invitation time adds one.
	Readers []TemplateReader `json:",omitempty"`
	// EraseWithAccount makes each dataset this template creates one that is
	// erased when its person's account is deleted, even where somebody else
	// also administers it — a household's custodian, which nobody signs in
	// as, would otherwise keep a removed member's records for ever.
	EraseWithAccount bool `json:",omitempty"`
}

// TemplateReader is an account given read on each dataset a template
// creates. Types, when present, scopes that read to those types, as a share
// does, and the dataset's administrator can change the scope later; absent,
// it reads the whole dataset. Read is all a reader gets: writing into every
// participant's dataset is not a right one policy line should hand out.
type TemplateReader struct {
	Account string
	Types   []string `json:",omitempty"`
}

// Invitation is an unredeemed invitation.
type Invitation struct {
	Issuer  string
	Expires int64
	// Account is the name the issuer assigned, reserved until redemption.
	Account string `json:",omitempty"`
	// By is the administration installation that issued this invitation,
	// when an administrator issued it rather than the issuer's own secret,
	// and ByAccount its account. Revoking that installation ends the
	// invitation: a lost phone is signed out, and what it handed out stops
	// being redeemable with it.
	By        string `json:",omitempty"`
	ByAccount string `json:",omitempty"`
	// Rebind makes the invitation reconnect Account, an existing account this
	// issuer made by device redemption and which has no password, to a new
	// device. See devices.go.
	Rebind bool `json:",omitempty"`
}

// invitationLifetime is how long an invitation may wait to be redeemed. Long
// enough for somebody to act on an email, short enough that a leaked link does
// not stay live indefinitely.
const invitationLifetime = 7 * 24 * time.Hour

// SetProvisioner installs how a template redemption creates its dataset.
func (a *Authority) SetProvisioner(provision func(ctx context.Context, displayName string, types []p.Meta) (string, error)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.provision = provision
}

// AddIssuer configures an issuer and returns its secret, which is shown once
// and stored only as a digest.
func (a *Authority) AddIssuer(ctx context.Context, issuer Issuer) (string, error) {
	if issuer.ID == "" || len(issuer.ID) > 255 {
		return "", errors.New("an issuer needs an identifier")
	}
	if (len(issuer.Datasets) > 0) == (issuer.Template != nil) {
		return "", errors.New("an issuer invites into named datasets or from a template, exactly one")
	}
	if !(issuer.Grants.Read || issuer.Grants.Write || issuer.Grants.Schema || issuer.Grants.Erase) {
		return "", errors.New("an invitee must be given at least one grant")
	}
	if issuer.Cap < 0 {
		return "", errors.New("a cap cannot be negative")
	}
	if issuer.Template != nil && issuer.Cap == 0 {
		return "", errors.New("a template issuer creates a dataset per redemption and must have a cap")
	}
	// An installation names one dataset, and device redemption makes exactly
	// one installation, so it serves a template or a single dataset.
	if issuer.DeviceRedemption && len(issuer.Datasets) > 1 {
		return "", errors.New("device redemption needs a template or a single dataset")
	}
	held, err := a.engine.Provisioned(ctx)
	if err != nil {
		return "", err
	}
	exists := map[string]bool{}
	for _, d := range held {
		exists[d.ID] = true
	}
	for _, dataset := range issuer.Datasets {
		if !exists[dataset] {
			return "", errors.New("an issuer names a dataset this storage does not hold")
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, taken := a.data.Issuers[issuer.ID]; taken {
		return "", errors.New("issuer already exists")
	}
	if issuer.Template != nil {
		administrator, ok := a.data.Principals[issuer.Template.Administrator]
		if !ok || administrator.Kind != "person" {
			return "", errors.New("a template's administrator must be an existing person")
		}
		if len(issuer.Template.Types) > 128 {
			return "", errors.New("a template installs at most 128 types")
		}
		// Checked now, so a mistake in the policy is the operator's error at
		// configuration rather than every participant's failed sign-up. The
		// engine still decides at creation; this catches what it would refuse
		// for the types' own sake.
		declared := map[string]p.Meta{}
		for _, m := range issuer.Template.Types {
			if err := m.ValidateDeclared(); err != nil {
				return "", errors.New("a template type is not a valid type declaration")
			}
			if err := m.ValidateSemantics(p.DevelopmentLimits()); err != nil {
				return "", errors.New("a template type is not a valid type declaration")
			}
			declared[m.ID] = m
		}
		for id := range declared {
			if _, err := p.EffectiveMetadata(declared, id); err != nil {
				return "", errors.New("a template's types do not form a valid schema")
			}
		}
		if len(issuer.Template.Readers) > 16 {
			return "", errors.New("a template names at most 16 readers")
		}
		for _, reader := range issuer.Template.Readers {
			principal, ok := a.data.Principals[reader.Account]
			if !ok || (principal.Kind != "person" && principal.Kind != "service") || reader.Account == issuer.Template.Administrator {
				return "", errors.New("a template's reader must be an existing account other than its administrator")
			}
			if reader.Types != nil && (len(reader.Types) == 0 || len(reader.Types) > 128) {
				return "", errors.New("a reader's types, when given, name between 1 and 128 types")
			}
			for _, typeID := range reader.Types {
				if p.ValidateIdentifier(p.MetaIdentifier, typeID) != nil {
					return "", errors.New("a reader's types must be type identifiers")
				}
			}
		}
	}
	secret := random()
	issuer.Secret, issuer.Issued, issuer.Disabled = digest(secret), 0, false
	if a.data.Issuers == nil {
		a.data.Issuers = map[string]Issuer{}
	}
	a.data.Issuers[issuer.ID] = issuer
	return secret, a.save()
}

// IssueInvitation hands out one invitation. The code is returned once and only
// its digest is kept, so reading the registry yields nothing redeemable.
func (a *Authority) IssueInvitation(issuerID, secret, account string) (string, string, time.Time, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	issuer, ok := a.data.Issuers[issuerID]
	if !ok || issuer.Disabled || !equal(issuer.Secret, digest(secret)) {
		return "", "", time.Time{}, store.Refuse("authentication_failed")
	}
	return a.issueInvitation(issuerID, issuer, account, Invitation{})
}

// IssueInvitationAs hands out one invitation on an administrator's approved
// installation instead of the issuer's secret, for a deployment whose
// administration tool is an application: a secret that outlives every
// sign-in must not live on a phone.
//
// It takes the standing configuring the issuer takes. The issuer's cap,
// template, names and disabling all apply as they do to its secret. An
// issuer into existing datasets adds people to them, so it also takes
// administering every one, as re-secreting that issuer does; a template
// issuer reaches no data that exists yet.
func (a *Authority) IssueInvitationAs(access store.Access, issuerID, account string) (string, string, time.Time, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	issuer, err := a.issuingAs(access, issuerID)
	if err != nil {
		return "", "", time.Time{}, err
	}
	if issuer.Disabled {
		return "", "", time.Time{}, store.Refuse("conflict")
	}
	return a.issueInvitation(issuerID, issuer, account, Invitation{By: access.Authorization, ByAccount: access.Principal})
}

// issuingAs admits an administration installation to act on an issuer's
// invitations: a server administrator's, approved to administer, who also
// administers every existing dataset the issuer invites into. The caller
// holds the lock.
func (a *Authority) issuingAs(access store.Access, issuerID string) (Issuer, error) {
	auth, ok := a.data.Authorizations[access.Authorization]
	actor, exists := a.data.Principals[access.Principal]
	if !ok || auth.Revoked || !auth.Administer || auth.Principal != access.Principal ||
		!exists || actor.Suspended || actor.Kind != "person" || !actor.MayAdministerServer {
		return Issuer{}, store.Refuse("permission_denied")
	}
	issuer, ok := a.data.Issuers[issuerID]
	if !ok {
		return Issuer{}, store.Refuse("not_found")
	}
	administers := administered(actor)
	for _, dataset := range issuer.Datasets {
		if !administers[dataset] {
			return Issuer{}, store.Refuse("permission_denied")
		}
	}
	return issuer, nil
}

// EndInvitationAs withdraws one invitation nobody has redeemed, leaving the
// issuer's others as they are; disabling the issuer ends all of them. It
// takes the standing issuing from that issuer takes. An unknown, redeemed or
// expired code is not_found, with no word of which.
func (a *Authority) EndInvitationAs(access store.Access, code string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := digest(code)
	invitation, ok := a.data.Invitations[key]
	if !ok || invitation.Expires <= time.Now().Unix() {
		// Refused the same way whether or not the caller could have ended it,
		// but only after checking it may act at all, so the answer tells a
		// caller with no standing nothing about which codes exist.
		if _, err := a.issuingAs(access, invitation.Issuer); p.ErrorCode(err) == "permission_denied" {
			return err
		}
		return store.Refuse("not_found")
	}
	if _, err := a.issuingAs(access, invitation.Issuer); err != nil {
		return err
	}
	delete(a.data.Invitations, key)
	return a.save()
}

// RecordBackup notes that a backup succeeded.
func (a *Authority) RecordBackup(at time.Time) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.data.LastBackupAt = at.Unix()
	return a.save()
}

// LastBackup is when the last backup succeeded, zero when none is known.
func (a *Authority) LastBackup() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.data.LastBackupAt == 0 {
		return time.Time{}
	}
	return time.Unix(a.data.LastBackupAt, 0).UTC()
}

// issueInvitation is issuance after whoever asked has been admitted. The caller holds
// the lock.
func (a *Authority) issueInvitation(issuerID string, issuer Issuer, account string, by Invitation) (string, string, time.Time, error) {
	// A name is the issuer's to give only when it was configured to give them;
	// otherwise the invitee chooses, and a name offered here would be ignored
	// in a way nobody would notice.
	if !issuer.AssignNames && account != "" {
		return "", "", time.Time{}, store.Refuse("invalid_field")
	}
	if issuer.AssignNames {
		if account == "" {
			account = "p-" + random()[:8]
		}
		if len(account) > 255 || !a.nameFree(account) {
			return "", "", time.Time{}, store.Refuse("conflict")
		}
	}
	// Counted at issuance rather than redemption, so the cap bounds what an
	// issuer can put into the world, not only what gets used.
	if issuer.Cap > 0 && issuer.Issued >= issuer.Cap {
		return "", "", time.Time{}, store.Refuse("conflict")
	}
	issuer.Issued++
	a.data.Issuers[issuerID] = issuer
	// An expired invitation can never be redeemed and reserves no name, so it
	// is dropped here rather than kept for ever: an issuer without a cap
	// would otherwise grow the registry with every invitation it handed out.
	now := time.Now().Unix()
	for key, pending := range a.data.Invitations {
		if pending.Expires <= now {
			delete(a.data.Invitations, key)
		}
	}
	code := random()
	expires := time.Now().Add(invitationLifetime)
	if a.data.Invitations == nil {
		a.data.Invitations = map[string]Invitation{}
	}
	a.data.Invitations[digest(code)] = Invitation{Issuer: issuerID, Expires: expires.Unix(), Account: account, By: by.By, ByAccount: by.ByAccount}
	return code, account, expires, a.save()
}

// issuedByLive reports that an invitation an administrator issued still has
// its installation behind it. One issued with the issuer's secret has none to
// lose. The caller holds the lock.
func (a *Authority) issuedByLive(invitation Invitation) bool {
	if invitation.By == "" {
		return true
	}
	auth, ok := a.data.Authorizations[invitation.By]
	return ok && !auth.Revoked
}

// nameFree reports that an account name is neither taken, retired, nor
// reserved by an unredeemed invitation. The caller holds the lock.
func (a *Authority) nameFree(account string) bool {
	if _, taken := a.data.Principals[account]; taken || a.data.Retired[account] {
		return false
	}
	now := time.Now().Unix()
	for _, pending := range a.data.Invitations {
		if pending.Account == account && pending.Expires > now {
			return false
		}
	}
	return true
}

// InvitationAccount is the name an invitation assigns, if any, so the page
// can show it as fixed rather than ask for one.
func (a *Authority) InvitationAccount(code string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	invitation, ok := a.data.Invitations[digest(code)]
	if !ok || invitation.Expires <= time.Now().Unix() || !a.issuedByLive(invitation) {
		return ""
	}
	return invitation.Account
}

// RedeemInvitation creates the account the invitation is for, with the name
// and password the person chose.
//
// It creates only new accounts. Redeeming into an existing one would let
// whoever holds a code add datasets to somebody else's account, and a deleted
// account's name is retired for the reason DeleteAccount gives.
func (a *Authority) RedeemInvitation(ctx context.Context, code, account, secret string) (string, error) {
	if len(secret) < 12 {
		return "", store.Refuse("invalid_field")
	}
	// Derived before the lock, for the reason derive gives. It used to run
	// inside it, so every redemption stalled every bearer-token check for as
	// long as the derivation took.
	salt := random()
	derived, err := a.derive(ctx, secret, salt)
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	key := digest(code)
	invitation, ok := a.data.Invitations[key]
	// A rebinding reconnects an existing account to a device, and only a
	// device redeems it: in a browser it would set a password on somebody's
	// account.
	if !ok || invitation.Expires <= time.Now().Unix() || !a.issuedByLive(invitation) || invitation.Rebind {
		return "", store.Refuse("not_found")
	}
	_, _, recovery, err := a.join(ctx, key, invitation, account, salt, derived)
	return recovery, err
}

// join makes the account an invitation is for, with the dataset or datasets
// it joins, and spends the invitation. salt and derived are its password, or
// empty for an account joined on a device, which has none. It returns the
// joining target separately from the account enriched with reader grants.
// The caller holds the lock and has checked the invitation is live.
func (a *Authority) join(ctx context.Context, key string, invitation Invitation, account, salt, derived string) (Principal, string, string, error) {
	// An assigned name is the name, whatever the form carried: the point of
	// assigning it is that the invitee cannot choose another.
	if invitation.Account != "" {
		account = invitation.Account
	}
	if account == "" || len(account) > 255 {
		return Principal{}, "", "", store.Refuse("invalid_field")
	}
	issuer, ok := a.data.Issuers[invitation.Issuer]
	if !ok || issuer.Disabled {
		return Principal{}, "", "", store.Refuse("not_found")
	}
	// The name is checked before anything is created, so a taken name costs
	// nothing — in particular, not a dataset created for nobody. A chosen name
	// must also not be one another invitation has reserved; an assigned one is
	// reserved by this invitation, so only taken or retired stops it.
	if _, taken := a.data.Principals[account]; taken || a.data.Retired[account] {
		return Principal{}, "", "", store.Refuse("conflict")
	}
	if invitation.Account == "" && !a.nameFree(account) {
		return Principal{}, "", "", store.Refuse("conflict")
	}
	datasets := append([]string(nil), issuer.Datasets...)
	var administrator Principal
	if issuer.Template != nil {
		if administrator, ok = a.data.Principals[issuer.Template.Administrator]; !ok {
			return Principal{}, "", "", store.Refuse("not_found")
		}
		if a.provision == nil {
			return Principal{}, "", "", store.Refuse("not_supported")
		}
		// "{account}" in a template's name becomes the invitee's account name,
		// so a household's datasets are called after their people rather than
		// all "Member".
		name := strings.ReplaceAll(issuer.Template.DisplayName, "{account}", account)
		made, err := a.provision(ctx, name, issuer.Template.Types)
		if err != nil {
			return Principal{}, "", "", err
		}
		if a.data.MadeBy == nil {
			a.data.MadeBy = map[string]string{}
		}
		a.data.MadeBy[made] = invitation.Issuer
		if a.data.Owners == nil {
			a.data.Owners = map[string]string{}
		}
		a.data.Owners[made] = account
		datasets = []string{made}
		held := map[string]store.Grants{}
		for d, g := range administrator.Datasets {
			held[d] = g
		}
		held[made] = store.Grants{Read: true, Write: true, Schema: true, Erase: true}
		administrator.Datasets = held
		a.data.Principals[administrator.ID] = administrator
		// A reader deleted since the policy was written is skipped: its name is
		// retired, so nobody else can come to hold what it would have.
		for _, named := range issuer.Template.Readers {
			reader, exists := a.data.Principals[named.Account]
			if !exists || named.Account == account {
				continue
			}
			readerHeld := map[string]store.Grants{}
			for d, g := range reader.Datasets {
				readerHeld[d] = g
			}
			readerHeld[made] = store.Grants{Read: true}
			reader.Datasets = readerHeld
			if named.Types != nil {
				shared := map[string][]string{}
				for d, types := range reader.SharedTypes {
					shared[d] = types
				}
				shared[made] = append([]string(nil), named.Types...)
				reader.SharedTypes = shared
			}
			a.data.Principals[reader.ID] = reader
			if err := a.syncSharedReads(ctx, reader.ID); err != nil {
				return Principal{}, "", "", err
			}
		}
	}
	grants := map[string]store.Grants{}
	for _, dataset := range datasets {
		grants[dataset] = issuer.Grants
	}
	invitee := Principal{ID: account, Kind: "person", Salt: salt, Password: derived, Datasets: grants, InvitedBy: invitation.Issuer, CreatedAt: time.Now().Unix()}
	// The person's recovery code is made with their account and shown to them
	// once, on the page that made it. An account joined on a device has no
	// password for a code to reset; its issuer rebinds it instead.
	recovery := ""
	if derived != "" {
		recovery, invitee.Recovery = newRecoveryCode()
	} else {
		invitee.DeviceJoined = true
	}
	if issuer.Template != nil && issuer.Template.EraseWithAccount {
		invitee.ErasedWith = datasets
	}
	a.data.Principals[account] = invitee
	delete(a.data.Invitations, key)
	// A member joining reads what was shared with its issuer's group.
	if err := a.applyGroups(ctx, account); err != nil {
		return Principal{}, "", "", err
	}
	return a.data.Principals[account], datasets[0], recovery, a.save()
}

// IssuerSummaries lists issuers for an operator, without their secrets.
// IssuerChange is what may change about an issuer once it is configured:
// how many invitations it may ever hand out, and whether it may hand out
// any. What it hands out — its datasets, template, readers and grants — does
// not change: datasets already created under the old policy would disagree
// with the new one, and an issuer with a different policy is a new issuer.
type IssuerChange struct {
	Cap      *int
	Disabled *bool
}

// ChangeIssuer applies a change to a configured issuer. A cap may be raised
// or lowered, and lowering it below what was already issued stops further
// invitations without touching those already out. A cap set here is at least
// one; an issuer is stopped by disabling it. A disabled issuer issues
// nothing, and invitations it already issued can no longer be redeemed.
func (a *Authority) ChangeIssuer(id string, change IssuerChange) (Issuer, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	issuer, ok := a.data.Issuers[id]
	if !ok {
		return Issuer{}, store.Refuse("not_found")
	}
	if change.Cap != nil {
		// Zero is "unbounded" when an issuer is configured, which is not what
		// anybody setting a cap to zero means. Disabling is how to stop one.
		if *change.Cap < 1 {
			return Issuer{}, store.Refuse("invalid_field")
		}
		issuer.Cap = *change.Cap
	}
	if change.Disabled != nil {
		issuer.Disabled = *change.Disabled
	}
	a.data.Issuers[id] = issuer
	if err := a.save(); err != nil {
		return Issuer{}, err
	}
	issuer.Secret = ""
	return issuer, nil
}

// RotateIssuerSecret replaces an issuer's secret and returns the new one; the
// old one stops working at once. Invitations already issued are unaffected:
// they are codes held by invitees, not uses of the secret.
func (a *Authority) RotateIssuerSecret(id string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	issuer, ok := a.data.Issuers[id]
	if !ok {
		return "", store.Refuse("not_found")
	}
	secret := random()
	issuer.Secret = digest(secret)
	a.data.Issuers[id] = issuer
	return secret, a.save()
}

func (a *Authority) IssuerSummaries() []Issuer {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]Issuer, 0, len(a.data.Issuers))
	for _, issuer := range a.data.Issuers {
		issuer.Secret = ""
		out = append(out, issuer)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
