// Package standalone composes the self-hosted server; its HTTP surface contains
// no product-specific schemas or workflow.
package standalone

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ekarpov/tessera/internal/acquisition"
	"github.com/ekarpov/tessera/internal/admin"
	"github.com/ekarpov/tessera/internal/identity"
	"github.com/ekarpov/tessera/internal/release"
	"github.com/ekarpov/tessera/internal/sqlengine"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/server"
	"github.com/ekarpov/tessera/pkg/store"
	"github.com/ekarpov/tessera/pkg/store/postgres"
	"github.com/ekarpov/tessera/pkg/store/sqlite"
)

type Config struct {
	FirstPartyConsoles []identity.ConsoleConfig `json:"first_party_consoles,omitempty"`
	IdentityBrand      *identity.BrandConfig    `json:"identity_brand,omitempty"`
	DeprecatedVersions []p.DeprecatedVersion    `json:"deprecated_versions,omitempty"`
	DisplayName        string                   `json:"display_name,omitempty"`
	Listen             string                   `json:"listen"`
	Issuer             string                   `json:"issuer"`
	DataDir            string                   `json:"data_dir"`
	TLSCertificate     string                   `json:"tls_certificate"`
	TLSKey             string                   `json:"tls_key"`
	Engine             string                   `json:"engine"`
	PostgresDSNFile    string                   `json:"postgres_dsn_file"`
	Content            bool                     `json:"content"`
	EncryptedStorage   bool                     `json:"encrypted_storage_confirmed"`
	// DatasetCreation offers POST /v1/datasets. Off by default: a deployment
	// that offers it lets a caller holding create_dataset allocate storage, and
	// nothing in the protocol bounds how much, so quota and rate limiting are
	// the deployment's to answer before it is turned on.
	DatasetCreation bool `json:"dataset_creation"`
	// OptionalReads names the optional read capabilities this deployment
	// advertises. Configuration may withhold an optional feature; it can never
	// disable a required operation, and an unknown name is refused at startup
	// rather than silently advertised.
	OptionalReads []string `json:"optional_reads,omitempty"`
	// Limits override the development defaults for datasets created from now
	// on, member by member. A member left out keeps its default rather than
	// becoming a bound of nothing. An existing dataset keeps the limits it was
	// created with; -set-dataset-limits changes one.
	Limits json.RawMessage `json:"limits,omitempty"`
	// Backup, when set, takes encrypted backups while the server runs, so a
	// nightly backup no longer means stopping the service.
	Backup *BackupConfig `json:"backup,omitempty"`
	// BackupSettingsBootstrap is an explicit local recovery input, never JSON configuration.
	BackupSettingsBootstrap string `json:"-"`
	// TrustedProxies names the reverse proxies, as addresses or CIDR ranges,
	// whose X-Forwarded-For this server believes when counting requests per
	// address. Leave it empty for a server reached directly: believed from
	// anyone, the header would let a caller choose its own address.
	TrustedProxies []string `json:"trusted_proxies,omitempty"`
}

// ParseProxies reads trusted_proxies. A bare address is its own range.
func ParseProxies(entries []string) ([]netip.Prefix, error) {
	proxies := make([]netip.Prefix, 0, len(entries))
	for _, entry := range entries {
		if prefix, err := netip.ParsePrefix(entry); err == nil {
			proxies = append(proxies, prefix.Masked())
			continue
		}
		addr, err := netip.ParseAddr(entry)
		if err != nil {
			return nil, errors.New("trusted_proxies must list addresses or CIDR ranges")
		}
		proxies = append(proxies, netip.PrefixFrom(addr.Unmap(), addr.Unmap().BitLen()))
	}
	return proxies, nil
}

