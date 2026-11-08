// Package identity implements the standalone authorization server. Authentication
// is separate from the dataset privileges installed at authorization creation.
package identity

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// Engine is the grant administration this authority drives. A grant belongs to
// one dataset, so installing or revoking one names it.
type Engine interface {
	SetAuthorization(context.Context, string, store.Access, store.Authorization) error
	Revoke(context.Context, string, store.Access) error
	// Provisioned lists what the storage holds.
	Provisioned(context.Context) ([]store.Dataset, error)
	// Reach names the datasets one authorization holds access on. That is not
	// the dataset its approval named: creating a dataset installs access to
	// it for whoever created it, on a dataset this registry never hears
	// about. Both taking the access away and deciding who may take it away
	// are questions about the reach rather than about the approval.
	Reach(context.Context, store.Access) ([]string, error)
}

// Principal is a person or service. Grants belong to a dataset, so a principal
// holds a set of them: one row per dataset it may reach, which is how one person
// keeps a dataset of their own and shares another.
type Principal struct {
	ID, Kind, Salt, Password string
	// Datasets maps a dataset identity to what this principal may do there.
	// A principal absent from a dataset holds nothing on it, which is the same
	// answer as holding a grant with nothing set.
	Datasets map[string]store.Grants
	// SharedTypes is the concrete types this principal may read on a dataset
	// it holds as a guest, when read access was shared by type. Absent means
	// the whole dataset.
	//
	// It lives on the account rather than on one authorization because that is
	// what a share means to the person making it. It used to be set on the one
	// authorization the share named, while every authorization minted later
	// came from the account's grant with a full view — so a guest's next
	// sign-in, a new token or an approval on another device, read everything
	// the share was meant to withhold. Review finding 29.
	SharedTypes map[string][]string `json:",omitempty"`
	// FrozenReaders stays with the account when a share is replaced or removed,
	// so administering a share cannot silently restore its withdrawn access.
	FrozenReaders map[string]ReaderFreeze `json:",omitempty"`
	// GroupFreezes carries inherited policy apart from a named share's own cut.
	// Removing one policy must not clear the other.
	GroupFreezes map[string]ReaderFreeze `json:",omitempty"`
	// InsertTypes is the concrete types this principal may add records of on
	// a dataset where it contributes. Held on the account for the reason
	// SharedTypes is, and apart from it: what a contributor adds to and what
	// it may see are separate decisions, and neither implies the other.
	InsertTypes map[string][]string `json:",omitempty"`
	// Suspended stops every credential this account holds from authenticating,
	// at once and without discarding any of them, which is what makes it
	// reversible. It is for an operator dealing with abuse or a lost device:
	// revoking each installation instead is permanent and one at a time.
	Suspended bool `json:",omitempty"`
	// MayCreateDataset is authority over the server rather than over any
	// dataset, so it is held here and not derived from a grant. Deriving it
	// from erase — as this once did — hid a policy inside an accident and made
	// self-service impossible: a person with no dataset could never qualify,
	// and neither could any credential an operator can mint.
	//
	// It survives its holder gaining a dataset, because a server owner has one
	// by definition and must still be able to make more.
	MayCreateDataset bool
	// MayAdministerServer is the right to act on what belongs to the server
	// rather than to a dataset — an approval carrying a server right and no
	// dataset grant, which nobody could administer before this existed.
	//
	// Held explicitly and never derived. Deriving administration from holding
	// erase somewhere made every account that owned a dataset an
	// administrator of everybody else's, and deriving standing over a
	// server-level approval from a shared membership made it available to
	// anyone who could arrange the membership — which, since adding an
	// existing account to a dataset needs no password, was anyone. Both were
	// escalations and both were a policy hidden inside an accident.
	MayAdministerServer bool
	// PasswordSetBy names who set the current password when it was not the
	// person: an administrator's account, or "operator" for the command line.
	// Whoever sets a password can sign in with it, so the person is told —
	// through /oauth/account — until they choose one of their own.
	PasswordSetBy string `json:",omitempty"`
	PasswordSetAt int64  `json:",omitempty"`
	// ErasedWith names datasets erased when this account is deleted, whoever
	// else administers them: a template marked erase_with_account made them
	// for this person. Deletion still erases only what its caller names.
	ErasedWith []string `json:",omitempty"`
	// Recovery is the digest of the person's recovery code. See recovery.go.
	Recovery string `json:",omitempty"`
	// CreatedAt is when the account was made, in Unix seconds; zero for one
	// made before it was recorded. SuspendedAt is when it was suspended, zero
	// while it is not — or for a suspension from before it was recorded.
	CreatedAt   int64 `json:",omitempty"`
	SuspendedAt int64 `json:",omitempty"`
	// InvitedBy is the issuer whose invitation made this account, which is
	// what makes it a member of that issuer's group. Empty for an account
	// made any other way, and for one made before it was recorded.
	InvitedBy string `json:",omitempty"`
	// ViaGroup names, per dataset, the issuer whose group grant this
	// account reads it through. A dataset absent here and present in
	// Datasets is held some other way, which a group grant never overrides.
	ViaGroup map[string]string `json:",omitempty"`
	// DeviceJoined marks an account made by redeeming an invitation on a
	// device, with no password. Rebound records the last time its issuer
	// reconnected it to a new device. See devices.go.
	DeviceJoined bool       `json:",omitempty"`
	Rebound      *Rebinding `json:",omitempty"`
}

