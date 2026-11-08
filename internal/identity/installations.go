package identity

import (
	"context"
	"net/http"
	"sort"
	"time"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// Installations are an account's signed-in devices and applications, each
// one authorization. A person lists and removes their own; an administrator
// lists and removes those of an account they may administer.
//
// Removing one is complete and immediate: its access goes from every dataset,
// its access token stops at the next request, nothing renews it, and no record
// of it stays behind.

// Installation describes one live installation to whoever may remove it.
type Installation struct {
	ID            string    `json:"id"`
	ClientName    string    `json:"client_name"`
	Dataset       string    `json:"dataset_id,omitempty"`
	Created       time.Time `json:"created_at"`
	Current       bool      `json:"current,omitempty"`
	Administer    bool      `json:"administer,omitempty"`
	CreateDataset bool      `json:"create_dataset,omitempty"`
	SharedReads   bool      `json:"shared_reads,omitempty"`
}

// installationsOf lists an account's live installations, newest first. The
// caller holds the lock.
func (a *Authority) installationsOf(account, current string) []Installation {
	out := []Installation{}
	for _, auth := range a.data.Authorizations {
		if auth.Principal != account || auth.Revoked {
			continue
		}
		out = append(out, Installation{ID: auth.ID, ClientName: a.data.Clients[auth.Client].Name, Dataset: auth.Dataset,
			Created: time.Unix(0, auth.Created).UTC(), Current: auth.ID == current, Administer: auth.Administer,
			CreateDataset: auth.CreateDataset, SharedReads: auth.SharedReads})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Created.Equal(out[j].Created) {
			return out[i].Created.After(out[j].Created)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// removeInstallation takes an installation's access off every dataset and
// forgets it: its record, its tokens and any code it was owed. The caller
// holds the lock and saves.
func (a *Authority) removeInstallation(ctx context.Context, id string) error {
	auth, ok := a.data.Authorizations[id]
	if !ok {
		return store.Refuse("not_found")
	}
	if err := a.revokeEverywhere(ctx, auth); err != nil {
		return err
	}
	// A confidential client's secret would mint another installation at
	// once, so removing its installation withdraws it, as revoking does.
	if client := a.data.Clients[auth.Client]; client.Secret != "" {
		client.Disabled = true
		a.data.Clients[client.ID] = client
	}
	for key, token := range a.data.Tokens {
		if token.Authorization == id {
			delete(a.data.Tokens, key)
		}
	}
	for key, code := range a.data.Codes {
		if code.Authorization == id {
			delete(a.data.Codes, key)
		}
	}
	delete(a.data.Authorizations, id)
	return nil
}

// OwnInstallations lists the installations of the account behind access.
func (a *Authority) OwnInstallations(access store.Access) []Installation {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.installationsOf(access.Principal, access.Authorization)
}

// RemoveOwnInstallation removes one of the caller's own installations, this
// one included.
func (a *Authority) RemoveOwnInstallation(ctx context.Context, access store.Access, id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	auth, ok := a.data.Authorizations[id]
	if !ok || auth.Revoked || auth.Principal != access.Principal {
		return store.Refuse("not_found")
	}
	if err := a.removeInstallation(ctx, id); err != nil {
		return err
	}
	return a.save()
}

// AccountInstallations lists an account's installations for an administrator
// who may administer it.
func (a *Authority) AccountInstallations(administrator, account string) ([]Installation, error) {
	if !a.MayAdministerAccount(administrator, account) {
		return nil, store.Refuse("permission_denied")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.installationsOf(account, ""), nil
}

// RemoveAccountInstallation removes one installation of an account an
// administrator may administer.
func (a *Authority) RemoveAccountInstallation(ctx context.Context, administrator, account, id string) error {
	if !a.MayAdministerAccount(administrator, account) {
		return store.Refuse("permission_denied")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	auth, ok := a.data.Authorizations[id]
	if !ok || auth.Revoked || auth.Principal != account {
		return store.Refuse("not_found")
	}
	if err := a.removeInstallation(ctx, id); err != nil {
		return err
	}
	return a.save()
}

// ownInstallations answers GET and DELETE /oauth/account/installations.
func (a *Authority) ownInstallations(w http.ResponseWriter, r *http.Request) {
	access, ok := a.bearerAccess(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, 200, map[string]any{"installations": a.OwnInstallations(access)})
		return
	}
	err := a.RemoveOwnInstallation(r.Context(), access, r.PathValue("installation"))
	switch code := p.ErrorCode(err); {
	case err == nil:
		w.WriteHeader(204)
	case code == "not_found":
		oauthError(w, 404, "not_found")
	default:
		oauthError(w, 503, "temporarily_unavailable")
	}
}