// BackupConfig schedules backups taken by the running server.
type BackupConfig struct {
	// Directory receives the backups. It must exist and be private.
	Directory string `json:"directory"`
	// KeyFile holds the 32-byte key, kept apart from the backups.
	KeyFile string `json:"key_file"`
	// EveryHours is the interval; the first backup is taken at startup.
	// Zero disables the internal schedule for callers that own scheduling.
	EveryHours int `json:"every_hours"`
	// Keep is how many backups are retained. Older ones are removed after a
	// new one is written, never before, so a failed backup deletes nothing.
	Keep int `json:"keep"`
}
type Runtime struct {
	Handler       http.Handler
	Authority     *identity.Authority
	Engine        *sqlite.Store
	Config        Config
	Acquisitions  *acquisition.Manager
	backupContext context.Context
	stopBackups   context.CancelFunc
}

func (r *Runtime) Close() error {
	if r.stopBackups != nil {
		r.stopBackups()
	}
	if r.Acquisitions != nil {
		r.Acquisitions.Close()
	}
	return r.Engine.Close()
}
func ReadConfig(path string) (Config, error) {
	var config Config
	raw, err := os.ReadFile(path)
	if err != nil {
		return config, errors.New("cannot read server configuration")
	}
	if err = p.DecodeRequest(raw, &config, 16<<10); err != nil {
		return config, errors.New("invalid server configuration")
	}
	issuer, err := url.Parse(config.Issuer)
	if err != nil || issuer.Scheme != "https" || issuer.Host == "" || issuer.Path != "" || issuer.RawQuery != "" || issuer.Fragment != "" || issuer.User != nil {
		return config, errors.New("issuer must be an HTTPS origin")
	}
	if config.Listen == "" || config.DataDir == "" || config.TLSCertificate == "" || config.TLSKey == "" {
		return config, errors.New("listen, data_dir and TLS certificate/key are required")
	}
	if !config.EncryptedStorage {
		return config, errors.New("configure encrypted storage and explicitly confirm encrypted_storage_confirmed")
	}
	if config.Engine == "" {
		config.Engine = "sqlite"
	}
	if config.Engine != "sqlite" && config.Engine != "postgres" {
		return config, errors.New("engine must be sqlite or postgres")
	}
	if b := config.Backup; b != nil {
		if b.Directory == "" || b.KeyFile == "" || b.EveryHours < 0 || b.EveryHours > 87600 || b.Keep < 1 || b.Keep > 1024 {
			return config, errors.New("backup needs a directory, a key_file, every_hours from 0 to 87600 and keep from 1 to 1024")
		}
		if info, err := os.Stat(b.Directory); err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
			return config, errors.New("the backup directory must exist and be readable by this account only")
		}
		if _, err := PrivateFile(b.KeyFile); err != nil {
			return config, err
		}
	}
	if _, err := p.MergeLimits(p.DevelopmentLimits(), config.Limits); err != nil {
		return config, errors.New("limits must be positive, name only known limits, and keep idempotency_retention_seconds at 86400 or more")
	}
	if _, err := ParseProxies(config.TrustedProxies); err != nil {
		return config, err
	}
	for _, name := range config.OptionalReads {
		known := false
		for _, allowed := range p.OptionalReadCapabilities {
			known = known || name == allowed
		}
		if !known {
			return config, errors.New("optional_reads may name only optional read capabilities")
		}
	}
	return config, nil
}

// inspectorAccess is the administration surface's read handle. It is not a
// credential: no token maps to it and none can be issued for it.
var inspectorAccess = store.Access{Principal: "administration", Authorization: "administration-inspector"}

