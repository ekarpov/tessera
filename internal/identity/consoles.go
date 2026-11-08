package identity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// ConsoleConfig is an operator-provisioned website identity and an authority
// ceiling, separate from branding and public OAuth registration. No role names
// or client vocabulary are interpreted by Tessera.
type ConsoleConfig struct {
	ClientID    string   `json:"client_id"`
	Name        string   `json:"name"`
	Origin      string   `json:"origin"`
	RedirectURI string   `json:"redirect_uri"`
	DataGrants  []string `json:"data_grants"`
	Administer  bool     `json:"administer"`
}

type consoleBinding struct {
	// Applied records the grant/view fingerprints installed by this policy.
	// Reapplying unchanged grants would invalidate the engine cursor generation.
	Applied          map[string]string `json:",omitempty"`
	Withdrawn        bool              `json:",omitempty"`
	Origin, Redirect string
}

type consolePolicy struct {
	config ConsoleConfig
	grants store.Grants
}

// SetConsoles provisions reserved public PKCE identities. Existing dynamically
// registered clients cannot be promoted. Configure before serving requests.
func (a *Authority) SetConsoles(configs []ConsoleConfig) error {
	if len(configs) > 16 {
		return errors.New("first_party_consoles supports at most 16 identities")
	}
	policies := map[string]consolePolicy{}
	for _, c := range configs {
		origin, e := url.Parse(c.Origin)
		redirect, re := url.Parse(c.RedirectURI)
		if !brandText(c.ClientID) || strings.ContainsFunc(c.ClientID, unicode.IsSpace) || !brandText(c.Name) || len(c.RedirectURI) > 2048 || e != nil || re != nil || origin.Scheme != "https" || origin.Host == "" || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.ForceQuery || origin.Fragment != "" || origin.String() != c.Origin || redirect.Scheme != "https" || redirect.Host != origin.Host || redirect.User != nil || redirect.Fragment != "" || !strings.HasPrefix(redirect.Path, "/") || redirect.String() != c.RedirectURI {
			return errors.New("first_party_consoles requires a client ID, name, exact HTTPS origin and absolute callback on that origin")
		}
		if _, exists := policies[c.ClientID]; exists {
			return errors.New("duplicate first_party_consoles client_id")
		}
		g := store.Grants{}
		if len(c.DataGrants) > 3 {
			return errors.New("first_party_consoles data_grants may contain read, write and insert only")
		}
		seen := map[string]bool{}
		for _, name := range c.DataGrants {
			if seen[name] {
				return errors.New("duplicate first_party_consoles data_grant")
			}
			seen[name] = true
			switch name {
			case "read":
				g.Read = true
			case "write":
				g.Write = true
			case "insert":
				g.Insert = true
			default:
				return errors.New("first_party_consoles data_grants may contain read, write and insert only")
			}
		}
		c.DataGrants = append([]string(nil), c.DataGrants...)
		policies[c.ClientID] = consolePolicy{config: c, grants: g}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for id := range policies {
		if c, exists := a.data.Clients[id]; exists && !c.OperatorConsole {
			return errors.New("first_party_consoles cannot promote an existing OAuth client")
		}
	}
	for id, policy := range policies {
		a.data.Clients[id] = Client{ID: id, Name: policy.config.Name, Redirects: []string{policy.config.RedirectURI}, OperatorConsole: true}
	}
	if len(policies) > 0 {
		if err := a.save(); err != nil {
			return err
		}
	}
	a.consoles.Store(&policies)
	return nil
}

func (a *Authority) console(id, redirect string) (consolePolicy, bool) {
	policies := a.consoles.Load()
	if policies == nil {
		return consolePolicy{}, false
	}
	c, ok := (*policies)[id]
	return c, ok && c.config.RedirectURI == redirect
}

// syncConsole uses only current account grants and existing view restrictions.
// It runs on every bearer authentication and token issue, including refresh.
// Its reach is kept separately from ordinary dataset approvals and shared reads.
func (a *Authority) syncConsole(ctx context.Context, auth Authorization) (Authorization, error) {
	if auth.Console == nil {
		return auth, nil
	}
	policy, ok := a.console(auth.Client, auth.Console.Redirect)
	client := a.data.Clients[auth.Client]
	if !ok || policy.config.Origin != auth.Console.Origin || !client.OperatorConsole || client.Disabled || len(client.Redirects) != 1 || client.Redirects[0] != policy.config.RedirectURI {
		if err := a.revokeEverywhere(ctx, auth); err != nil {
			return auth, err
		}
		auth.Administer = false
		a.data.Authorizations[auth.ID] = auth
		return auth, store.Refuse("authentication_failed")
	}
	principal := a.data.Principals[auth.Principal]
	access := store.Access{Principal: auth.Principal, Authorization: auth.ID}
	reached, err := a.engine.Reach(ctx, access)
	if err != nil {
		return auth, err
	}
	existing := map[string]bool{}
	for _, id := range reached {
		existing[id] = true
	}
	applied := map[string]string{}
	current := map[string]bool{}
	ids := make([]string, 0, len(principal.Datasets))
	for id := range principal.Datasets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if auth.Console.Withdrawn {
			break
		}
		held := principal.Datasets[id]
		if !held.Current() {
			continue
		}
		g := store.Grants{Read: held.Read && policy.grants.Read, Write: held.Write && policy.grants.Write, Insert: held.Insert && policy.grants.Insert, ExpiresAt: held.ExpiresAt}
		view, guest := guestView(principal, id)
		if guest {
			g.Write = false
		}
		if !g.Read && !g.Write && !g.Insert {
			continue
		}
		desired := store.Authorization{Grants: g, View: view}
		raw, err := json.Marshal(desired)
		if err != nil {
			return auth, err
		}
		sum := sha256.Sum256(raw)
		fingerprint := hex.EncodeToString(sum[:])
		if !existing[id] || auth.Console.Applied[id] != fingerprint {
			if err := a.engine.SetAuthorization(ctx, id, access, desired); err != nil {
				if p.ErrorCode(err) == "not_found" {
					continue
				}
				return auth, err
			}
		}
		applied[id] = fingerprint
		current[id] = true
	}
	// Consult the engine's actual reach: configuration, withdrawal or a failed
	// previous request may have changed it without a registry save.
	for _, id := range reached {
		if !current[id] {
			if err := a.engine.Revoke(ctx, id, access); err != nil {
				return auth, err
			}
		}
	}
	binding := *auth.Console
	binding.Applied = applied
	auth.Console = &binding
	auth.Administer = !auth.Console.Withdrawn && policy.config.Administer && (principal.MayAdministerServer || len(administered(principal)) > 0)
	auth.CreateDataset = false
	a.data.Authorizations[auth.ID] = auth
	return auth, nil
}

func (a *Authority) newConsoleAuthorization(ctx context.Context, principal Principal, f flow) (Authorization, error) {
	policy, ok := a.console(f.Client, f.Redirect)
	if !ok {
		return Authorization{}, store.Refuse("permission_denied")
	}
	if err := a.makeRoomForAuthorization(ctx, principal); err != nil {
		return Authorization{}, err
	}
	auth := Authorization{ID: random(), Principal: principal.ID, Client: f.Client, Console: &consoleBinding{Origin: policy.config.Origin, Redirect: f.Redirect}, Created: time.Now().UnixNano()}
	a.data.Authorizations[auth.ID] = auth
	return a.syncConsole(ctx, auth)
}
