package identity

import (
	"context"
	"strings"

	"github.com/ekarpov/tessera/pkg/store"
)

func containsGrants(held, want store.Grants) bool {
	return held.Current() && (!want.Read || held.Read) && (!want.Write || held.Write) && (!want.Insert || held.Insert) && (!want.Schema || held.Schema) && (!want.Erase || held.Erase)
}

// reuseConsent is called only after fresh password verification, while holding
// mu. It never combines multiple approvals or reuses a withdrawn/revoked one.
// A new, narrower installation is issued, not the previous broader credential.
func (a *Authority) reuseConsent(ctx context.Context, f flow, principal Principal, offered []datasetOption, id string) (Authorization, bool, error) {
	if f.ForceConsent {
		return Authorization{}, false, nil
	}
	var prior Authorization
	for _, candidate := range a.data.Authorizations {
		if candidate.Console != nil || candidate.Revoked || candidate.Principal != principal.ID || candidate.Client != f.Client || candidate.ConsentRedirect != f.Redirect || f.Dataset != "" && candidate.Dataset != f.Dataset {
			continue
		}
		if prior.ID == "" || candidate.Created > prior.Created || candidate.Created == prior.Created && candidate.ID > prior.ID {
			prior = candidate
		}
	}
	if prior.ID == "" {
		return Authorization{}, false, nil
	}
	dataset := f.Dataset
	if dataset == "" {
		dataset = prior.Dataset
		if dataset != "" {
			offeredBefore := false
			for _, choice := range offered {
				offeredBefore = offeredBefore || choice.ID == dataset
			}
			if !offeredBefore {
				return Authorization{}, false, nil
			}
		}
	}
	held := principal.Datasets[dataset]
	// Omitted scope continues the previous safe data scope, without gaining
	// newly held rights. An explicit scope (including empty) replaces it.
	wanted := store.Grants{Read: prior.Grants.Read && held.Read, Write: prior.Grants.Write && held.Write, Insert: prior.Grants.Insert && held.Insert}
	administer, create, shared := false, false, f.SharedReads
	if f.HasScope {
		wanted = store.Grants{}
		for _, name := range strings.Split(f.Scope, " ") {
			switch name {
			case "read":
				wanted.Read = true
			case "write":
				wanted.Write = true
			case "insert":
				wanted.Insert = true
			case "schema":
				wanted.Schema = true
			case "erase":
				wanted.Erase = true
			case "administer":
				administer = true
			case "create_dataset":
				create = true
			case "shared_reads":
				if !f.SharedReads {
					return Authorization{}, false, nil
				}
				shared = true
			}
		}
	}
	if dataset != "" && !containsGrants(held, wanted) {
		return Authorization{}, false, nil
	}
	if dataset == "" && (wanted.Read || wanted.Write || wanted.Insert || wanted.Schema || wanted.Erase) {
		return Authorization{}, false, nil
	}
	if administer && !(principal.MayAdministerServer || len(administered(principal)) > 0) || create && !principal.MayCreateDataset {
		return Authorization{}, false, nil
	}
	if !(wanted.Read || wanted.Write || wanted.Insert || wanted.Schema || wanted.Erase || administer || create || shared) {
		return Authorization{}, false, nil
	}
	if !containsGrants(prior.Grants, wanted) || administer && !prior.Administer || create && !prior.CreateDataset || shared && !prior.SharedReads {
		return Authorization{}, false, nil
	}

	// The sign-in submission is spent before any setup can have a side effect.
	delete(a.flows, id)
	var auth Authorization
	var err error
	if dataset == "" && shared && !create && !administer {
		auth, err = a.newSharedReadsAuthorization(ctx, principal, f.Client)
	} else {
		auth, err = a.newAuthorization(ctx, principal, f.Client, dataset, wanted, create, administer)
		if err == nil && administer {
			auth.Administer = true
			a.data.Authorizations[auth.ID] = auth
		}
		if err == nil && shared {
			auth.SharedReads = true
			a.data.Authorizations[auth.ID] = auth
			err = a.syncSharedReads(ctx, principal.ID)
		}
	}
	return auth, true, err
}
