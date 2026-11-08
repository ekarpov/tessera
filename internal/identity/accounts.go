package identity

import (
	"context"
	"slices"
	"sort"
	"time"

	"github.com/ekarpov/tessera/pkg/store"
)

// Account lifecycle: suspending, reinstating and deleting an account.
//
// These belong to the deployment rather than the protocol. The protocol never
// names an account; it answers requests an authorization makes. What an
// account is, and how one stops, is how this deployment provisions access.

// SuspendAccount stops every credential the account holds from
// authenticating, at once, and keeps all of them. Reinstating restores them
// exactly. Revoking installations one at a time is the permanent alternative.
func (a *Authority) SuspendAccount(id string) error { return a.setSuspended(id, true) }

// ReinstateAccount reverses SuspendAccount.
func (a *Authority) ReinstateAccount(id string) error { return a.setSuspended(id, false) }

func (a *Authority) setSuspended(id string, suspended bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	principal, ok := a.data.Principals[id]
	if !ok {
		return store.Refuse("not_found")
	}
	// Reported as a conflict rather than succeeding silently, so that what an
	// administrator did and what the page says happened are the same thing.
	if principal.Suspended == suspended {
		return store.Refuse("conflict")
	}
	principal.Suspended = suspended
	principal.SuspendedAt = 0
	if suspended {
		principal.SuspendedAt = time.Now().Unix()
	}
	a.data.Principals[id] = principal
	// A sign-in in progress for this account must not complete into an
	// approval after the account was suspended under it.
	if suspended {
		for flowID, f := range a.flows {
			if f.Principal == id {
				delete(a.flows, flowID)
			}
		}
	}
	return a.save()
}

// DeleteAccount removes an account and everything it could authenticate
// with, and retires its name.
//
// A dataset somebody else also administers is not touched: an account leaving
// is not a statement about data other people hold. A dataset this account is
// the last administrator of is different. Left behind, it would be data
// nobody could administer or erase, so deleting the account erases it — but
// only when the caller names exactly those datasets in erase. Anything else
// is refused with conflict, reason erasure_required, and the datasets listed,
// so a deletion never erases what its caller did not see and confirm.
//
// The last server administrator is not deleted either: nothing could then
// administer the server through it.
//
// The name is retired rather than freed. A receipt answers to its owning
// principal, so an account created later under the same name could ask about,
// and read, the outcomes this one left in datasets that still exist.
// Kept is a dataset the account held something in that its deletion left in
// place, and why: another_administrator when it administered the dataset
// alongside somebody else, member when it held access to somebody else's.
// Either way records it wrote there remain, and a person deleting their
// account should be told so rather than read an empty erasure as "nothing
// of mine is left".
type Kept struct {
	Dataset string `json:"dataset"`
	Reason  string `json:"reason"`
}

// DeleteAccount deletes the account as described above and reports the
// datasets it held that remain.
func (a *Authority) DeleteAccount(ctx context.Context, id string, erase []string) ([]Kept, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.deleteAccount(ctx, id, erase)
}

// DeleteAccountProved is DeleteAccount for a person who proved their
// password as seen, checked again under the same lock as the deletion: a
// password changed or an account suspended since it was proved stops it.
func (a *Authority) DeleteAccountProved(ctx context.Context, id string, erase []string, seen Principal) ([]Kept, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.unchanged(id, seen) {
		return nil, store.Refuse("authentication_failed")
	}
	return a.deleteAccount(ctx, id, erase)
}

// unchanged reports, under a.mu, whether an account still has the password it
// was proved against, and may still act.
//
// Every password check copies the account, releases the lock for the slow
// derivation, and compares against the copy. Without this, a recovery or a
// password change that landed during the derivation left the old password
// working for that one request — for a sign-in, long enough to obtain tokens
// after the recovery meant to shut its holder out.
func (a *Authority) unchanged(id string, seen Principal) bool {
	current, ok := a.data.Principals[id]
	return ok && current.Kind == "person" && !current.Suspended && current.Salt == seen.Salt && current.Password == seen.Password
}

