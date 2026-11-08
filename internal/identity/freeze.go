package identity

import (
	"context"
	"strings"
	"time"

	"github.com/ekarpov/tessera/pkg/store"
)

// ReaderFreeze is a share's sequence cut and the attribution shown to its
// owner. The time describes the action; only Cut decides record visibility.
type ReaderFreeze struct {
	Cut uint64
	At  string
	By  string
}

// readerFreeze is the intersection of the named share and inherited group
// policy. Attribution follows the cut that currently narrows the view most.
func readerFreeze(principal Principal, dataset string) ReaderFreeze {
	direct, group := principal.FrozenReaders[dataset], principal.GroupFreezes[dataset]
	if direct.Cut == 0 || (group.Cut != 0 && group.Cut < direct.Cut) {
		return group
	}
	return direct
}

func withFreeze(old map[string]ReaderFreeze, dataset string, freeze ReaderFreeze) map[string]ReaderFreeze {
	out := make(map[string]ReaderFreeze, len(old)+1)
	for d, f := range old {
		out[d] = f
	}
	if freeze.Cut == 0 {
		delete(out, dataset)
	} else {
		out[dataset] = freeze
	}
	return out
}

// writesDataset checks held grants, which do not establish ownership.
func (a *Authority) writesDataset(account, dataset string) bool {
	principal := a.data.Principals[account]
	held := principal.Datasets[dataset]
	return !principal.Suspended && (principal.Kind == "person" || principal.Kind == "service") && held.Current() && held.Write
}

func (a *Authority) writeInstallation(ctx context.Context, access store.Access, dataset string) bool {
	auth, ok := a.data.Authorizations[access.Authorization]
	if !ok || auth.Revoked || auth.Principal != access.Principal || !a.writesDataset(access.Principal, dataset) {
		return false
	}
	if auth.Dataset == dataset {
		return auth.Grants.Current() && auth.Grants.Write
	}
	// Creation can give an installation a dataset beyond its original approval.
	if !auth.CreateDataset {
		return false
	}
	engine, ok := a.engine.(interface {
		Info(context.Context, store.Access, string) (store.Info, error)
	})
	if !ok {
		return false
	}
	info, err := engine.Info(ctx, access, dataset)
	return err == nil && info.Access.Current() && info.Access.Write
}

// ownerInstallation requires the account recorded at creation. An older or
// operator-created dataset has no such evidence: administrative standing must
// never be sufficient to restore a withdrawn share, even with write approval.
func (a *Authority) ownerInstallation(ctx context.Context, access store.Access, dataset string) bool {
	if !a.writeInstallation(ctx, access, dataset) {
		return false
	}
	if owner := a.data.Owners[dataset]; owner != "" {
		return owner == access.Principal
	}
	return !a.administersDataset(access.Principal, dataset)
}

// ReaderStanding checks the ordinary installation, rather than widening a
// write approval into the administration right.
func (a *Authority) ReaderStanding(ctx context.Context, access store.Access, dataset string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.writeInstallation(ctx, access, dataset) {
		return store.Refuse("permission_denied")
	}
	return nil
}

// FreezeReader narrows a named or group share. Administrators may freeze;
// restoring access requires the dataset owner's ordinary write installation.
// Repeated freeze preserves the original cut, while an unfrozen unfreeze is
// refused so the caller cannot mistake it for a changed share.
func (a *Authority) FreezeReader(ctx context.Context, access store.Access, dataset, account string, unfreeze, administration bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	owner := a.ownerInstallation(ctx, access, dataset)
	if !owner && (unfreeze || !administration || !a.administersDataset(access.Principal, dataset)) {
		return store.Refuse("permission_denied")
	}
	issuer, group := strings.CutPrefix(account, "issuer:")
	var prior ReaderFreeze
	principal := a.data.Principals[account]
	if group {
		grant, ok := a.data.Groups[dataset][issuer]
		if !ok {
			return store.Refuse("not_found")
		}
		prior = grant.Freeze
	} else {
		held, ok := principal.Datasets[dataset]
		if !ok || !held.Read || held.Write || held.Schema || held.Erase {
			return store.Refuse("not_found")
		}
		if principal.ViaGroup[dataset] != "" {
			return store.RefuseWith("conflict", "group_reader", nil)
		}
		prior = principal.FrozenReaders[dataset]
	}
	if unfreeze && prior.Cut == 0 {
		return store.Refuse("conflict")
	}
	if !unfreeze && prior.Cut != 0 {
		return nil
	}
	freeze := ReaderFreeze{}
	if !unfreeze {
		engine, ok := a.engine.(interface {
			ReaderCut(context.Context, string) (uint64, error)
		})
		if !ok {
			return store.Refuse("not_supported")
		}
		cut, err := engine.ReaderCut(ctx, dataset)
		if err != nil {
			return err
		}
		freeze = ReaderFreeze{Cut: cut, At: time.Now().UTC().Format(time.RFC3339Nano), By: access.Principal}
	}
	if group {
		if a.data.FrozenGroups == nil {
			a.data.FrozenGroups = map[string]map[string]ReaderFreeze{}
		}
		if a.data.FrozenGroups[dataset] == nil {
			a.data.FrozenGroups[dataset] = map[string]ReaderFreeze{}
		}
		if freeze.Cut == 0 {
			delete(a.data.FrozenGroups[dataset], issuer)
		} else {
			a.data.FrozenGroups[dataset][issuer] = freeze
		}
		grant := a.data.Groups[dataset][issuer]
		grant.Freeze = freeze
		a.data.Groups[dataset][issuer] = grant
		for _, member := range a.members(issuer) {
			if err := a.applyGroups(ctx, member); err != nil {
				return err
			}
		}
	} else {
		principal.FrozenReaders = withFreeze(principal.FrozenReaders, dataset, freeze)
		a.data.Principals[account] = principal
		if err := a.reprovisionGuest(ctx, principal, dataset); err != nil {
			return err
		}
	}
	return a.save()
}
