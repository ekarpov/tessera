package identity

import (
	"context"
	"net/http"
	"sort"
	"time"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// This file is the administration surface's view of the identity registry. Every
// type here is a projection that omits secrets by construction rather than by
// remembering to leave them out of a template: passwords, salts, client secrets
// and token values have no field to travel in.

type AdminPrincipal struct {
	ID, Kind string
	// Datasets is what this principal may do, per dataset. A server holding
	// several has no single answer, which is why this is a map and not a grant.
	Datasets        map[string]store.Grants
	Authentications int
	Suspended       bool
	// CreatedAt and SuspendedAt are Unix seconds, zero where unknown or not
	// suspended. InvitedBy is the issuer that made the account, if one did.
	CreatedAt, SuspendedAt int64
	InvitedBy              string
}
type AdminClient struct {
	ID, Name, Principal string
	Redirects           []string
	// Confidential reports that a secret exists. The secret itself is never
	// read out of the registry by this package.
	Confidential, Disabled bool
}
type AdminAuthorization struct {
	ID, Principal, Client, ClientName string
	// Dataset is what this installation was approved for. An administrator of
	// more than one dataset otherwise reads a list of credentials with no
	// indication which is which, and revoking is not an act to perform on a
	// guess.
	Dataset string
	Grants  store.Grants
	// CreateDataset is the server right this installation carries. It is not a
	// grant on any dataset, so it does not show in Grants, and an installation
	// holding it is not withdrawn however empty those are.
	CreateDataset bool
	// Revoked means the credentials no longer authenticate. Withdrawn means
	// they still do, but carry no dataset access. They are different answers
	// and an administrator acts on them differently.
	Revoked, Withdrawn bool
	ActiveTokens       int
}
type AdminOverview struct {
	Principals     []AdminPrincipal
	Clients        []AdminClient
	Authorizations []AdminAuthorization
}

// AuthenticateAdministrator accepts a person who administers at least one
// dataset, or the server. A service principal is refused: administration is
// an interactive act. What they may then act on is Administers and
// AdministersServer, not this.
// AdministersServer reports that this account may act on what belongs to the
// server rather than to a dataset.
func (a *Authority) AdministersServer(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	principal, ok := a.data.Principals[id]
	return ok && principal.Kind == "person" && principal.MayAdministerServer
}

func (a *Authority) AuthenticateAdministrator(id, secret string) bool {
	a.mu.Lock()
	principal, exists := a.data.Principals[id]
	// Derive in both branches so that an unknown account costs roughly what a
	// wrong password costs. This hides nothing about which accounts exist
	// beyond that; the registry is not a public directory.
	salt, expected := "administration", ""
	if exists && !principal.Suspended {
		salt, expected = principal.Salt, principal.Password
	}
	a.mu.Unlock()
	// Derived outside the lock. This is a deliberate 600k-iteration
	// derivation, and it is the same lock every API request takes to check its
	// bearer token, so holding it here stalled unrelated traffic for as long
	// as the derivation ran. Nothing read above is written by anything but an
	// administrative action, and a password changing mid-derivation is the
	// same race as one changing just before it.
	// A derivation refused as busy counts as not signed in; the browser asks
	// again, and nothing about the password was learned either way.
	derived, err := a.derive(context.Background(), secret, salt)
	matched := err == nil && equal(derived, expected)
	if matched {
		a.mu.Lock()
		matched = a.unchanged(id, principal)
		a.mu.Unlock()
	}
	// Either kind of administrator: one who administers a dataset, or one who
	// administers the server and may hold no dataset at all. Requiring a
	// dataset would have left the second unable to reach the pages that are
	// theirs.
	return exists && matched && (len(a.Administers(id)) > 0 || a.AdministersServer(id))
}

// Administers names the datasets this account may administer: the ones where
// it holds a current erase grant.
//
// Administration is per dataset and not per server. A server holds a dataset
// for each person on it and a shared one besides, so an account owning a
// dataset of its own must not thereby administer everybody else's — which is
// what deriving administration from "erase anywhere" and then listing every
// dataset the storage holds made it.
func (a *Authority) Administers(id string) map[string]bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return administered(a.data.Principals[id])
}

func administered(principal Principal) map[string]bool {
	scope := map[string]bool{}
	if principal.Kind != "person" {
		return scope
	}
	for dataset, g := range principal.Datasets {
		// An expired grant is not a grant. The engine has read it that way
		// since grants could expire; administration read the flag alone, so a
		// grant that had run out still opened these pages.
		if g.Erase && g.Current() && dataset != "" {
			scope[dataset] = true
		}
	}
	return scope
}