func (a *Authority) deleteAccount(ctx context.Context, id string, erase []string) ([]Kept, error) {
	principal, ok := a.data.Principals[id]
	if !ok {
		return nil, store.Refuse("not_found")
	}
	if principal.MayAdministerServer && !a.anotherServerAdministrator(id) {
		return nil, store.RefuseWith("conflict", "last_server_administrator", nil)
	}
	sole := a.deletionErases(id)
	confirmed := append([]string{}, erase...)
	sort.Strings(confirmed)
	if !slices.Equal(sole, slices.Compact(confirmed)) {
		return nil, store.RefuseWith("conflict", "erasure_required", map[string]any{"datasets": sole})
	}
	if len(sole) > 0 && a.erase == nil {
		return nil, store.Refuse("conflict")
	}
	// Each erasure is recorded as it completes. One that fails part way leaves
	// the account in place holding only what is still to erase, so asking
	// again lists exactly that rather than a dataset that no longer exists.
	for _, dataset := range sole {
		if err := a.erase(ctx, dataset); err != nil {
			return nil, err
		}
		for _, other := range a.data.Principals {
			delete(other.Datasets, dataset)
		}
		for authID, auth := range a.data.Authorizations {
			if auth.Dataset == dataset && !auth.Revoked {
				auth.Revoked = true
				a.data.Authorizations[authID] = auth
			}
		}
		if err := a.save(); err != nil {
			return nil, err
		}
	}
	ids := make([]string, 0)
	for authID, auth := range a.data.Authorizations {
		if auth.Principal == id {
			ids = append(ids, authID)
		}
	}
	sort.Strings(ids)
	for _, authID := range ids {
		auth := a.data.Authorizations[authID]
		if !auth.Revoked {
			if err := a.revokeEverywhere(ctx, auth); err != nil {
				return nil, err
			}
		}
		delete(a.data.Authorizations, authID)
	}
	gone := map[string]bool{}
	for _, authID := range ids {
		gone[authID] = true
	}
	for key, token := range a.data.Tokens {
		if gone[token.Authorization] {
			delete(a.data.Tokens, key)
		}
	}
	for key, code := range a.data.Codes {
		if gone[code.Authorization] {
			delete(a.data.Codes, key)
		}
	}
	// A service's client credential belongs to the account and goes with it.
	for clientID, client := range a.data.Clients {
		if client.Principal == id {
			delete(a.data.Clients, clientID)
		}
	}
	for flowID, f := range a.flows {
		if f.Principal == id {
			delete(a.flows, flowID)
		}
	}
	kept := []Kept{}
	for dataset, held := range a.data.Principals[id].Datasets {
		if dataset == "" || !(held.Read || held.Write || held.Schema || held.Erase) {
			continue
		}
		reason := "member"
		if held.Erase && held.Current() {
			reason = "another_administrator"
		}
		kept = append(kept, Kept{Dataset: dataset, Reason: reason})
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].Dataset < kept[j].Dataset })
	delete(a.data.Principals, id)
	if a.data.Retired == nil {
		a.data.Retired = map[string]bool{}
	}
	a.data.Retired[id] = true
	return kept, a.save()
}

// SetEraser installs how a dataset is erased when the last account
// administering it is deleted. Set by the composition, because datasets
// belong to the storage rather than to this registry.
func (a *Authority) SetEraser(erase func(ctx context.Context, dataset string) error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.erase = erase
}

// DeletionErases names the datasets deleting this account would erase: those
// it is the last administrator of. Deleting it must name exactly these.
func (a *Authority) DeletionErases(id string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.deletionErases(id)
}

// deletionErases is what deleting an account erases: the datasets it alone
// administers, and those a template made to be erased with it.
func (a *Authority) deletionErases(id string) []string {
	erases := a.soleAdministered(id)
	principal := a.data.Principals[id]
	for _, dataset := range principal.ErasedWith {
		if _, held := principal.Datasets[dataset]; held && !slices.Contains(erases, dataset) {
			erases = append(erases, dataset)
		}
	}
	sort.Strings(erases)
	return erases
}