func Open(ctx context.Context, config Config) (result *Runtime, err error) {
	if err := p.ValidateDeprecatedVersions([]string{"1"}, config.DeprecatedVersions); err != nil {
		return nil, err
	}
	if config.DisplayName != "" {
		if err := sqlengine.ValidateDisplayName(config.DisplayName); err != nil {
			return nil, errors.New("display_name must be valid UTF-8 text of at most 200 bytes, with non-whitespace text and no control characters")
		}
	}
	if err = os.MkdirAll(config.DataDir, 0700); err != nil {
		return nil, errors.New("cannot create server data directory")
	}
	if info, e := os.Stat(config.DataDir); e != nil || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("server data directory must be private")
	}
	limits, err := p.MergeLimits(p.DevelopmentLimits(), config.Limits)
	if err != nil {
		return nil, err
	}
	var engine *sqlite.Store
	if config.Engine == "postgres" {
		dsn, e := PrivateFile(config.PostgresDSNFile)
		if e != nil {
			return nil, e
		}
		engine, err = postgres.Open(ctx, strings.TrimSpace(string(dsn)), filepath.Join(config.DataDir, "postgres-fence"), limits)
	} else {
		engine, err = sqlite.Open(ctx, filepath.Join(config.DataDir, "dataset.db"), limits)
	}
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			engine.Close()
		}
	}()
	if err = engine.RegisterHostRegistry(ctx); err != nil {
		return nil, err
	}
	if err = engine.SetContentCleanupDirectory(filepath.Join(config.DataDir, "content")); err != nil {
		return nil, err
	}
	if err = engine.EnableReads(config.OptionalReads...); err != nil {
		return nil, errors.New("cannot advertise the configured optional read capabilities")
	}
	if config.Content {
		if err = engine.EnableContent(ctx, filepath.Join(config.DataDir, "content")); err != nil {
			return nil, err
		}
	}
	// This deployment serves every dataset its storage holds. Which one an
	// approval is for is carried by the request and shown on the screen, so the
	// authority is bound to the server rather than to a dataset.
	authority, err := identity.Open(ctx, filepath.Join(config.DataDir, "identity.json"), config.Issuer, engine)
	if err != nil {
		return nil, err
	}
	if err = authority.ReconcileDatasets(ctx); err != nil {
		return nil, err
	}
	proxies, err := ParseProxies(config.TrustedProxies)
	if err != nil {
		return nil, err
	}
	authority.SetTrustedProxies(proxies)
	if err = authority.SetConsoles(config.FirstPartyConsoles); err != nil {
		return nil, err
	}
	authority.SetDisplayName(config.DisplayName)
	if err = authority.SetBrand(config.IdentityBrand); err != nil {
		return nil, err
	}
	// Creation is offered only when the configuration says so, and the decision
	// is the deployment's rather than the protocol's. A caller may create when
	// it holds create_dataset, which this deployment reads from the account
	// registry — so the answer follows the same administration as every other
	// grant, and withdrawing it stops new creations without touching recovery.
	options := server.Options{DeprecatedVersions: config.DeprecatedVersions, DisplayName: config.DisplayName, AuthMethods: []string{"oauth2", "bearer_token", "client_credentials"}, AuthorizationServers: []string{config.Issuer}, ContentOrigin: config.Issuer, SharedReads: true}
	if config.DatasetCreation {
		options.CreationOffered = func() bool { return true }
		options.Provision = func(_ context.Context, a store.Access) error {
			if !authority.MayCreateDataset(a) {
				return store.Refuse("permission_denied")
			}
			return nil
		}
	}
	// A dataset created over the wire has to become one this deployment holds
	// as well, and not only at the next restart. Two things follow a creation
	// here, both idempotent because a replay runs them again: the account
	// that created it is recorded as holding it, and the administration
	// handle below is given its read. Without the first nobody administers
	// what was just made; without the second nobody can see it.
	options.Provisioned = func(ctx context.Context, a store.Access, dataset string) error {
		if err := engine.SetAuthorization(ctx, dataset, inspectorAccess,
			store.Authorization{Grants: store.Grants{Read: true}, View: store.FullView()}); err != nil {
			return err
		}
		return authority.RecordCreation(a.Principal, dataset,
			store.Grants{Read: true, Write: true, Schema: true, Erase: true})
	}
	// A template invitation creates a dataset per redemption. It is created
	// under a handle of its own, which then holds nothing: creation installs
	// full access for whoever creates, and neither the issuing service nor
	// this composition should be left reaching a participant's dataset. The
	// administration handle gets its read, as for every dataset held here.
	authority.SetProvisioner(func(ctx context.Context, displayName string, types []p.Meta) (string, error) {
		return unheldDataset(ctx, engine, displayName, types)
	})
	// Deleting the last administrator of a dataset erases it, through a handle
	// that exists for that one erasure: granted erase, used, and revoked from
	// the replacement, so the emptied dataset is left held by nobody.
	authority.SetEraser(func(ctx context.Context, dataset string) error {
		eraser := store.Access{Principal: "account-deletion", Authorization: "account-deletion-" + dataset}
		if err := engine.SetAuthorization(ctx, dataset, eraser,
			store.Authorization{Grants: store.Grants{Erase: true}, View: store.FullView()}); err != nil {
			return err
		}
		erased, err := engine.Erase(ctx, eraser, dataset, dataset)
		if err != nil {
			_ = engine.Revoke(ctx, dataset, eraser)
			return err
		}
		return engine.Revoke(ctx, erased.DatasetID, eraser)
	})
	records, err := server.NewWithOptions(erasureAware{Store: engine, authority: authority}, authority.Authenticate, options)
	if err != nil {
		return nil, err
	}
	// The administration surface reads the dataset's description and schema
	// through a handle the composition provisions here. It is not a credential:
	// no token maps to it and none can be issued for it, and it carries read
	// only, so administration cannot write, change schema or erase.
	inspector := inspectorAccess
	held, err := engine.Provisioned(ctx)
	if err != nil {
		return nil, err
	}
	for _, d := range held {
		if err = engine.SetAuthorization(ctx, d.ID, inspector, store.Authorization{Grants: store.Grants{Read: true}, View: store.FullView()}); err != nil {
			return nil, err
		}
	}
	administration, err := admin.New(authority, engine, inspector, config.Engine)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	auth := authority.Handler()
	mux.Handle("/oauth/", auth)
	administration.Routes(mux)
	administration.APIRoutes(mux)
	administration.ReaderRoutes(mux)
	mux.Handle("/.well-known/oauth-authorization-server", auth)
	if config.Content {
		mux.HandleFunc("GET /.well-known/tessera-content", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"protocol_versions": []string{"1"}, "server": map[string]string{"name": "tessera-content", "version": release.Version()}, "auth_methods": []string{"oauth2", "bearer_token", "client_credentials"}, "authorization_servers": []string{config.Issuer}})
		})
	}
	mux.Handle("/", records)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		mux.ServeHTTP(w, r)
	})
	engine.StartMaintenance(ctx)
	runtime := &Runtime{Handler: handler, Authority: authority, Engine: engine, Config: config}
	// On demand as well as on the schedule, where backups are configured at all.
	if config.Backup != nil {
		key, e := PrivateFile(config.Backup.KeyFile)
		if e != nil {
			return nil, e
		}
		var recovery *acquisition.SettingsExport
		if config.BackupSettingsBootstrap != "" {
			file, readErr := os.Open(config.BackupSettingsBootstrap)
			if readErr != nil {
				return nil, errors.New("cannot read backup settings bootstrap")
			}
			raw, readErr := io.ReadAll(io.LimitReader(file, 16385))
			file.Close()
			if readErr != nil || len(raw) > 16384 {
				return nil, errors.New("invalid backup settings bootstrap")
			}
			recovery, readErr = decodeBackupSettingsBootstrap(raw)
			if readErr != nil {
				return nil, errors.New("invalid backup settings bootstrap")
			}
		}
		runtime.Acquisitions, e = acquisition.OpenNative(config.Backup.Directory, key, config.Backup.EveryHours, config.Backup.Keep, recovery, func(job context.Context, kind, path string) (s, r string, evidence []byte, failure error) {
			if kind == "backup" {
				s, failure = engine.BackupCapturedWithFiles(job, path, key, authority.Freeze, func() []string { return RegistryFiles(config.DataDir) })
				if failure != nil {
					return
				}
			}
			evidence, r, failure = engine.CaptureRetirementEvidence(job)
			return
		}, func(at time.Time) {
			// This diagnostic cannot invalidate an already published archive. Replay
			// on startup retries a missed write without moving the timestamp backwards.
			if at.After(authority.LastBackup()) {
				_ = authority.RecordBackup(at)
			}
		})
		if e != nil {
			return nil, e
		}
		runtime.backupContext, runtime.stopBackups = context.WithCancel(ctx)
		administration.SetAcquisitions(runtime.Acquisitions)
		administration.SetBackup(runtime.BackupNow)
	}
	if config.Backup == nil && config.BackupSettingsBootstrap != "" {
		return nil, errors.New("backups are not configured")
	}
	return runtime, nil
}