// MayAdminister reports that this administrator may act on this authorization.
// Rendering a page and acting on what it shows go through the same predicate,
// so a stale page or a crafted form cannot reach further than the page could.
//
// An authorization carrying no dataset carries a server right instead, and
// belongs to the server rather than to any dataset. Only an account holding
// the server administration right may act on one.
//
// It was briefly administrable by whoever administered a dataset the holder
// belonged to, described then as the narrowest rule available. It was not
// narrow enough, because adding an existing account to a dataset needs no
// password: an administrator could manufacture the membership, and with it
// the standing to withdraw a server-level credential whose reach — every
// dataset it had created — lay entirely outside their own. The replacement
// is not a narrower derivation but no derivation: the right is held or it is
// not.
//
// Excluding those was still not enough. An approval that names a dataset may
// also carry the right to create, and creating installs access on what it
// created, so an ordinary approval's reach outgrows the dataset it names.
// Withdrawal acts across that whole reach — it has to, or a withdrawn
// installation keeps everything it made — so the standing to withdraw has to
// cover the whole reach too. An administrator of one dataset could otherwise
// cut an installation off from another they have nothing to do with.
func (a *Authority) MayAdminister(ctx context.Context, administrator, authorization string) (bool, error) {
	a.mu.Lock()
	principal := a.data.Principals[administrator]
	scope := administered(principal)
	auth, ok := a.data.Authorizations[authorization]
	a.mu.Unlock()
	if !ok {
		return false, nil
	}
	if auth.Console != nil {
		if principal.Kind != "person" || (len(scope) == 0 && !principal.MayAdministerServer) {
			return false, nil
		}
		return a.within(ctx, scope, auth)
	}
	if auth.Dataset == "" {
		// Server-level, so it is the server administrator's and nobody
		// else's. Its reach is not consulted: what it created belongs to
		// whoever administers those datasets, and ending the credential is a
		// separate act from administering them.
		return principal.MayAdministerServer && principal.Kind == "person", nil
	}
	if len(scope) == 0 || !scope[auth.Dataset] {
		return false, nil
	}
	return a.within(ctx, scope, auth)
}

// within reports that everything this authorization can reach is inside the
// administrator's scope. The reach is the engine's answer and not this
// registry's, so it is read outside the lock.
func (a *Authority) within(ctx context.Context, scope map[string]bool, auth Authorization) (bool, error) {
	reach, err := a.engine.Reach(ctx, store.Access{Principal: auth.Principal, Authorization: auth.ID})
	if err != nil {
		return false, err
	}
	for _, dataset := range reach {
		if !scope[dataset] {
			return false, nil
		}
	}
	return true, nil
}

// HoldsCreation reports that this account holds the server right to create a
// dataset. An administrator conferring a right is bound by the rule every
// other grant here follows: never more than the granter holds.
func (a *Authority) HoldsCreation(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	principal, ok := a.data.Principals[id]
	return ok && principal.Kind == "person" && principal.MayCreateDataset
}

// RecordCreation records the account that created a dataset as holding it,
// and does so once.
//
// Creation is the composition acting, so a new dataset has no member at all
// until one is named and its creator could otherwise neither administer nor
// see what they had just made. But the notice that carries this runs on a
// replay as well as on the first attempt — that is what makes bookkeeping
// which failed once get retried — and writing the grants unconditionally
// turned a replay into a way to undo an administrator.
//
// Reported by review: create a dataset, have your grants on it reduced, then
// replay the original creation request and hold it fully again. So the rule
// is not "write the creator's grants" but "record the creation", and a
// dataset somebody already holds is a creation already recorded.
func (a *Authority) RecordCreation(principalID, dataset string, grants store.Grants) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, principal := range a.data.Principals {
		if _, holds := principal.Datasets[dataset]; holds {
			return nil
		}
	}
	principal, ok := a.data.Principals[principalID]
	if !ok || dataset == "" {
		return store.Refuse("not_found")
	}
	if principal.Datasets == nil {
		principal.Datasets = map[string]store.Grants{}
	}
	principal.Datasets[dataset] = grants
	a.data.Principals[principalID] = principal
	if a.data.Owners == nil {
		a.data.Owners = map[string]string{}
	}
	// A replay after all grants were removed must not transfer ownership.
	if a.data.Owners[dataset] == "" {
		a.data.Owners[dataset] = principalID
	}
	if err := a.syncSharedReads(context.Background(), principalID); err != nil {
		return err
	}
	return a.save()
}

