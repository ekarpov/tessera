package identity

import (
	"context"
	"slices"
)

// ReconcileDatasets removes authority over identities absent from the complete
// storage inventory. The standalone composition calls this before serving,
// including after restore has reapplied newer erasures to an older archive.
// A failed inventory or persistence step must prevent startup. Replacements
// receive no grants, ownership or issuer policy from the archived identities.
func (a *Authority) ReconcileDatasets(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	datasets, err := a.engine.Provisioned(ctx)
	if err != nil {
		return err
	}
	active := make(map[string]bool, len(datasets))
	for _, d := range datasets {
		active[d.ID] = true
	}
	changed := false
	for id, principal := range a.data.Principals {
		changed = pruneDatasets(principal.Datasets, active) || changed
		changed = pruneDatasets(principal.SharedTypes, active) || changed
		changed = pruneDatasets(principal.InsertTypes, active) || changed
		changed = pruneDatasets(principal.FrozenReaders, active) || changed
		changed = pruneDatasets(principal.GroupFreezes, active) || changed
		changed = pruneDatasets(principal.ViaGroup, active) || changed
		kept := slices.DeleteFunc(principal.ErasedWith, func(d string) bool { return !active[d] })
		changed = len(kept) != len(principal.ErasedWith) || changed
		principal.ErasedWith = kept
		a.data.Principals[id] = principal
	}
	for id, auth := range a.data.Authorizations {
		if auth.Dataset != "" && !active[auth.Dataset] && !auth.Revoked {
			auth.Revoked = true
			changed = true
		}
		kept := slices.DeleteFunc(auth.SharedDatasets, func(d string) bool { return !active[d] })
		changed = len(kept) != len(auth.SharedDatasets) || changed
		auth.SharedDatasets = kept
		a.data.Authorizations[id] = auth
	}
	// A fixed-policy issuer must not be narrowed or redirected to a replacement.
	// Disable the whole issuer if any of its named datasets is gone. Its policy
	// and issued-count history remain intact; outstanding invitations are refused.
	for id, issuer := range a.data.Issuers {
		if issuer.Disabled {
			continue
		}
		for _, dataset := range issuer.Datasets {
			if !active[dataset] {
				issuer.Disabled = true
				a.data.Issuers[id] = issuer
				changed = true
				break
			}
		}
	}
	for id, code := range a.data.DeviceCodes {
		if code.Dataset != "" && !active[code.Dataset] {
			delete(a.data.DeviceCodes, id)
			changed = true
		}
	}
	changed = pruneDatasets(a.data.Groups, active) || changed
	changed = pruneDatasets(a.data.FrozenGroups, active) || changed
	changed = pruneDatasets(a.data.Stopped, active) || changed
	changed = pruneDatasets(a.data.Owners, active) || changed
	changed = pruneDatasets(a.data.MadeBy, active) || changed
	if !changed {
		return nil
	}
	return a.save()
}

func pruneDatasets[T any](values map[string]T, active map[string]bool) bool {
	changed := false
	for id := range values {
		if !active[id] {
			delete(values, id)
			changed = true
		}
	}
	return changed
}