// PrivateFile reads a file the server treats as a credential, refusing one any
// other account can read.
//
// The refusal names the file and says what is wrong with it. It used to say
// "credential files must be private regular files" and name nothing, which on
// a deployment holding a TLS key, a configuration and a database DSN is three
// candidates and no signal — an operator gets a restart loop pointing
// nowhere. A connection string is the one that catches people, because it does
// not look like a credential and it carries the database password, and 0644 is
// the mode a file gets by not being thought about.
//
// Naming the path and the mode is safe here in a way it would not be in a
// protocol error: this is the operator's own console at startup, they wrote
// the path, and the contents are never quoted.
func PrivateFile(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("a credential file is required and none was configured")
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read credential file %s", path)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("credential file %s is not a regular file", path)
	}
	if info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("credential file %s is mode %04o and must be 0600: other accounts on this host can read it", path, info.Mode().Perm())
	}
	if info.Size() > 64<<10 {
		return nil, fmt.Errorf("credential file %s is too large", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read credential file %s", path)
	}
	return raw, nil
}

// AddPerson adds a person to one dataset with the grants they are given, and
// optionally the server right to create datasets. It no longer hands out every
// grant: a household is several people on one shared dataset, and four people
// who can each erase it is not an administration model. Adding the same person
// to a second dataset extends them, and never takes creation away.
//
// The dataset may be empty when creation is the only thing granted, which is
// how the first account on a server comes to exist before any dataset does.
func (r *Runtime) AddPerson(id, passwordFile, dataset string, grants store.Grants, mayCreate, mayAdminister bool) error {
	raw, err := PrivateFile(passwordFile)
	if err != nil {
		return err
	}
	secret := strings.TrimSpace(string(raw))
	if mayAdminister {
		return r.Authority.AddServerAdministrator(id, secret, dataset, grants, mayCreate)
	}
	return r.Authority.AddPerson(id, secret, dataset, grants, mayCreate)
}