// Rebinding is when, and by which issuer, an account was reconnected to a new
// device. The account's own installations can read it.
type Rebinding struct {
	At     int64
	Issuer string
}
type Client struct {
	Logo                        []byte `json:",omitempty"`
	OperatorConsole             bool   `json:",omitempty"`
	Disabled                    bool
	ID, Name, Secret, Principal string
	Redirects                   []string
	// Registered is when a client registered itself, in Unix seconds. Zero
	// for a client provisioned by the operator.
	Registered int64 `json:",omitempty"`
}

// Authorization is a separately revocable installation. Ordinary OAuth names
// its approved dataset; an operator-provisioned Console instead carries an
// exact website binding and follows its current-account policy.
type Authorization struct {
	ConsentRedirect                string          `json:",omitempty"`
	Console                        *consoleBinding `json:",omitempty"`
	ID, Principal, Client, Dataset string
	Grants                         store.Grants
	// CreateDataset is the server right this installation was approved for. It
	// is independent of Dataset and of Grants: an authorization may carry it
	// with no dataset at all, which is the case a caller needs when it holds
	// nothing yet and is about to make something.
	CreateDataset bool
	// Administer lets this installation act through the administration API
	// as the account that approved it, within exactly what that account
	// administers. It is its own right, asked for on the consent screen,
	// because reusing an existing one would change what a past approval
	// meant: a dashboard approved to read, or even to erase, did not consent
	// to adding people or deleting accounts.
	Administer bool `json:",omitempty"`
	// Approved is what the installation was approved for, before it was
	// intersected with what the account held. Kept so that a change to the
	// account's standing later gives an installation no right it was never
	// approved for. Nil for one approved before it was recorded.
	Approved *store.Grants `json:",omitempty"`
	// SharedReads makes this an installation that reads everything shared
	// with its account rather than one dataset: see sharedreads.go. Dataset
	// is empty and the reach follows the account's shares.
	SharedReads bool `json:",omitempty"`
	// SharedDatasets are the datasets shared reads installed access on, and
	// the only ones it may take access from again: an installation that also
	// holds its own dataset, or created some, reaches those by other rights.
	SharedDatasets []string `json:",omitempty"`
	Revoked        bool
	// Created orders a person's installations, so the oldest can be signed out
	// when they hold too many. Zero for one made before it was recorded, which
	// sorts first.
	Created int64 `json:",omitempty"`
}
type Token struct {
	Authorization, Kind string
	Expires             int64
	Used                bool
}
type Code struct {
	Remember                                   bool `json:",omitempty"`
	Authorization, Client, Redirect, Challenge string
	Expires                                    int64
}
type state struct {
	// Version is the registry shape on disk, so that a field moving between
	// releases is a migration rather than a silent loss. See migrate.go.
	Version        int
	Principals     map[string]Principal
	Clients        map[string]Client
	Authorizations map[string]Authorization
	Tokens         map[string]Token
	Codes          map[string]Code
	// Retired holds the identifiers of deleted accounts, which are never
	// issued again. A receipt answers to its owning principal, so a new
	// account given a deleted one's name could ask about, and read, the
	// outcomes the old account left behind.
	Retired map[string]bool `json:",omitempty"`
	// Issuers may hand out invitations; Invitations are the ones not yet
	// redeemed, keyed by a digest of the code so the registry never holds a
	// code that could be redeemed from it.
	Issuers     map[string]Issuer     `json:",omitempty"`
	Invitations map[string]Invitation `json:",omitempty"`
	// Groups are group reader grants, by dataset and then by issuer: every
	// account that issuer made reads the named types. See groups.go.
	Groups map[string]map[string]GroupGrant `json:",omitempty"`
	// FrozenGroups retains cuts when an administrator removes a group share,
	// because recreating it is not the owner's decision to restore access.
	FrozenGroups map[string]map[string]ReaderFreeze `json:",omitempty"`
	// LastBackupAt is when the last backup succeeded, in Unix seconds, so an
	// administrator can be told after a restart as well as before one.
	LastBackupAt int64 `json:",omitempty"`
	// MadeBy names the issuer whose invitation made each dataset a template
	// created, which is what places it under that issuer's policy.
	MadeBy map[string]string `json:",omitempty"`
	// Owners records the invitee or creating account, independently of grants.
	// Older and operator-created datasets have no recorded owner.
	Owners map[string]string `json:",omitempty"`
	// DeviceCodes and PasswordLinks are single-use, short-lived secrets, by
	// digest. See devices.go.
	DeviceCodes   map[string]DeviceCode   `json:",omitempty"`
	PasswordLinks map[string]PasswordLink `json:",omitempty"`
	// Stopped records, per dataset and type, who last stopped the type being
	// shared from it and when, so the dataset's own account can be told.
	// Sharing the type again clears it. It is a note, not a journal.
	Stopped map[string]map[string]StopRecord `json:",omitempty"`
}

