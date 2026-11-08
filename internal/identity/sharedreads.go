package identity

import (
	"context"
	"sort"
	"time"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// Shared reads: one installation that reads everything shared with its
// account, instead of one sign-in per person who shares.
//
// It reaches every dataset where the account is a reader — holding read and
// neither write, schema nor erase there, so never a dataset it writes or
// administers, nor one it only adds to — within what each share names: its
// types and its expiry. It reads and
// does nothing else. It grows when somebody shares with the account and
// shrinks when a share ends, which is the one thing about it the approval has
// to say, because every other approval covers what existed when it was given.

// sharedWith names the datasets an account reads as a guest, with the view and
// grant an installation reading them gets. The caller holds the lock.
func sharedWith(principal Principal) map[string]store.Authorization {
	shared := map[string]store.Authorization{}
	for dataset, held := range principal.Datasets {
		if !held.Read || held.Write || held.Schema || held.Erase {
			continue
		}
		view := store.FullView()
		if types, scoped := principal.SharedTypes[dataset]; scoped {
			view = store.SharedView(types)
		}
		grants := store.Grants{Read: true}
		if held.ExpiresAt != nil {
			when := *held.ExpiresAt
			grants.ExpiresAt = &when
		}
		shared[dataset] = store.Authorization{Grants: grants, View: view.BeforeSequence(readerFreeze(principal, dataset).Cut)}
	}
	return shared
}

// syncSharedReads brings every live shared-reads installation of the account
// to exactly what is shared with it now: access installed on each dataset
// shared with it, and removed from every one no longer shared. Called
// wherever an account's standing as a reader changes, and on every token such
// an installation is issued, so a share made by some route that did not call
// it is picked up by the next refresh. Removal is never left to that: every
// route that ends a share calls this. The caller holds the lock.
func (a *Authority) syncSharedReads(ctx context.Context, principalID string) error {
	principal, ok := a.data.Principals[principalID]
	if !ok {
		return nil
	}
	// First-party website access follows current account authority as well.
	for _, auth := range a.data.Authorizations {
		if auth.Principal == principalID && auth.Console != nil && !auth.Revoked {
			if _, err := a.syncConsole(ctx, auth); err != nil && p.ErrorCode(err) != "authentication_failed" {
				return err
			}
		}
	}
	var shared map[string]store.Authorization
	for authID, auth := range a.data.Authorizations {
		if auth.Principal != principalID || auth.Revoked || !auth.SharedReads {
			continue
		}
		if shared == nil {
			shared = sharedWith(principal)
		}
		access := store.Access{Principal: principalID, Authorization: authID}
		// The installation's own dataset is held by its own approval, and a
		// share of it changes nothing here: this reaches what is shared and
		// leaves the rest alone.
		for _, dataset := range auth.SharedDatasets {
			if _, still := shared[dataset]; !still && dataset != auth.Dataset {
				if err := a.engine.Revoke(ctx, dataset, access); err != nil {
					return err
				}
			}
		}
		datasets := make([]string, 0, len(shared))
		for dataset := range shared {
			if dataset != auth.Dataset {
				datasets = append(datasets, dataset)
			}
		}
		sort.Strings(datasets)
		installed := make([]string, 0, len(datasets))
		for _, dataset := range datasets {
			err := a.engine.SetAuthorization(ctx, dataset, access, shared[dataset])
			if err != nil && p.ErrorCode(err) != "not_found" {
				return err
			}
			if err == nil {
				installed = append(installed, dataset)
			}
		}
		auth.SharedDatasets = installed
		a.data.Authorizations[authID] = auth
	}
	return nil
}

// newSharedReadsAuthorization is an installation approved to read everything
// shared with the account. It names no dataset and carries read alone. The
// caller holds the lock.
func (a *Authority) newSharedReadsAuthorization(ctx context.Context, principal Principal, client string) (Authorization, error) {
	if err := a.makeRoomForAuthorization(ctx, principal); err != nil {
		return Authorization{}, err
	}
	approved := store.Grants{Read: true}
	auth := Authorization{ID: random(), Principal: principal.ID, Client: client, Grants: store.Grants{Read: true},
		SharedReads: true, Approved: &approved, Created: time.Now().UnixNano()}
	a.data.Authorizations[auth.ID] = auth
	return auth, a.syncSharedReads(ctx, principal.ID)
}
