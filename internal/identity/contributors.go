package identity

import (
	"context"
	"sort"
	"time"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// Contributors: who may add records to a dataset, of which types, managed by
// account as readers are.
//
// A contributor adds and holds nothing afterwards. What it adds belongs to the
// dataset the moment it commits: it cannot read it, change it or delete it, and
// an insert grant conveys no continuing authority over what was written. That
// is what separates a sensor, a form or a bank feed — which assert something
// once — from a writer who has to correct and withdraw, and which this is not
// for.

// Contributor is one account that adds records to a dataset.
type Contributor struct {
	Account   string     `json:"account"`
	Types     []string   `json:"types"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// Contributors lists the accounts that add to a dataset, for one of its
// administrators. An account that also reads is listed here and among the
// readers, since it is both.
func (a *Authority) Contributors(administrator, dataset string) ([]Contributor, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.administersDataset(administrator, dataset) {
		return nil, store.Refuse("permission_denied")
	}
	out := []Contributor{}
	for id, principal := range a.data.Principals {
		held, ok := principal.Datasets[dataset]
		if !ok || !held.Insert || held.Write || held.Schema || held.Erase {
			continue
		}
		out = append(out, Contributor{Account: id, Types: principal.InsertTypes[dataset], ExpiresAt: held.ExpiresAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Account < out[j].Account })
	return out, nil
}

// SetContributor lets an account add records of the named concrete types to a
// dataset, creating a person's account when secret is given and it does not
// exist. Calling it again replaces the type list and the expiry. An account
// that reads keeps reading. The expiry is the account's on this dataset, as a
// reader's is, so it ends reading and adding together, and setting either
// standing again replaces it.
//
// Refused for an account that writes to the dataset: it already adds, and
// narrowing it to adding is a different decision.
func (a *Authority) SetContributor(ctx context.Context, administrator, dataset, account string, types []string, expiry *time.Time, secret string) error {
	if len(types) == 0 || len(types) > 128 {
		return store.Refuse("invalid_field")
	}
	for _, typeID := range types {
		if p.ValidateIdentifier(p.MetaIdentifier, typeID) != nil {
			return store.Refuse("invalid_field")
		}
	}
	if expiry != nil && !expiry.After(time.Now()) {
		return store.Refuse("invalid_field")
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
	// A service is the common contributor: a device or a feed that asserts
	// what it measured and has no reason to see anything.
	case exists && principal.Kind != "person" && principal.Kind != "service":
		return store.Refuse("conflict")
	case !exists && (secret == "" || a.data.Retired[account] || account == "" || len(account) > 255):
		return store.Refuse("not_found")
	case !exists:
		principal = Principal{ID: account, Kind: "person", Salt: salt, Password: derived, CreatedAt: time.Now().Unix()}
	}
	held := principal.Datasets[dataset]
	if held.Write || held.Schema || held.Erase {
		return store.Refuse("conflict")
	}
	held.Insert = true
	held.ExpiresAt = nil
	if expiry != nil {
		when := *expiry
		held.ExpiresAt = &when
	}
	datasets := map[string]store.Grants{}
	for d, g := range principal.Datasets {
		datasets[d] = g
	}
	datasets[dataset] = held
	principal.Datasets = datasets
	inserts := map[string][]string{}
	for d, t := range principal.InsertTypes {
		inserts[d] = t
	}
	inserts[dataset] = append([]string(nil), types...)
	principal.InsertTypes = inserts
	a.data.Principals[account] = principal
	if err := a.reprovisionGuest(ctx, principal, dataset); err != nil {
		return err
	}
	return a.save()
}

// RemoveContributor stops an account adding to a dataset. What it added stays,
// because it was never the contributor's. An account that also reads keeps
// reading; one that only added loses its standing here entirely.
func (a *Authority) RemoveContributor(ctx context.Context, administrator, dataset, account string) error {
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
	if !ok || !held.Insert {
		return store.Refuse("not_found")
	}
	if held.Write || held.Schema || held.Erase {
		return store.Refuse("conflict")
	}
	if held.Read {
		held.Insert = false
		return a.narrowGuest(ctx, principal, dataset, held, func(p *Principal) { delete(p.InsertTypes, dataset) })
	}
	return a.endGuest(ctx, principal, dataset)
}