// StopRecord is who stopped a type being shared from a dataset, and when, in
// Unix seconds.
type StopRecord struct {
	By string
	At int64
}
type flow struct {
	ForceConsent                             bool
	Console                                  bool
	Client, Redirect, State, Challenge, CSRF string
	// Dataset is what the application asked to be authorized for, empty when it
	// asked for none. Carried from the request to the approval screen, so a
	// named one is shown rather than chosen.
	Dataset string
	// Scope is a validated, optional request for consent defaults, never authority.
	Scope    string
	HasScope bool
	// SharedReads is a request to read everything shared with the account,
	// instead of a dataset; the two never come together.
	SharedReads bool
	// Principal is empty until the person has signed in, which is what makes
	// this two steps: the account is not known at the moment the first screen
	// is rendered, so the datasets it may reach cannot be offered there.
	Principal string
	Expires   time.Time
}
type Authority struct {
	// provision creates a dataset with types installed and returns its
	// identity, holding no access to it afterwards. Set by the composition,
	// because datasets belong to the storage rather than to this registry.
	provision    func(ctx context.Context, displayName string, types []p.Meta) (string, error)
	erase        func(ctx context.Context, dataset string) error
	mu           sync.Mutex
	path, issuer string
	engine       Engine
	data         state
	flows        map[string]flow
	// Recovery contexts are memory-only, bounded and contain no authorization code.
	recoveries map[string]flow
	windows    map[string]rateWindow
	proxies    []netip.Prefix
	tickets    map[string]deletionTicket
	swept      time.Time
	// derivations bounds how many password derivations run at once. See
	// derive.
	derivations chan struct{}
	// iterations is the derivation cost, fixed when the authority opens.
	// Always ProductionIterations outside a test binary; see derivation.go.
	iterations int
	// displayName is the deployment's name, shown on every page beside the
	// host. Atomic so a page can be built by a handler holding mu.
	hasClientLogos atomic.Bool
	consoles       atomic.Pointer[map[string]consolePolicy]
	displayName    atomic.Pointer[string]
	brand          atomic.Pointer[pageBrand]
}
type rateWindow struct {
	Start time.Time
	Count int
}