// GrantDataset adds an existing account to a dataset, replacing whatever it
// held there. It is an administrative decision and it is meant to overwrite;
// RecordCreation is the one that must not.
func (a *Authority) GrantDataset(principalID, dataset string, grants store.Grants) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	principal, ok := a.data.Principals[principalID]
	if !ok || dataset == "" {
		return store.Refuse("not_found")
	}
	if principal.Datasets == nil {
		principal.Datasets = map[string]store.Grants{}
	}
	principal.Datasets[dataset] = grants
	a.data.Principals[principalID] = principal
	if err := a.syncSharedReads(context.Background(), principalID); err != nil {
		return err
	}
	return a.save()
}

// AdminOverview returns the registry as this administrator may see it, ordered
// so that two renderings of unchanged state are identical.
//
// Scoped to the datasets they administer. The registry describes every account
// on the server, and an administrator of one dataset has no business reading
// the membership of another — nor revoking credentials they cannot see the
// purpose of.
func (a *Authority) AdminOverview(ctx context.Context, administrator string) AdminOverview {
	overview, scope, authorizations := a.overview(administrator)
	// What the page offers and what an action accepts go through one rule, so
	// an approval reaching outside this scope is not shown either. The reach
	// is a question for the engine, asked after the lock is dropped.
	shown := make([]AdminAuthorization, 0, len(overview.Authorizations))
	for _, entry := range overview.Authorizations {
		within, err := a.within(ctx, scope, authorizations[entry.ID])
		if err != nil || !within {
			continue
		}
		shown = append(shown, entry)
	}
	overview.Authorizations = shown
	return overview
}

func (a *Authority) overview(administrator string) (AdminOverview, map[string]bool, map[string]Authorization) {
	a.mu.Lock()
	defer a.mu.Unlock()
	scope := administered(a.data.Principals[administrator])
	serverAdministrator := a.data.Principals[administrator].MayAdministerServer && !a.data.Principals[administrator].Suspended
	listed := map[string]Authorization{}
	now := time.Now().Unix()
	tokens := map[string]int{}
	for _, token := range a.data.Tokens {
		if token.Expires > now && !token.Used {
			tokens[token.Authorization]++
		}
	}
	overview := AdminOverview{}
	shows := map[string]bool{}
	// An account shows through the datasets this administrator holds, and it
	// shows only those: an account's membership of a dataset out of scope is
	// exactly what this surface must not report.
	for _, principal := range a.data.Principals {
		shown := map[string]store.Grants{}
		for dataset, g := range principal.Datasets {
			if scope[dataset] {
				shown[dataset] = g
			}
		}
		// A server administrator sees every account, because suspending or
		// deleting one is theirs to do whoever holds its datasets. What those
		// datasets are is still not shown past their own scope.
		// Their applications and approvals still show only through a dataset
		// in scope.
		if len(shown) == 0 && !serverAdministrator {
			continue
		}
		shows[principal.ID] = len(shown) > 0
		entry := AdminPrincipal{ID: principal.ID, Kind: principal.Kind, Datasets: shown, Suspended: principal.Suspended,
			CreatedAt: principal.CreatedAt, SuspendedAt: principal.SuspendedAt, InvitedBy: principal.InvitedBy}
		for _, auth := range a.data.Authorizations {
			if auth.Principal == principal.ID && !auth.Revoked {
				entry.Authentications++
			}
		}
		overview.Principals = append(overview.Principals, entry)
	}
	for _, client := range a.data.Clients {
		if !shows[client.Principal] {
			continue
		}
		overview.Clients = append(overview.Clients, AdminClient{ID: client.ID, Name: client.Name, Principal: client.Principal,
			Redirects: append([]string{}, client.Redirects...), Confidential: client.Secret != "", Disabled: client.Disabled})
	}
	for _, auth := range a.data.Authorizations {
		// The same rule MayAdminister applies, so what a page offers and what
		// an action accepts cannot drift apart — including the server-level
		// approvals, which this surface neither shows nor acts on.
		if auth.Console != nil {
			if !shows[auth.Principal] || (len(scope) == 0 && !serverAdministrator) {
				continue
			}
		} else if auth.Dataset == "" {
			// Shown to the server's administrator and to nobody else, by the
			// same rule that decides who may act on it.
			if !a.data.Principals[administrator].MayAdministerServer {
				continue
			}
		} else if !scope[auth.Dataset] {
			continue
		}
		grants := auth.Grants
		listed[auth.ID] = auth
		overview.Authorizations = append(overview.Authorizations, AdminAuthorization{ID: auth.ID, Principal: auth.Principal,
			Client: auth.Client, ClientName: a.data.Clients[auth.Client].Name, Dataset: auth.Dataset, Grants: grants, Revoked: auth.Revoked,
			CreateDataset: auth.CreateDataset,
			Withdrawn:     !auth.Revoked && ((auth.Console != nil && auth.Console.Withdrawn) || (auth.Console == nil && !auth.CreateDataset && !auth.Administer && !grants.Read && !grants.Write && !grants.Schema && !grants.Erase)),
			ActiveTokens:  tokens[auth.ID]})
	}
	sort.Slice(overview.Principals, func(i, j int) bool { return overview.Principals[i].ID < overview.Principals[j].ID })
	sort.Slice(overview.Clients, func(i, j int) bool { return overview.Clients[i].ID < overview.Clients[j].ID })
	sort.Slice(overview.Authorizations, func(i, j int) bool {
		return overview.Authorizations[i].ID < overview.Authorizations[j].ID
	})
	return overview, scope, listed
}