func (a *Authority) soleAdministered(id string) []string {
	sole := []string{}
	for dataset := range administered(a.data.Principals[id]) {
		if !a.anotherAdministrator(dataset, id) {
			sole = append(sole, dataset)
		}
	}
	sort.Strings(sole)
	return sole
}

func (a *Authority) anotherServerAdministrator(except string) bool {
	for id, other := range a.data.Principals {
		if id != except && other.Kind == "person" && !other.Suspended && other.MayAdministerServer {
			return true
		}
	}
	return false
}

// anotherAdministrator counts a suspended administrator too. Whether a
// dataset is erased turns on it, and a suspension is reversible: reinstating
// that person must find their dataset still there.
func (a *Authority) anotherAdministrator(dataset, except string) bool {
	for id, other := range a.data.Principals {
		if id == except || other.Kind != "person" {
			continue
		}
		if held, ok := other.Datasets[dataset]; ok && held.Erase && held.Current() {
			return true
		}
	}
	return false
}

// MayAdministerAccount reports whether an administrator may suspend, reinstate
// or delete an account from the administration surface.
//
// A server administrator may act on any account but their own. Otherwise an
// administrator may act on an account only when every dataset it holds is one
// they administer — administering one dataset a person belongs to is not
// standing to remove them from others. Nobody acts on their own account here,
// because an administrator who suspends or deletes themselves has no way back
// through this surface.
func (a *Authority) MayAdministerAccount(administrator, id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if administrator == id {
		return false
	}
	actor, ok := a.data.Principals[administrator]
	if !ok || actor.Suspended || actor.Kind != "person" {
		return false
	}
	target, ok := a.data.Principals[id]
	if !ok {
		return false
	}
	if actor.MayAdministerServer {
		return true
	}
	administers := administered(actor)
	held := 0
	for dataset, g := range target.Datasets {
		if dataset == "" || !(g.Read || g.Write || g.Schema || g.Erase) {
			continue
		}
		if !administers[dataset] {
			return false
		}
		held++
	}
	return held > 0
}

// ResetPassword sets a new password for a person, for the day somebody forgets
// theirs.
//
// Existing sign-ins keep working. A forgotten password is not a compromised
// one, and signing a household member out of every device because they could
// not remember a password punishes the wrong thing. Where a compromise is
// suspected, suspension or revocation is the tool, and it is a separate one on
// purpose.
func (a *Authority) ResetPassword(id, secret, by string) error {
	if len(secret) < 12 {
		return store.Refuse("invalid_field")
	}
	// Derived before the lock is taken. It is a deliberate 600,000-iteration
	// derivation and the lock is the one every API request takes to check its
	// bearer token; holding it here would stall unrelated traffic, which this
	// file has already had to fix twice elsewhere.
	salt := random()
	derived, err := a.derive(context.Background(), secret, salt)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	principal, ok := a.data.Principals[id]
	if !ok || principal.Kind != "person" {
		return store.Refuse("not_found")
	}
	principal.Salt, principal.Password = salt, derived
	principal.PasswordSetBy, principal.PasswordSetAt = by, time.Now().Unix()
	a.data.Principals[id] = principal
	a.endPendingSignIns(id)
	return a.save()
}

// MayResetPassword reports whether an administrator may set another person's
// password.
//
// Setting a password is signing in as that person, so it is data access, not
// account management: whoever sets it can read everything the account can.
// The standing is therefore administering every dataset the account holds —
// the same rule a dataset administrator meets for suspending and deleting —
// and a server administrator meets it too. Suspending and deleting reach no
// data, so a server administrator may do those to anybody; setting a password
// on an account whose datasets they do not administer would be reading them
// by the back door. An account holding no dataset has nothing to read, and
// its password is the server administrator's to set. Never one's own here.
func (a *Authority) MayResetPassword(administrator, id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if administrator == id {
		return false
	}
	actor, ok := a.data.Principals[administrator]
	if !ok || actor.Suspended || actor.Kind != "person" {
		return false
	}
	target, ok := a.data.Principals[id]
	if !ok || target.Kind != "person" {
		return false
	}
	administers := administered(actor)
	held := 0
	for dataset, g := range target.Datasets {
		if dataset == "" || !(g.Read || g.Write || g.Schema || g.Erase) {
			continue
		}
		if !administers[dataset] {
			return false
		}
		held++
	}
	return held > 0 || actor.MayAdministerServer
}