// Open reads the private identity store. The authority serves a server rather
// than a dataset: it issues grants over any dataset a principal holds, and every
// approval names the one it is for, because an approval screen that does not say
// which dataset it is for is not consent.
func Open(ctx context.Context, path, issuer string, engine Engine) (*Authority, error) {
	a := &Authority{path: path, issuer: issuer, engine: engine, flows: map[string]flow{}, windows: map[string]rateWindow{}, derivations: make(chan struct{}, derivationSlots()), iterations: derivationCost(), data: state{Version: identityVersion, Principals: map[string]Principal{}, Clients: map[string]Client{}, Authorizations: map[string]Authorization{}, Tokens: map[string]Token{}, Codes: map[string]Code{}}}
	raw, err := os.ReadFile(path)
	if err == nil {
		// Decoding leaves a member the document does not mention untouched, so
		// the version has to be cleared before reading rather than after: a
		// file written before versions existed would otherwise inherit this
		// build's and claim to be a shape it is not.
		a.data.Version = 0
		info, e := os.Stat(path)
		if e != nil || info.Mode().Perm()&0077 != 0 || json.Unmarshal(raw, &a.data) != nil {
			return nil, errors.New("invalid private identity store")
		}
		a.hasClientLogos.Store(a.logoBudget() > 0)
		if err = a.readable(); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, errors.New("cannot read identity store")
	}
	return a, nil
}
func random() string {
	var data [32]byte
	if _, err := rand.Read(data[:]); err != nil {
		panic("secure entropy unavailable")
	}
	return base64.RawURLEncoding.EncodeToString(data[:])
}
func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func (a *Authority) save() error {
	// Older deployments cannot preserve frozen shares or Console bindings.
	a.data.Version = identityVersion
	now := time.Now().Unix()
	for key, value := range a.data.Tokens {
		if value.Expires <= now {
			delete(a.data.Tokens, key)
		}
	}
	for key, value := range a.data.Codes {
		if value.Expires <= now {
			delete(a.data.Codes, key)
		}
	}
	raw, err := json.Marshal(a.data)
	if err != nil {
		return errors.New("cannot encode identity store")
	}
	if err = os.MkdirAll(filepath.Dir(a.path), 0700); err != nil {
		return errors.New("cannot create identity directory")
	}
	file, err := os.CreateTemp(filepath.Dir(a.path), ".identity-")
	if err != nil {
		return errors.New("cannot persist identity store")
	}
	name := file.Name()
	defer os.Remove(name)
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(raw)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return errors.New("cannot persist identity store")
	}
	if err = os.Rename(name, a.path); err != nil {
		return errors.New("cannot replace identity store")
	}
	dir, err := os.Open(filepath.Dir(a.path))
	if err != nil {
		return errors.New("cannot persist identity directory")
	}
	defer dir.Close()
	return dir.Sync()
}

// derivationSlots is how many password derivations may run at once: one per
// processor, and never fewer than two, so one slow derivation never makes the
// next wait for it alone.
func derivationSlots() int {
	return max(2, runtime.NumCPU())
}

// derivationWait is how long a request waits for a free slot before it is
// told to come back. A variable only so a test need not wait ten seconds.
var derivationWait = 10 * time.Second