// RevokeAuthorization ends the credentials. Existing access and refresh tokens
// stop working and the dataset authorization goes with them.
func (a *Authority) RevokeAuthorization(ctx context.Context, id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.data.Authorizations[id]; !ok {
		return store.Refuse("not_found")
	}
	return a.revoke(ctx, id)
}

// WithdrawGrants removes dataset access while leaving the credentials valid,
// which is the separation the deployment contract requires: a principal that may
// no longer read the data can still authenticate, and can still recover the
// outcome of a write it made while it could.
func (a *Authority) WithdrawGrants(ctx context.Context, id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	auth, ok := a.data.Authorizations[id]
	if !ok {
		return store.Refuse("not_found")
	}
	if auth.Console != nil {
		if auth.Revoked || auth.Console.Withdrawn {
			return store.Refuse("conflict")
		}
		if err := a.revokeEverywhere(ctx, auth); err != nil {
			return err
		}
		binding := *auth.Console
		binding.Withdrawn = true
		auth.Console = &binding
		auth.Administer = false
		a.data.Authorizations[id] = auth
		return a.save()
	}
	grants := auth.Grants
	// Refuse a withdrawal that would change nothing, so that the action an
	// administrator took and the outcome the page reports are the same thing.
	// Holding the server right alone is something, and an installation that
	// holds only it is one an administrator must still be able to stop.
	if auth.Revoked || !(grants.Read || grants.Write || grants.Schema || grants.Erase || auth.CreateDataset) {
		return store.Refuse("conflict")
	}
	if err := a.revokeEverywhere(ctx, auth); err != nil {
		return err
	}
	auth.Grants = store.Grants{}
	// The right to create goes with the access, because creating a dataset is
	// how an installation grants itself access to one. Leaving it behind would
	// let a withdrawn installation replace in one request everything the
	// withdrawal had just taken away.
	auth.CreateDataset = false
	a.data.Authorizations[id] = auth
	return a.save()
}

// GrantRead installs a read-only share over named concrete types. Guests never
// receive write, and a share never widens what the granting account itself has.
func (a *Authority) GrantRead(ctx context.Context, id string, types []string, expiry *time.Time) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	auth, ok := a.data.Authorizations[id]
	if !ok || auth.Revoked {
		return store.Refuse("not_found")
	}
	if len(types) == 0 || len(types) > 128 {
		return store.Refuse("invalid_field")
	}
	for _, typeID := range types {
		if p.ValidateIdentifier(p.MetaIdentifier, typeID) != nil {
			return store.Refuse("invalid_field")
		}
	}
	principal, ok := a.data.Principals[auth.Principal]
	if !ok {
		return store.Refuse("not_found")
	}
	// A share makes an account a guest on this dataset, and an account that
	// writes here is not one. When a share narrowed a single authorization,
	// sharing a writer's installation left the writer's account alone; now that
	// the scope is the account's, it would quietly make a writer read-only
	// everywhere — or an administrator unable to administer. Restricting a
	// writer is a different decision from sharing with a guest, and it is
	// refused here rather than taken by accident.
	held := principal.Datasets[auth.Dataset]
	if held.Write || held.Schema || held.Erase {
		return store.Refuse("conflict")
	}
	grants := store.Grants{Read: true}
	if expiry != nil {
		if !expiry.After(time.Now()) {
			return store.Refuse("invalid_field")
		}
		when := *expiry
		grants.ExpiresAt = &when
	}
	if err := a.scopeReader(ctx, principal, auth.Dataset, types, grants); err != nil {
		return err
	}
	return a.save()
}