// ChangeOwnPassword sets a new password for a person who proves the current
// one. Every installation the account holds is revoked: a person changing
// their password because somebody else knew it — an administrator who set
// it, or whoever took it — must not leave that somebody signed in.
func (a *Authority) ChangeOwnPassword(ctx context.Context, id, current, next string) error {
	if len(next) < 12 {
		return store.Refuse("invalid_field")
	}
	a.mu.Lock()
	principal, ok := a.data.Principals[id]
	a.mu.Unlock()
	proved, err := a.derive(ctx, current, principal.Salt)
	if err != nil {
		return err
	}
	if !ok || principal.Kind != "person" || principal.Suspended || !equal(proved, principal.Password) {
		return store.Refuse("authentication_failed")
	}
	salt := random()
	derived, err := a.derive(ctx, next, salt)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.unchanged(id, principal) {
		return store.Refuse("authentication_failed")
	}
	principal = a.data.Principals[id]
	for authID, auth := range a.data.Authorizations {
		if auth.Principal != id || auth.Revoked {
			continue
		}
		if err = a.revokeEverywhere(ctx, auth); err != nil {
			return err
		}
		auth.Revoked = true
		a.data.Authorizations[authID] = auth
	}
	principal.Salt, principal.Password = salt, derived
	principal.PasswordSetBy, principal.PasswordSetAt = "", 0
	a.data.Principals[id] = principal
	a.endPendingSignIns(id)
	return a.save()
}

// AuthenticateAdministration answers who is acting through the administration
// API: the account behind a bearer token whose installation was approved to
// administer. Everything else about the token is checked as Authenticate
// checks it, so a suspended account or a revoked installation is refused here
// too.
func (a *Authority) AuthenticateAdministration(ctx context.Context, bearer string) (string, error) {
	access, err := a.AdministrationAccess(ctx, bearer)
	return access.Principal, err
}