// AddService provisions a service, writing its secret to output. With fresh
// it is given a new dataset of its own, which nobody else holds; otherwise the
// dataset named, or none. datasetOutput, when set, receives the identity of
// the dataset it holds, so an operator's tooling need not read the registry.
func (r *Runtime) AddService(ctx context.Context, id, output, dataset string, fresh bool, datasetOutput string) error {
	if output == "" {
		return errors.New("secret output file is required")
	}
	if fresh && dataset != "" {
		return errors.New("-new-dataset makes a dataset; it cannot also name one")
	}
	file, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errors.New("cannot create private service credential file")
	}
	defer file.Close()
	var named *os.File
	if datasetOutput != "" {
		if named, err = os.OpenFile(datasetOutput, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600); err != nil {
			_ = os.Remove(output)
			return errors.New("the dataset output must be a new file")
		}
		defer named.Close()
	}
	fail := func(err error) error {
		_ = os.Remove(output)
		if named != nil {
			_ = os.Remove(datasetOutput)
		}
		return err
	}
	if fresh {
		if dataset, err = unheldDataset(ctx, r.Engine, id, nil); err != nil {
			return fail(err)
		}
	}
	secret, err := r.Authority.AddService(id, dataset, store.Grants{Read: true, Write: true, Schema: true})
	if err != nil {
		return fail(err)
	}
	if _, err = file.WriteString(secret + "\n"); err != nil {
		return errors.New("cannot persist service credential")
	}
	if err = file.Sync(); err != nil {
		return errors.New("cannot persist service credential")
	}
	if named != nil {
		if _, err = named.WriteString(dataset + "\n"); err != nil {
			return errors.New("cannot write the dataset output")
		}
		return named.Sync()
	}
	return nil
}

