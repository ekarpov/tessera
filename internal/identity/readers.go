package identity

import (
	"context"
	"sort"
	"time"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// Readers: who may read what in a dataset, managed by account.
//
// The administration page shares by authorization, an identifier a person
// never sees, and adding an account with read access left it reading the
// whole dataset until the first share narrowed it — a window a product must
// never leave open. These operate on accounts, and adding a reader sets its
// types in the same step that grants it read.

// Reader is one account that reads a dataset.
type Reader struct {
	Account string `json:"account"`
	// Types is nil when the account reads the whole dataset.
	Types     []string   `json:"types"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// Group names the group grant this account reads through, as
	// "issuer:<id>", when it does.
	Group    string `json:"group,omitempty"`
	FrozenAt string `json:"frozen_at,omitempty"`
	FrozenBy string `json:"frozen_by,omitempty"`
}

// Readers lists the read-only accounts of a dataset, for one of its
// administrators. Accounts that also write are not readers and are not listed.
func (a *Authority) Readers(administrator, dataset string) ([]Reader, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.administersDataset(administrator, dataset) && !a.writesDataset(administrator, dataset) {
		return nil, store.Refuse("permission_denied")
	}
	out := []Reader{}
	for id, principal := range a.data.Principals {
		held, ok := principal.Datasets[dataset]
		if !ok || !held.Read || held.Write || held.Schema || held.Erase {
			continue
		}
		reader := Reader{Account: id, Types: principal.SharedTypes[dataset], ExpiresAt: held.ExpiresAt, FrozenAt: readerFreeze(principal, dataset).At, FrozenBy: readerFreeze(principal, dataset).By}
		if issuer := principal.ViaGroup[dataset]; issuer != "" {
			reader.Group = "issuer:" + issuer
		}
		out = append(out, reader)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Account < out[j].Account })
	return out, nil
}

// SetReader makes an account a reader of named types in a dataset, creating
// the account when secret is given and it does not exist. Calling it again
// replaces the type list, which is how a person changes what they share.
//
// Refused for an account that writes to the dataset: restricting a writer is
// a different decision from sharing with a reader, for the reason GrantRead
// gives. Never more than the administrator has — they administer the dataset,
// and a reader holds read.
func (a *Authority) SetReader(ctx context.Context, administrator, dataset, account string, types []string, expiry *time.Time, secret string) error {
	if len(types) == 0 || len(types) > 128 {
		return store.Refuse("invalid_field")
	}
	for _, typeID := range types {
		if p.ValidateIdentifier(p.MetaIdentifier, typeID) != nil {
			return store.Refuse("invalid_field")
		}
	}
	grants := store.Grants{Read: true}
	if expiry != nil {
		if !expiry.After(time.Now()) {
			return store.Refuse("invalid_field")
		}
		when := *expiry
		grants.ExpiresAt = &when
	}
	// A new account's password is derived before the lock, for the reason
	// ResetPassword gives.
	var salt, derived string
	if secret != "" {
		if len(secret) < 12 {
			return store.Refuse("invalid_field")
		}
		salt = random()
		var err error
		if derived, err = a.derive(ctx, secret, salt); err != nil {
			return err
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.administersDataset(administrator, dataset) {
		return store.Refuse("permission_denied")
	}
	principal, exists := a.data.Principals[account]
	switch {
	// A service can be a reader: a household's service reading what each
	// member chose to share is the same decision as sharing with a person.
	case exists && principal.Kind != "person" && principal.Kind != "service":
		return store.Refuse("conflict")
	case !exists && (secret == "" || a.data.Retired[account] || account == "" || len(account) > 255):
		return store.Refuse("not_found")
	case !exists:
		principal = Principal{ID: account, Kind: "person", Salt: salt, Password: derived, CreatedAt: time.Now().Unix()}
	}
	if held := principal.Datasets[dataset]; held.Write || held.Schema || held.Erase {
		return store.Refuse("conflict")
	}
	if err := a.scopeReader(ctx, principal, dataset, types, grants); err != nil {
		return err
	}
	return a.save()
}

// RemoveReader ends an account's access to a dataset: the grant goes, the
// scope goes, and every authorization it held there is revoked. The account
// itself stays; it may belong to other datasets.
func (a *Authority) RemoveReader(ctx context.Context, administrator, dataset, account string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.administersDataset(administrator, dataset) {
		return store.Refuse("permission_denied")
	}
	principal, ok := a.data.Principals[account]
	if !ok {
		return store.Refuse("not_found")
	}
	held, ok := principal.Datasets[dataset]
	if !ok {
		return store.Refuse("not_found")
	}
	if held.Write || held.Schema || held.Erase {
		return store.Refuse("conflict")
	}
	// A group reader is one because of the group, and removing them here
	// would last until the group was next applied. The group grant is what
	// to change.
	if principal.ViaGroup[dataset] != "" {
		return store.RefuseWith("conflict", "group_reader", map[string]any{"issuer": principal.ViaGroup[dataset]})
	}
	// An account that also contributes keeps contributing: it stops reading,
	// and every installation it holds here is brought to that.
	if held.Insert {
		if !held.Read {
			return store.Refuse("not_found")
		}
		held.Read = false
		if err := a.narrowGuest(ctx, principal, dataset, held, func(p *Principal) { delete(p.SharedTypes, dataset) }); err != nil {
			return err
		}
	} else if err := a.endGuest(ctx, principal, dataset); err != nil {
		return err
	}
	// A share naming the account took precedence over its group's; with it
	// gone, the group's applies again.
	if err := a.applyGroups(ctx, account); err != nil {
		return err
	}
	return a.save()
}

// endGuest ends an account's standing on a dataset where it was a guest: the
// grant goes, every scope goes, and every authorization it held there is
// revoked. The account itself stays; it may belong to other datasets. The
// caller holds the lock.
func (a *Authority) endGuest(ctx context.Context, principal Principal, dataset string) error {
	if err := a.endGuestUnsaved(ctx, principal, dataset); err != nil {
		return err
	}
	return a.save()
}

// endGuestUnsaved is endGuest for a caller that saves.
func (a *Authority) endGuestUnsaved(ctx context.Context, principal Principal, dataset string) error {
	account := principal.ID
	for authID, auth := range a.data.Authorizations {
		if auth.Principal != account || auth.Dataset != dataset || auth.Revoked {
			continue
		}
		if err := a.revokeEverywhere(ctx, auth); err != nil {
			return err
		}
		auth.Revoked = true
		a.data.Authorizations[authID] = auth
	}
	datasets := map[string]store.Grants{}
	for d, g := range principal.Datasets {
		if d != dataset {
			datasets[d] = g
		}
	}
	principal.Datasets = datasets
	delete(principal.SharedTypes, dataset)
	delete(principal.InsertTypes, dataset)
	delete(principal.ViaGroup, dataset)
	a.data.Principals[account] = principal
	return a.syncSharedReads(ctx, account)
}

// narrowGuest takes one of a guest's two standings away and keeps the other.
// The caller holds the lock.
func (a *Authority) narrowGuest(ctx context.Context, principal Principal, dataset string, held store.Grants, forget func(*Principal)) error {
	datasets := map[string]store.Grants{}
	for d, g := range principal.Datasets {
		datasets[d] = g
	}
	datasets[dataset] = held
	principal.Datasets = datasets
	forget(&principal)
	a.data.Principals[principal.ID] = principal
	if err := a.reprovisionGuest(ctx, principal, dataset); err != nil {
		return err
	}
	return a.save()
}

// administersDataset is the standing rule for everything dataset-scoped here:
// the acting account holds a current erase grant on it, or administers the
// server. The caller holds the lock.
func (a *Authority) administersDataset(administrator, dataset string) bool {
	actor, ok := a.data.Principals[administrator]
	if !ok || actor.Suspended || actor.Kind != "person" {
		return false
	}
	// Administering the server is not administering a dataset: a server
	// administrator holding no data who could manage readers could make
	// themselves one, and read it.
	return administered(actor)[dataset]
}