// AdministrationAccess is AuthenticateAdministration answering the
// installation as well as the account, for an action that must stay tied to
// the installation that took it.
func (a *Authority) AdministrationAccess(ctx context.Context, bearer string) (store.Access, error) {
	access, err := a.Authenticate(ctx, bearer)
	if err != nil {
		return store.Access{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.data.Authorizations[access.Authorization].Administer {
		return store.Access{}, store.Refuse("permission_denied")
	}
	return access, nil
}

// RecordErasure brings the registry up to date with an erasure made through
// the protocol, which the storage performed without it.
//
// An erasure gives the dataset a fresh identity and provisions only the
// authorization that erased it. The registry went on naming the old identity:
// the person who erased no longer saw their dataset under their account, and
// every administrator of it was shown an identity the storage no longer has.
// Now the person who erased holds the replacement with the grants they held
// before; everybody else's hold ends, as the storage's did; the erasing
// installation follows the replacement; and issuers and template bindings
// naming the old identity name the new one.
func (a *Authority) RecordErasure(access store.Access, old, replacement string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, principal := range a.data.Principals {
		held, ok := principal.Datasets[old]
		bound := slices.Contains(principal.ErasedWith, old)
		if !ok && !bound {
			continue
		}
		datasets := map[string]store.Grants{}
		for d, g := range principal.Datasets {
			if d != old {
				datasets[d] = g
			}
		}
		if ok && id == access.Principal {
			datasets[replacement] = held
		}
		principal.Datasets = datasets
		principal.FrozenReaders = withFreeze(principal.FrozenReaders, old, ReaderFreeze{})
		principal.GroupFreezes = withFreeze(principal.GroupFreezes, old, ReaderFreeze{})
		if types, shared := principal.SharedTypes[old]; shared {
			scoped := map[string][]string{}
			for d, t := range principal.SharedTypes {
				if d != old {
					scoped[d] = t
				}
			}
			if id == access.Principal {
				scoped[replacement] = types
			}
			principal.SharedTypes = scoped
		}
		// Nobody contributes to a replacement: the eraser holds it whole,
		// and every other standing on the old identity ended with it.
		if _, contributed := principal.InsertTypes[old]; contributed {
			inserts := map[string][]string{}
			for d, t := range principal.InsertTypes {
				if d != old {
					inserts[d] = t
				}
			}
			principal.InsertTypes = inserts
		}
		if bound {
			principal.ErasedWith = slices.Clone(principal.ErasedWith)
			for i, d := range principal.ErasedWith {
				if d == old {
					principal.ErasedWith[i] = replacement
				}
			}
		}
		a.data.Principals[id] = principal
	}
	// Group grants on the erased dataset end with everyone else's hold on it.
	delete(a.data.Groups, old)
	delete(a.data.FrozenGroups, old)
	delete(a.data.Stopped, old)
	// The replacement is the same person's dataset, made by the same issuer,
	// and stays under that issuer's policy.
	if owner, recorded := a.data.Owners[old]; recorded {
		delete(a.data.Owners, old)
		a.data.Owners[replacement] = owner
	}
	if issuer, made := a.data.MadeBy[old]; made {
		delete(a.data.MadeBy, old)
		a.data.MadeBy[replacement] = issuer
	}
	for id, principal := range a.data.Principals {
		if _, grouped := principal.ViaGroup[old]; grouped {
			delete(principal.ViaGroup, old)
			a.data.Principals[id] = principal
		}
	}
	for authID, auth := range a.data.Authorizations {
		if auth.Dataset != old {
			continue
		}
		if authID == access.Authorization {
			auth.Dataset = replacement
		} else {
			auth.Revoked = true
		}
		a.data.Authorizations[authID] = auth
	}
	for issuerID, issuer := range a.data.Issuers {
		if i := slices.Index(issuer.Datasets, old); i >= 0 {
			issuer.Datasets = slices.Clone(issuer.Datasets)
			issuer.Datasets[i] = replacement
			a.data.Issuers[issuerID] = issuer
		}
	}
	return a.save()
}

// MayRotateServiceSecret reports whether an administrator may replace a
// service's secret. Whoever holds the new secret signs in as the service and
// reads what it reads, so this is data access, as setting a person's password
// is: it takes being the server's administrator and administering every
// dataset the service holds. A service holding no dataset has nothing to
// read. The command line, run by whoever holds the host, is not limited.
func (a *Authority) MayRotateServiceSecret(administrator, id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	actor, ok := a.data.Principals[administrator]
	if !ok || actor.Suspended || actor.Kind != "person" || !actor.MayAdministerServer {
		return false
	}
	target, ok := a.data.Principals[id]
	if !ok || target.Kind != "service" {
		// Not a service: answered by the rotation itself, as not_found.
		return true
	}
	administers := administered(actor)
	for dataset, g := range target.Datasets {
		if dataset != "" && (g.Read || g.Write || g.Schema || g.Erase) && !administers[dataset] {
			return false
		}
	}
	return true
}

// endPendingSignIns drops every sign-in in progress for an account, and every
// unconfirmed deletion of it. Call it holding a.mu whenever the account's
// password changes: a sign-in that proved the old password, or a deletion
// ticket bought with it, must not finish afterwards — after a recovery above
// all, which is exactly when somebody else may have held the old password.
func (a *Authority) endPendingSignIns(id string) {
	for flowID, f := range a.flows {
		if f.Principal == id {
			delete(a.flows, flowID)
		}
	}
	for key, ticket := range a.tickets {
		if ticket.Account == id {
			delete(a.tickets, key)
		}
	}
}

// MayAdministerDatasets reports whether an administrator may configure what
// reaches existing datasets: the server's administrator who also administers
// every one of them. An issuer into a dataset is a way into it — its secret
// invites whoever holds it — so configuring one, or replacing its secret,
// takes the standing to add people there directly.
func (a *Authority) MayAdministerDatasets(administrator string, datasets []string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	actor, ok := a.data.Principals[administrator]
	if !ok || actor.Suspended || actor.Kind != "person" || !actor.MayAdministerServer {
		return false
	}
	administers := administered(actor)
	for _, dataset := range datasets {
		if !administers[dataset] {
			return false
		}
	}
	return true
}