func (r *Runtime) AddAutomation(ctx context.Context, principal, output, dataset string) error {
	file, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errors.New("cannot create private automation credential file")
	}
	defer file.Close()
	token, err := r.Authority.AddAutomation(ctx, principal, dataset)
	if err != nil {
		return err
	}
	if _, err = file.WriteString(token + "\n"); err != nil {
		return errors.New("cannot persist automation credential")
	}
	return file.Sync()
}

// ResolveDataset turns an optional -dataset into the one a command acts on.
// A storage holding exactly one has a single possible answer, so naming it is
// ceremony and the flag may be omitted; a storage holding several has no
// default worth guessing, because the wrong guess grants somebody access to
// the wrong records.
func (r *Runtime) ResolveDataset(ctx context.Context, named string) (string, error) {
	held, err := r.Engine.Provisioned(ctx)
	if err != nil {
		return "", err
	}
	if named != "" {
		for _, d := range held {
			if d.ID == named {
				return named, nil
			}
		}
		return "", errors.New("this storage holds no such dataset")
	}
	if len(held) == 1 {
		return held[0].ID, nil
	}
	names := make([]string, 0, len(held))
	for _, d := range held {
		names = append(names, d.ID)
	}
	return "", fmt.Errorf("this storage holds %d datasets, so -dataset is required: %s", len(held), strings.Join(names, ", "))
}

// ParseGrants reads the -grants list. Nothing is granted by default beyond what
// is asked for, and erase is never implied: it is the grant that destroys a
// dataset, and a person who holds it should have been given it deliberately.
func ParseGrants(list string) (store.Grants, error) {
	grants := store.Grants{}
	for _, name := range strings.Split(list, ",") {
		switch strings.TrimSpace(name) {
		case "read":
			grants.Read = true
		case "write":
			grants.Write = true
		case "schema":
			grants.Schema = true
		case "erase":
			grants.Erase = true
		case "":
		default:
			return store.Grants{}, fmt.Errorf("unknown grant %q: use read, write, schema or erase", name)
		}
	}
	if !grants.Read && !grants.Write && !grants.Schema && !grants.Erase {
		return store.Grants{}, errors.New("a person needs at least one grant")
	}
	return grants, nil
}

// erasureAware is the storage as the protocol server sees it, telling the
// account registry about each erasure made through the protocol. Every other
// method is the storage's own.
type erasureAware struct {
	*sqlite.Store
	authority *identity.Authority
}

// Erase erases, then records the new identity in the registry. The erasure
// has happened whatever the registry does, so a failure to record it is not
// reported as a failure to erase: the caller would retry an erasure that is
// already done. Administration leaves out a dataset it cannot reach, so a
// registry that fell behind shows less rather than failing.
func (e erasureAware) Erase(ctx context.Context, a store.Access, dataset, confirm string) (store.EraseResult, error) {
	result, err := e.Store.Erase(ctx, a, dataset, confirm)
	if err == nil {
		_ = e.authority.RecordErasure(a, dataset, result.DatasetID)
	}
	return result, err
}

// unheldDataset creates a dataset that nobody holds yet, for the account
// registry to grant. Creation installs full access for whoever creates, so it
// is created under a handle of its own that then holds nothing, and the
// administration handle gets its read, as for every dataset held here.
func unheldDataset(ctx context.Context, engine *sqlite.Store, displayName string, types []p.Meta) (string, error) {
	installer := store.Access{Principal: "installer", Authorization: "installer"}
	made, err := engine.CreateDataset(ctx, installer, displayName, p.Limits{}, types, "", [32]byte{})
	if err != nil {
		return "", err
	}
	if err = engine.Revoke(ctx, made.DatasetID, installer); err != nil {
		return "", err
	}
	if err = engine.SetAuthorization(ctx, made.DatasetID, inspectorAccess,
		store.Authorization{Grants: store.Grants{Read: true}, View: store.FullView()}); err != nil {
		return "", err
	}
	return made.DatasetID, nil
}