// derive runs the password derivation within the bound on how many run at
// once.
//
// The derivation is deliberately expensive, and something has to stop a flood
// of attempts from spending every processor on it. That used to be a
// server-wide count of attempts per minute, and a count is a budget: a few
// addresses could spend it and close sign-in for everybody for the minute. A
// bound on concurrency is not a budget. A flood fills the slots and each
// attempt waits its turn, so sign-in slows under attack instead of closing;
// only a wait past derivationWait is refused, and it is refused as busy, to
// be retried, rather than as an answer about the password.
//
// Never call it holding a.mu: the wait, and the derivation, would stall every
// request that checks a bearer token.
func (a *Authority) derive(ctx context.Context, secret, salt string) (string, error) {
	wait := time.NewTimer(derivationWait)
	defer wait.Stop()
	select {
	case a.derivations <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	case <-wait.C:
		return "", store.Refuse("rate_limited")
	}
	defer func() { <-a.derivations }()
	return password(secret, salt, a.iterations), nil
}

func password(secret, salt string, iterations int) string {
	key, err := pbkdf2.Key(sha256.New, secret, []byte(salt), iterations, 32)
	if err != nil {
		return ""
	}
	return hex.EncodeToString(key)
}

// AddPerson is local administration. No client can provision itself an account.
func (a *Authority) AddPerson(id, secret, dataset string, grants store.Grants, mayCreate bool) error {
	return a.addPerson(id, secret, dataset, grants, mayCreate, false)
}

// AddServerAdministrator provisions an account entitled to act on what belongs
// to the server rather than to a dataset. It is separate from AddPerson
// because conferring it is a different decision from adding somebody to a
// dataset, and a flag on the common path is a flag that gets passed by
// accident.
func (a *Authority) AddServerAdministrator(id, secret, dataset string, grants store.Grants, mayCreate bool) error {
	return a.addPerson(id, secret, dataset, grants, mayCreate, true)
}