// scopeReader makes an account a type-scoped reader of a dataset: every live
// authorization it holds there is narrowed now, and the account records the
// scope so every later one inherits it. A guest signed in on two devices is
// one guest, and narrowing one device would leave the other reading
// everything. The caller holds the lock and saves.
func (a *Authority) scopeReader(ctx context.Context, principal Principal, dataset string, types []string, grants store.Grants) error {
	if principal.SharedTypes == nil {
		principal.SharedTypes = map[string][]string{}
	}
	if principal.Datasets == nil {
		principal.Datasets = map[string]store.Grants{}
	}
	// Reading and contributing are held side by side: making an account a
	// reader leaves what it may add to where it was.
	grants.Insert = principal.Datasets[dataset].Insert
	// A share naming the account is its own standing, and takes precedence
	// over any group it reads the dataset through.
	if principal.ViaGroup[dataset] != "" {
		// A named replacement keeps the withdrawal it inherited from the group.
		principal.FrozenReaders = withFreeze(principal.FrozenReaders, dataset, readerFreeze(principal, dataset))
		principal.GroupFreezes = withFreeze(principal.GroupFreezes, dataset, ReaderFreeze{})
		groups := map[string]string{}
		for d, issuer := range principal.ViaGroup {
			if d != dataset {
				groups[d] = issuer
			}
		}
		principal.ViaGroup = groups
	}
	principal.SharedTypes[dataset] = append([]string(nil), types...)
	principal.Datasets[dataset] = grants
	a.data.Principals[principal.ID] = principal
	a.reshared(dataset, types)
	return a.reprovisionGuest(ctx, principal, dataset)
}

// guestView is the view every authorization of this account on the dataset
// carries, and whether the account is a guest there — a reader by type or a
// contributor — rather than somebody holding the dataset whole.
func guestView(principal Principal, dataset string) (store.View, bool) {
	types, shared := principal.SharedTypes[dataset]
	inserts := principal.InsertTypes[dataset]
	if !shared && len(inserts) == 0 {
		return store.FullView().BeforeSequence(readerFreeze(principal, dataset).Cut), false
	}
	view := store.SharedView(types)
	if !shared && principal.Datasets[dataset].Read {
		view = store.FullView()
	}
	return view.WithInserts(inserts).BeforeSequence(readerFreeze(principal, dataset).Cut), true
}

// reprovisionGuest brings every live authorization the account holds on the
// dataset to what the account now holds there. A guest signed in on two
// devices is one guest, and changing one device would leave the other as it
// was. An installation keeps no right the account no longer holds and gains
// none it was not approved for: one approved to read reads as the guest does,
// and one approved to add adds as the guest does. One approved before
// approvals were recorded keeps reading, as it always did, and adds nothing.
// The caller holds the lock and saves.
func (a *Authority) reprovisionGuest(ctx context.Context, principal Principal, dataset string) error {
	held := principal.Datasets[dataset]
	view, _ := guestView(principal, dataset)
	for authID, other := range a.data.Authorizations {
		if other.Principal != principal.ID || other.Dataset != dataset || other.Revoked {
			continue
		}
		approved := store.Grants{Read: true}
		if other.Approved != nil {
			approved = *other.Approved
		}
		grants := store.Grants{Read: held.Read && approved.Read, Insert: held.Insert && approved.Insert, ExpiresAt: held.ExpiresAt}
		if err := a.engine.SetAuthorization(ctx, dataset, store.Access{Principal: principal.ID, Authorization: authID},
			store.Authorization{Grants: grants, View: view}); err != nil {
			return err
		}
		other.Grants = grants
		a.data.Authorizations[authID] = other
	}
	return a.syncSharedReads(ctx, principal.ID)
}

// AdminRateLimited applies the shared per-address and global buckets.
func (a *Authority) AdminRateLimited(r *http.Request, bucket string, max int) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return !a.allowed(r, bucket, max)
}

// IssueCSRF mints a single-use token bound to the administrator. ConsumeCSRF
// spends it, so a replayed form submission does not act twice.
func (a *Authority) IssueCSRF(key string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	token := random()
	a.flows["admin:"+key] = flow{CSRF: token, Expires: time.Now().Add(5 * time.Minute)}
	return token
}
func (a *Authority) ConsumeCSRF(key, token string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	f, ok := a.flows["admin:"+key]
	if !ok || time.Now().After(f.Expires) || !equal(f.CSRF, token) {
		return false
	}
	delete(a.flows, "admin:"+key)
	return true
}
