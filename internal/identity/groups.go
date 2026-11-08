package identity

import (
	"context"
	"sort"
	"time"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// Group readers: a dataset shared with everyone one issuer made, rather than
// with named accounts. A household's members each share chosen types with
// the household, and a member who joins later reads them without anybody
// naming them — which per-account shares cannot do, since no member can see
// who the others are.
//
// A group grant is applied to each member as the reader standing a named
// share would give, so everything that follows a share follows it: types,
// expiry, suspension, shared reads. What is different is who decides, and it
// is narrow on purpose: only an administrator of the dataset, and only for the
// issuer that made their own account. Issuing that issuer's invitations is
// then a grant of read on every such share, which is what the issuer's cap and
// its being disabled are for.
//
// A share naming the account directly wins over the group's, and an account
// that writes the dataset is never a group reader of it.

// GroupGrant is what a group reads on one dataset.
type GroupGrant struct {
	Freeze    ReaderFreeze `json:",omitempty"`
	Types     []string     `json:"types"`
	ExpiresAt *time.Time   `json:"expires_at,omitempty"`
}

// Group is a group grant as an administrator sees it, with the accounts it
// reaches now.
type Group struct {
	Issuer    string     `json:"issuer"`
	Types     []string   `json:"types"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Members   []string   `json:"members"`
	FrozenAt  string     `json:"frozen_at,omitempty"`
	FrozenBy  string     `json:"frozen_by,omitempty"`
}

// SetGroupReader lets every account the issuer made read the named types of
// a dataset, including accounts it makes later. Calling it again replaces the
// types and the expiry.
func (a *Authority) SetGroupReader(ctx context.Context, administrator, dataset, issuer string, types []string, expiry *time.Time) error {
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
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.administersDataset(administrator, dataset) || issuer == "" || a.data.Principals[administrator].InvitedBy != issuer {
		return store.Refuse("permission_denied")
	}
	grant := GroupGrant{Types: append([]string(nil), types...), Freeze: a.data.FrozenGroups[dataset][issuer]}
	if expiry != nil {
		when := *expiry
		grant.ExpiresAt = &when
	}
	if a.data.Groups == nil {
		a.data.Groups = map[string]map[string]GroupGrant{}
	}
	if a.data.Groups[dataset] == nil {
		a.data.Groups[dataset] = map[string]GroupGrant{}
	}
	a.data.Groups[dataset][issuer] = grant
	a.reshared(dataset, types)
	for _, member := range a.members(issuer) {
		if err := a.applyGroups(ctx, member); err != nil {
			return err
		}
	}
	return a.save()
}

// RemoveGroupReader ends a group grant. Every account reading the dataset
// through it stops; one that also holds a share of its own keeps that.
func (a *Authority) RemoveGroupReader(ctx context.Context, administrator, dataset, issuer string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.administersDataset(administrator, dataset) {
		return store.Refuse("permission_denied")
	}
	if _, ok := a.data.Groups[dataset][issuer]; !ok {
		return store.Refuse("not_found")
	}
	delete(a.data.Groups[dataset], issuer)
	if len(a.data.Groups[dataset]) == 0 {
		delete(a.data.Groups, dataset)
	}
	for _, member := range a.members(issuer) {
		principal := a.data.Principals[member]
		if principal.ViaGroup[dataset] != issuer {
			continue
		}
		if err := a.leaveGroup(ctx, principal, dataset); err != nil {
			return err
		}
	}
	return a.save()
}

// Groups lists a dataset's group grants for one of its administrators.
func (a *Authority) Groups(administrator, dataset string) ([]Group, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.administersDataset(administrator, dataset) && !a.writesDataset(administrator, dataset) {
		return nil, store.Refuse("permission_denied")
	}
	out := []Group{}
	for issuer, grant := range a.data.Groups[dataset] {
		members := []string{}
		for _, member := range a.members(issuer) {
			if a.data.Principals[member].ViaGroup[dataset] == issuer {
				members = append(members, member)
			}
		}
		out = append(out, Group{Issuer: issuer, Types: grant.Types, ExpiresAt: grant.ExpiresAt, Members: members, FrozenAt: grant.Freeze.At, FrozenBy: grant.Freeze.By})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Issuer < out[j].Issuer })
	return out, nil
}

// members names the person accounts an issuer made, in order. The caller
// holds the lock.
func (a *Authority) members(issuer string) []string {
	out := []string{}
	for id, principal := range a.data.Principals {
		if principal.Kind == "person" && principal.InvitedBy == issuer && issuer != "" {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// applyGroups gives an account the reader standing its issuer's group grants
// call for, on every dataset that has one, and brings its installations to
// it. A dataset the account holds some other way — a share naming it, or
// writing it — is left as it is. The caller holds the lock and saves.
func (a *Authority) applyGroups(ctx context.Context, principalID string) error {
	principal, ok := a.data.Principals[principalID]
	if !ok || principal.Kind != "person" || principal.InvitedBy == "" {
		return nil
	}
	datasets := make([]string, 0, len(a.data.Groups))
	for dataset := range a.data.Groups {
		datasets = append(datasets, dataset)
	}
	sort.Strings(datasets)
	for _, dataset := range datasets {
		grant, ok := a.data.Groups[dataset][principal.InvitedBy]
		if !ok {
			continue
		}
		held, holds := principal.Datasets[dataset]
		if held.Write || held.Schema || held.Erase || (holds && principal.ViaGroup[dataset] == "") {
			continue
		}
		standing := store.Grants{Read: true, Insert: held.Insert}
		if grant.ExpiresAt != nil {
			when := *grant.ExpiresAt
			standing.ExpiresAt = &when
		}
		principal = withDataset(principal, dataset, standing)
		principal.SharedTypes = withEntry(principal.SharedTypes, dataset, append([]string(nil), grant.Types...))
		principal.ViaGroup = withGroup(principal.ViaGroup, dataset, principal.InvitedBy)
		principal.GroupFreezes = withFreeze(principal.GroupFreezes, dataset, grant.Freeze)
		a.data.Principals[principalID] = principal
		if err := a.reprovisionGuest(ctx, principal, dataset); err != nil {
			return err
		}
	}
	return nil
}

// leaveGroup ends the standing an account had on a dataset through a group.
// An account that also adds records there keeps adding. The caller holds the
// lock and saves.
func (a *Authority) leaveGroup(ctx context.Context, principal Principal, dataset string) error {
	delete(principal.ViaGroup, dataset)
	principal.GroupFreezes = withFreeze(principal.GroupFreezes, dataset, ReaderFreeze{})
	held := principal.Datasets[dataset]
	if held.Insert {
		held.Read = false
		held.ExpiresAt = nil
		datasets := map[string]store.Grants{}
		for d, g := range principal.Datasets {
			datasets[d] = g
		}
		datasets[dataset] = held
		principal.Datasets = datasets
		delete(principal.SharedTypes, dataset)
		a.data.Principals[principal.ID] = principal
		return a.reprovisionGuest(ctx, principal, dataset)
	}
	a.data.Principals[principal.ID] = principal
	return a.endGuestUnsaved(ctx, principal, dataset)
}

func withDataset(principal Principal, dataset string, grants store.Grants) Principal {
	datasets := map[string]store.Grants{}
	for d, g := range principal.Datasets {
		datasets[d] = g
	}
	datasets[dataset] = grants
	principal.Datasets = datasets
	return principal
}

func withEntry(m map[string][]string, key string, value []string) map[string][]string {
	out := map[string][]string{}
	for k, v := range m {
		out[k] = v
	}
	out[key] = value
	return out
}

func withGroup(m map[string]string, dataset, issuer string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		out[k] = v
	}
	out[dataset] = issuer
	return out
}

// reshared clears the record of a type having been stopped, once the dataset
// shares it again. The caller holds the lock.
func (a *Authority) reshared(dataset string, types []string) {
	for _, typeID := range types {
		delete(a.data.Stopped[dataset], typeID)
	}
	if len(a.data.Stopped[dataset]) == 0 {
		delete(a.data.Stopped, dataset)
	}
}

// Stop is a type somebody stopped a dataset sharing, as its administrators
// are told.
type Stop struct {
	Type      string `json:"type"`
	StoppedBy string `json:"stopped_by"`
	StoppedAt string `json:"stopped_at"`
}

// Stops lists who stopped which types being shared from a dataset, and when,
// for one of its administrators.
func (a *Authority) Stops(administrator, dataset string) ([]Stop, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.administersDataset(administrator, dataset) && !a.writesDataset(administrator, dataset) {
		return nil, store.Refuse("permission_denied")
	}
	out := []Stop{}
	for typeID, record := range a.data.Stopped[dataset] {
		out = append(out, Stop{Type: typeID, StoppedBy: record.By, StoppedAt: time.Unix(record.At, 0).UTC().Format(time.RFC3339)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out, nil
}

// Unshared is what removing types from the shares of one dataset changes.
type Unshared struct {
	Dataset string `json:"dataset"`
	// Groups are the issuers whose group share named one of the types.
	Groups []string `json:"groups"`
	// Readers are the accounts that stop reading at least one of the types,
	// through a group or a share naming them.
	Readers []string `json:"readers"`
	// WholeDatasetReaders read the whole dataset, not by type, and are not
	// changed by removing a type: ending that is ending their share.
	WholeDatasetReaders []string `json:"whole_dataset_readers,omitempty"`
}

// Unshare removes types from every share — group and named — on the datasets
// the caller may moderate, so nobody reads records of those types there any
// more. Nothing is deleted: each dataset keeps its records, and its own account
// can still read and change them, and share them again. A share left with no
// types ends. Each type that stops being shared from a dataset is recorded
// there with who stopped it and when, for the dataset's own account to read.
//
// The caller may moderate a dataset it administers, and — as the server's
// administrator — one an issuer configured to moderate sharing made. Only the
// named datasets are touched when some are named; otherwise every one it may
// moderate. With dryRun it answers what it would change and changes nothing.
func (a *Authority) Unshare(ctx context.Context, administrator string, types, only []string, dryRun bool) ([]Unshared, error) {
	if len(types) == 0 || len(types) > 128 {
		return nil, store.Refuse("invalid_field")
	}
	removed := map[string]bool{}
	for _, typeID := range types {
		if p.ValidateIdentifier(p.MetaIdentifier, typeID) != nil {
			return nil, store.Refuse("invalid_field")
		}
		removed[typeID] = true
	}
	without := func(held []string) []string {
		out := []string{}
		for _, typeID := range held {
			if !removed[typeID] {
				out = append(out, typeID)
			}
		}
		return out
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	actor, ok := a.data.Principals[administrator]
	if !ok || actor.Suspended || actor.Kind != "person" {
		return nil, store.Refuse("permission_denied")
	}
	moderated := administered(actor)
	if actor.MayAdministerServer {
		for dataset, issuer := range a.data.MadeBy {
			if a.data.Issuers[issuer].ModerateSharing {
				moderated[dataset] = true
			}
		}
	}
	datasets := []string{}
	if len(only) > 0 {
		// Named, every one must be the caller's to moderate: stopping one
		// member's Track must not quietly do less than was asked, nor reach a
		// dataset the caller has no standing on.
		for _, dataset := range only {
			if !moderated[dataset] {
				return nil, store.Refuse("permission_denied")
			}
			datasets = append(datasets, dataset)
		}
	} else {
		for dataset := range moderated {
			datasets = append(datasets, dataset)
		}
	}
	sort.Strings(datasets)
	stopped := func(dataset string, removedHere []string) {
		if dryRun {
			return
		}
		if a.data.Stopped == nil {
			a.data.Stopped = map[string]map[string]StopRecord{}
		}
		if a.data.Stopped[dataset] == nil {
			a.data.Stopped[dataset] = map[string]StopRecord{}
		}
		for _, typeID := range removedHere {
			a.data.Stopped[dataset][typeID] = StopRecord{By: administrator, At: time.Now().Unix()}
		}
	}
	intersect := func(held []string) []string {
		out := []string{}
		for _, typeID := range held {
			if removed[typeID] {
				out = append(out, typeID)
			}
		}
		return out
	}
	out := []Unshared{}
	for _, dataset := range datasets {
		change := Unshared{Dataset: dataset, Groups: []string{}, Readers: []string{}}
		issuers := make([]string, 0, len(a.data.Groups[dataset]))
		for issuer := range a.data.Groups[dataset] {
			issuers = append(issuers, issuer)
		}
		sort.Strings(issuers)
		for _, issuer := range issuers {
			grant := a.data.Groups[dataset][issuer]
			remaining := without(grant.Types)
			if len(remaining) == len(grant.Types) {
				continue
			}
			change.Groups = append(change.Groups, issuer)
			stopped(dataset, intersect(grant.Types))
			reached := []string{}
			for _, member := range a.members(issuer) {
				if a.data.Principals[member].ViaGroup[dataset] == issuer {
					reached = append(reached, member)
				}
			}
			change.Readers = append(change.Readers, reached...)
			if dryRun {
				continue
			}
			if len(remaining) == 0 {
				delete(a.data.Groups[dataset], issuer)
				for _, member := range reached {
					if err := a.leaveGroup(ctx, a.data.Principals[member], dataset); err != nil {
						return nil, err
					}
				}
				continue
			}
			grant.Types = remaining
			a.data.Groups[dataset][issuer] = grant
			for _, member := range reached {
				if err := a.applyGroups(ctx, member); err != nil {
					return nil, err
				}
			}
		}
		if len(a.data.Groups[dataset]) == 0 {
			delete(a.data.Groups, dataset)
		}
		ids := make([]string, 0, len(a.data.Principals))
		for id := range a.data.Principals {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			principal := a.data.Principals[id]
			held, ok := principal.Datasets[dataset]
			if !ok || !held.Read || held.Write || held.Schema || held.Erase || principal.ViaGroup[dataset] != "" {
				continue
			}
			shared, scoped := principal.SharedTypes[dataset]
			if !scoped {
				change.WholeDatasetReaders = append(change.WholeDatasetReaders, id)
				continue
			}
			remaining := without(shared)
			if len(remaining) == len(shared) {
				continue
			}
			change.Readers = append(change.Readers, id)
			stopped(dataset, intersect(shared))
			if dryRun {
				continue
			}
			if len(remaining) > 0 {
				principal.SharedTypes = withEntry(principal.SharedTypes, dataset, remaining)
				a.data.Principals[id] = principal
				if err := a.reprovisionGuest(ctx, principal, dataset); err != nil {
					return nil, err
				}
				continue
			}
			if held.Insert {
				held.Read = false
				principal = withDataset(principal, dataset, held)
				delete(principal.SharedTypes, dataset)
				a.data.Principals[id] = principal
				if err := a.reprovisionGuest(ctx, principal, dataset); err != nil {
					return nil, err
				}
				continue
			}
			if err := a.endGuestUnsaved(ctx, principal, dataset); err != nil {
				return nil, err
			}
		}
		sort.Strings(change.Readers)
		if len(change.Groups) > 0 || len(change.Readers) > 0 || len(change.WholeDatasetReaders) > 0 {
			out = append(out, change)
		}
	}
	if !dryRun {
		if err := a.save(); err != nil {
			return nil, err
		}
	}
	return out, nil
}