func (a *Authority) addPerson(id, secret, dataset string, grants store.Grants, mayCreate, mayAdminister bool) error {
	if id == "" || len(id) > 255 || len(secret) < 12 {
		return errors.New("person ID and a password of at least 12 bytes are required")
	}
	// A person is added to a dataset, unless the only thing being given is a
	// server right — which is how an account comes to exist before any
	// dataset does, how an operator provisions the first user of a server,
	// and how the server's own administrator is provisioned.
	if dataset == "" && !mayCreate && !mayAdminister {
		return errors.New("a person is added to a dataset, which has to be named")
	}
	// Derived before the lock, for the reason derive gives. An existing person
	// being extended does not use it, which costs one derivation and keeps
	// the lock free of it.
	salt := random()
	derived, err := a.derive(context.Background(), secret, salt)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.data.Retired[id] {
		return errors.New("that account name belonged to a deleted account and is not reused")
	}
	if existing, ok := a.data.Principals[id]; ok && existing.Kind != "person" {
		return errors.New("principal already exists")
	}
	held := map[string]store.Grants{}
	if dataset != "" {
		held[dataset] = grants
	}
	// Adding an existing person to a second dataset extends them rather than
	// replacing them, which is the household case: one account, a dataset of
	// their own and a share of the common one.
	if existing, ok := a.data.Principals[id]; ok && existing.Kind == "person" {
		for d, g := range existing.Datasets {
			if d != dataset {
				held[d] = g
			}
		}
		existing.Datasets = held
		// Granting creation extends; it is never taken away by an addition,
		// because adding somebody to a dataset is not a statement about their
		// server rights.
		existing.MayCreateDataset = existing.MayCreateDataset || mayCreate
		existing.MayAdministerServer = existing.MayAdministerServer || mayAdminister
		a.data.Principals[id] = existing
		if err := a.syncSharedReads(context.Background(), id); err != nil {
			return err
		}
		return a.save()
	}
	a.data.Principals[id] = Principal{ID: id, Kind: "person", Salt: salt, Password: derived, Datasets: held, MayCreateDataset: mayCreate, MayAdministerServer: mayAdminister, CreatedAt: time.Now().Unix()}
	return a.save()
}
func (a *Authority) AddService(id, dataset string, grants store.Grants) (string, error) {
	if id == "" || len(id) > 255 {
		return "", errors.New("invalid principal ID")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.data.Principals[id]; ok || a.data.Retired[id] {
		return "", errors.New("principal already exists")
	}
	secret := random()
	// A service may start holding nothing: a template issuer's reader gains
	// its datasets as the template creates them.
	held := map[string]store.Grants{}
	if dataset != "" {
		held[dataset] = grants
	}
	a.data.Principals[id] = Principal{ID: id, Kind: "service", Datasets: held, CreatedAt: time.Now().Unix()}
	a.data.Clients[id] = Client{ID: id, Name: id, Principal: id, Secret: digest(secret)}
	if err := a.save(); err != nil {
		return "", err
	}
	return secret, nil
}
func (a *Authority) Authenticate(ctx context.Context, bearer string) (store.Access, error) {
	if err := ctx.Err(); err != nil {
		return store.Access{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	token, ok := a.data.Tokens[digest(bearer)]
	if !ok || token.Kind != "access" || token.Expires <= time.Now().Unix() {
		return store.Access{}, store.Refuse("authentication_failed")
	}
	auth, ok := a.data.Authorizations[token.Authorization]
	if !ok || auth.Revoked || !a.active(auth.Principal) {
		return store.Access{}, store.Refuse("authentication_failed")
	}
	if auth.Console != nil {
		if _, err := a.syncConsole(ctx, auth); err != nil {
			return store.Access{}, store.Refuse("authentication_failed")
		}
	}
	return store.Access{Principal: auth.Principal, Authorization: auth.ID}, nil
}
func (a *Authority) issue(auth Authorization, refresh bool) map[string]any {
	access := random()
	a.data.Tokens[digest(access)] = Token{Authorization: auth.ID, Kind: "access", Expires: time.Now().Add(5 * time.Minute).Unix()}
	result := map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": 300}
	if refresh {
		token := random()
		a.data.Tokens[digest(token)] = Token{Authorization: auth.ID, Kind: "refresh", Expires: time.Now().Add(30 * 24 * time.Hour).Unix()}
		result["refresh_token"] = token
	}
	return result
}

// maxAuthorizations bounds the registry, which is rewritten whole on every
// change. maxPersonAuthorizations bounds one person's live installations.
const (
	maxAuthorizations       = 10000
	maxPersonAuthorizations = 256
)

// makeRoomForAuthorization keeps the bound on authorizations from being a
// budget one account can spend for everybody.
//
// It used to count every authorization ever made, revoked ones included, and
// refuse past the bound: a long-running server eventually refused every
// sign-in, and one account approving again and again could bring that about
// in hours. Now a person holding the most live installations allowed has
// their oldest signed out to make room — which costs them one old session
// and nobody else anything — and when the registry is full, revoked
// authorizations are dropped: their tokens no longer authenticate and their
// access is already gone from the storage. A service is not capped: it holds
// one authorization per dataset it reaches and cannot multiply them.
func (a *Authority) makeRoomForAuthorization(ctx context.Context, principal Principal) error {
	if principal.Kind == "person" {
		live := []Authorization{}
		for _, auth := range a.data.Authorizations {
			if auth.Principal == principal.ID && !auth.Revoked {
				live = append(live, auth)
			}
		}
		if len(live) >= maxPersonAuthorizations {
			sort.Slice(live, func(i, j int) bool {
				if live[i].Created != live[j].Created {
					return live[i].Created < live[j].Created
				}
				return live[i].ID < live[j].ID
			})
			for _, oldest := range live[:len(live)-maxPersonAuthorizations+1] {
				if err := a.revokeEverywhere(ctx, oldest); err != nil {
					return err
				}
				oldest.Revoked, oldest.CreateDataset = true, false
				a.data.Authorizations[oldest.ID] = oldest
			}
		}
	}
	if len(a.data.Authorizations) >= maxAuthorizations {
		for id, auth := range a.data.Authorizations {
			if auth.Revoked {
				delete(a.data.Authorizations, id)
			}
		}
	}
	if len(a.data.Authorizations) >= maxAuthorizations {
		return store.Refuse("rate_limited")
	}
	return nil
}

func (a *Authority) newAuthorization(ctx context.Context, principal Principal, client, dataset string, requested store.Grants, create, administer bool) (Authorization, error) {
	if err := a.makeRoomForAuthorization(ctx, principal); err != nil {
		return Authorization{}, err
	}
	// An installation never exceeds what the person holds, on a dataset or on
	// the server. Asking for a right the account does not have is refused
	// rather than quietly granted as much as it could be.
	create = create && principal.MayCreateDataset
	// An authorization over no dataset is the bootstrap case: it carries a
	// server right and nothing else, and its listing is empty.
	// A server administrator may instead approve administration alone, which
	// reaches accounts and no dataset.
	if dataset == "" {
		administers := administer && principal.MayAdministerServer
		if !create && !administers {
			return Authorization{}, store.Refuse("permission_denied")
		}
		auth := Authorization{ID: random(), Principal: principal.ID, Client: client, CreateDataset: create, Administer: administers, Created: time.Now().UnixNano()}
		a.data.Authorizations[auth.ID] = auth
		return auth, nil
	}
	held, ok := principal.Datasets[dataset]
	if !ok {
		return Authorization{}, store.Refuse("permission_denied")
	}
	grants := store.Grants{Read: held.Read && requested.Read, Write: held.Write && requested.Write, Schema: held.Schema && requested.Schema, Erase: held.Erase && requested.Erase, Insert: held.Insert && requested.Insert, ExpiresAt: held.ExpiresAt}
	// A guest's scope is the account's, so every authorization the account
	// obtains carries it — including this one, minted after the share.
	view, guest := guestView(principal, dataset)
	if guest {
		grants.Write, grants.Schema, grants.Erase = false, false, false
	}
	approved := store.Grants{Read: requested.Read, Write: requested.Write, Schema: requested.Schema, Erase: requested.Erase, Insert: requested.Insert}
	auth := Authorization{ID: random(), Principal: principal.ID, Client: client, Dataset: dataset, Grants: grants, CreateDataset: create, Approved: &approved, Created: time.Now().UnixNano()}
	if err := a.engine.SetAuthorization(ctx, dataset, store.Access{Principal: principal.ID, Authorization: auth.ID}, store.Authorization{Grants: grants, View: view}); err != nil {
		return auth, err
	}
	a.data.Authorizations[auth.ID] = auth
	return auth, nil
}
func (a *Authority) revoke(ctx context.Context, id string) error {
	auth, ok := a.data.Authorizations[id]
	if !ok {
		return nil
	}
	auth.Revoked = true
	auth.CreateDataset = false
	client := a.data.Clients[auth.Client]
	if client.Secret != "" {
		client.Disabled = true
		a.data.Clients[client.ID] = client
	}
	a.data.Authorizations[id] = auth
	if err := a.revokeEverywhere(ctx, auth); err != nil {
		return err
	}
	return a.save()
}

// revokeEverywhere takes this authorization's access off every dataset the
// storage holds, not only the one its approval named.
//
// An authorization that created a dataset holds access to it, because creation
// installs full access for the caller that made it — and it installs it under
// the same principal and authorization, on a dataset this registry never hears
// about. Revoking the named dataset alone therefore left a withdrawn
// installation reading, writing and erasing everything it had ever created.
// The datasets are the list to walk because nothing here is.
func (a *Authority) revokeEverywhere(ctx context.Context, auth Authorization) error {
	access := store.Access{Principal: auth.Principal, Authorization: auth.ID}
	held, err := a.engine.Reach(ctx, access)
	if err != nil {
		return err
	}
	named := auth.Dataset == ""
	for _, dataset := range held {
		if dataset == auth.Dataset {
			named = true
		}
		if err = a.engine.Revoke(ctx, dataset, access); err != nil {
			return err
		}
	}
	// A dataset the approval names but the reach does not hold was retired,
	// or its row is already gone. Revoking against it costs one statement,
	// and skipping it would rest on reading an absence this layer cannot
	// interpret.
	if !named {
		return a.engine.Revoke(ctx, auth.Dataset, access)
	}
	return nil
}
func equal(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// AddAutomation creates a separately revocable long-lived token. It never
// includes erase, even when the account may erase the dataset.
func (a *Authority) AddAutomation(ctx context.Context, principalID, dataset string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	principal, ok := a.data.Principals[principalID]
	if !ok {
		return "", errors.New("unknown principal")
	}
	if principal.Suspended {
		return "", errors.New("the account is suspended")
	}
	// A long-lived token never carries erase — a credential that can destroy a
	// dataset and lives for a year is a hazard nobody chose. It may carry
	// create, which destroys nothing, where the account holds that right.
	auth, err := a.newAuthorization(ctx, principal, "automation", dataset, store.Grants{Read: true, Write: true, Schema: true, Insert: true}, principal.MayCreateDataset, false)
	if err != nil {
		return "", err
	}
	token := random()
	a.data.Tokens[digest(token)] = Token{Authorization: auth.ID, Kind: "access", Expires: time.Now().Add(365 * 24 * time.Hour).Unix()}
	if err = a.save(); err != nil {
		return "", err
	}
	return token, nil
}

// MayCreateDataset reports whether this authorization may create a dataset. It
// is a server-level right rather than a dataset grant, so it is not in the
// access table and cannot be answered by asking about a dataset.
//
// The rule is deliberately narrow: an authorization that already administers a
// dataset may create another. That gives a household a way in without a second
// mechanism, and it is what makes the self-service case visibly *missing* — a
// person with no dataset yet holds nothing to be an administrator of, so this
// answers no and the bootstrap authorization that would answer yes is the piece
// still to be designed. Better a gap that is obvious than one papered over.
func (a *Authority) MayCreateDataset(access store.Access) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	auth, ok := a.data.Authorizations[access.Authorization]
	if !ok || auth.Revoked || auth.Principal != access.Principal {
		return false
	}
	// Both must hold: the account has the right, and this installation was
	// approved for it. Withdrawing either stops new creations, and neither
	// reaches an outcome already recorded.
	return auth.CreateDataset && a.data.Principals[auth.Principal].MayCreateDataset
}

// active reports whether an account exists and may authenticate. Every path
// by which a credential becomes access asks it — bearer tokens, the three
// token grants, sign-in and administrator sign-in — so suspending an account
// is one flag rather than a sweep over its installations.
func (a *Authority) active(id string) bool {
	principal, ok := a.data.Principals[id]
	return ok && !principal.Suspended
}

// RotateServiceSecret replaces a service's secret and returns the new one; the
// old one stops working at once. The service keeps its authorizations, so what
// it recorded and the receipts it may still ask about stay its own; tokens
// already issued lapse within minutes. A secret withdrawn by revoking the
// service's authorization stays withdrawn: rotating replaces a lost secret, it
// does not undo a revocation.
func (a *Authority) RotateServiceSecret(id string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	principal, ok := a.data.Principals[id]
	client, held := a.data.Clients[id]
	if !ok || principal.Kind != "service" || !held || client.Principal != id {
		return "", store.Refuse("not_found")
	}
	secret := random()
	client.Secret = digest(secret)
	a.data.Clients[id] = client
	return secret, a.save()
}

// Freeze stops every change to the registry, and to the grants it writes
// into the storage, until the function it returns is called. A backup holds
// it for the moment it pins its SQL snapshot and reads the registry file, so
// the two describe the same state. Keep it short: it also holds every bearer
// token check.
func (a *Authority) Freeze() func() {
	a.mu.Lock()
	return a.mu.Unlock
}
