package standalone

import (
	"encoding/json"
	"fmt"
	"github.com/ekarpov/tessera/internal/identity"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
	"github.com/ekarpov/tessera/pkg/store/sqlite"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStandaloneServiceAndAdminBoundaries(t *testing.T) {
	config := Config{Issuer: "https://tessera.example", DataDir: t.TempDir(), Engine: "sqlite", Content: true, EncryptedStorage: true}
	if err := os.Chmod(config.DataDir, 0700); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	secret, err := runtime.Authority.AddService("synthetic-service", sole(t, runtime.Engine), store.Grants{Read: true, Write: true, Schema: true})
	if err != nil {
		t.Fatal(err)
	}
	token := func() string {
		r := httptest.NewRequest("POST", "/oauth/token", strings.NewReader("grant_type=client_credentials&dataset="+sole(t, runtime.Engine)))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.SetBasicAuth("synthetic-service", secret)
		w := httptest.NewRecorder()
		runtime.Handler.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		var response map[string]any
		json.Unmarshal(w.Body.Bytes(), &response)
		return response["access_token"].(string)
	}
	bearer := token()
	r := httptest.NewRequest("GET", "/v1/datasets", nil)
	r.Header.Set("Authorization", "Bearer "+bearer)
	w := httptest.NewRecorder()
	runtime.Handler.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "content_store_id") {
		t.Fatal(w.Code, w.Body.String())
	}
	r = httptest.NewRequest("GET", "/admin", nil)
	r.Header.Set("Authorization", "Bearer "+bearer)
	w = httptest.NewRecorder()
	runtime.Handler.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("service gained administration")
	}
	runtime.Close()
	runtime, err = Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if _, err = runtime.Authority.Authenticate(t.Context(), bearer); err != nil {
		t.Fatal("restart lost credentials")
	}
}
func TestConfigurationRequiresTLSAndEncryption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"listen":"127.0.0.1:8443","issuer":"http://unsafe.example","data_dir":"/tmp/test","tls_certificate":"cert","tls_key":"key","encrypted_storage_confirmed":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadConfig(path); err == nil {
		t.Fatal("plaintext public issuer accepted")
	}
}

// sole names the storage's single dataset, which a grant has to name now that a
// storage may hold several.
func sole(t *testing.T, engine *sqlite.Store) string {
	t.Helper()
	held, err := engine.Provisioned(t.Context())
	if err != nil || len(held) != 1 {
		t.Fatal("fixture should hold exactly one dataset", err)
	}
	return held[0].ID
}

// The deployment layer was the thing pinned to one dataset, not the engine.
// A household is one server holding a dataset per person and a shared one, and
// the people on the shared one are not four administrators who can each erase
// it — which is what granting everybody everything made them.
func TestAServerAdministersEveryDatasetItHolds(t *testing.T) {
	config := Config{Issuer: "https://tessera.example", DataDir: t.TempDir(), Engine: "sqlite", EncryptedStorage: true}
	if err := os.Chmod(config.DataDir, 0700); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	personal := sole(t, runtime.Engine)
	owner := store.Access{Principal: "alice", Authorization: "setup"}
	if err := runtime.Engine.SetAuthorization(t.Context(), personal, owner, store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true, Erase: true}, View: store.FullView()}); err != nil {
		t.Fatal(err)
	}
	shared, err := runtime.Engine.CreateDataset(t.Context(), owner, "Household", p.Limits{}, nil, "", [32]byte{})
	if err != nil {
		t.Fatal(err)
	}

	// Naming the dataset is required once there is more than one, because the
	// wrong guess grants somebody access to the wrong records.
	if _, err = runtime.ResolveDataset(t.Context(), ""); err == nil {
		t.Fatal("a storage holding two datasets guessed which one a grant was for")
	}
	if _, err = runtime.ResolveDataset(t.Context(), "s-not-here"); err == nil {
		t.Fatal("a dataset this storage does not hold was accepted")
	}
	if resolved, e := runtime.ResolveDataset(t.Context(), shared.DatasetID); e != nil || resolved != shared.DatasetID {
		t.Fatal("a named dataset this storage holds was refused", e)
	}

	// One administrator, and the rest read and write. Erase is never implied.
	if err = runtime.Authority.AddPerson("alice", "synthetic-password-only", shared.DatasetID, store.Grants{Read: true, Write: true, Schema: true, Erase: true}, false); err != nil {
		t.Fatal(err)
	}
	for _, member := range []string{"bob", "chris"} {
		if err = runtime.Authority.AddPerson(member, "synthetic-password-only", shared.DatasetID, store.Grants{Read: true, Write: true}, false); err != nil {
			t.Fatal(err)
		}
	}
	registry := runtime.Authority.AdminOverview(t.Context(), "alice")
	held := map[string]map[string]store.Grants{}
	for _, entry := range registry.Principals {
		held[entry.ID] = entry.Datasets
	}
	if !held["alice"][shared.DatasetID].Erase {
		t.Fatal("the administrator cannot erase the dataset it administers")
	}
	for _, member := range []string{"bob", "chris"} {
		g := held[member][shared.DatasetID]
		if !g.Read || !g.Write || g.Erase || g.Schema {
			t.Fatal("a household member was made an administrator", member, g)
		}
	}

	// And a person can be on two datasets with different grants, which is the
	// whole shape: a dataset of their own, and a share of the common one.
	if err = runtime.Authority.AddPerson("bob", "synthetic-password-only", personal, store.Grants{Read: true, Write: true, Schema: true, Erase: true}, false); err != nil {
		t.Fatal(err)
	}
	// Read as bob, who administers the personal dataset. Alice administers the
	// shared one and must not be shown this at all: the registry describes
	// every account on the server, and an administrator of one dataset reading
	// the membership of another is the disclosure half of administering it.
	for _, entry := range runtime.Authority.AdminOverview(t.Context(), "alice").Principals {
		if entry.Datasets[personal].Read || entry.Datasets[personal].Erase {
			t.Fatal("an administrator of one dataset was shown membership of another", entry.ID, entry.Datasets)
		}
	}
	registry = runtime.Authority.AdminOverview(t.Context(), "bob")
	for _, entry := range registry.Principals {
		if entry.ID != "bob" {
			continue
		}
		if len(entry.Datasets) != 1 || !entry.Datasets[personal].Erase {
			t.Fatal("adding a person to a second dataset did not keep the grants apart", entry.Datasets)
		}
	}
}

// The provisioning hook existed and nothing wired it, so a capability the
// engine has was unreachable from the shipped server. It is off unless the
// configuration says otherwise, because a deployment offering it lets a caller
// allocate storage and the protocol bounds nothing about how much.
func TestCreationIsOfferedOnlyWhenConfigured(t *testing.T) {
	for _, offered := range []bool{false, true} {
		config := Config{Issuer: "https://tessera.example", DataDir: t.TempDir(), Engine: "sqlite",
			EncryptedStorage: true, DatasetCreation: offered}
		if err := os.Chmod(config.DataDir, 0700); err != nil {
			t.Fatal(err)
		}
		runtime, err := Open(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest("GET", "/.well-known/tessera", nil)
		w := httptest.NewRecorder()
		runtime.Handler.ServeHTTP(w, r)
		var document struct {
			Capabilities []string       `json:"server_capabilities"`
			Limits       map[string]int `json:"server_limits"`
		}
		if json.Unmarshal(w.Body.Bytes(), &document) != nil {
			t.Fatal("discovery is not readable")
		}
		advertised := false
		for _, name := range document.Capabilities {
			if name == "dataset_creation" {
				advertised = true
			}
		}
		if advertised != offered {
			t.Fatal("dataset_creation advertised:", advertised, "configured:", offered)
		}
		// The window is advertised either way. Withdrawing the capability says
		// "you cannot create"; withdrawing the window would say "you cannot
		// reconcile", and a client must be able to tell those apart.
		if window := document.Limits["idempotency_retention_seconds"]; window <= 0 {
			t.Fatal("the server-scoped retention window is not advertised", window)
		}
		runtime.Close()
	}
}

// Creating a dataset installs access to it for the caller that created it, and
// the registry never learns the identity. Withdrawal revoked the dataset the
// approval named and nothing else, so an installation an administrator had
// just stopped kept reading and writing everything it had ever made.
func TestWithdrawalReachesTheDatasetsAnInstallationCreated(t *testing.T) {
	config := Config{Issuer: "https://tessera.example", DataDir: t.TempDir(), Engine: "sqlite", EncryptedStorage: true}
	if err := os.Chmod(config.DataDir, 0700); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	home := sole(t, runtime.Engine)
	if err = runtime.Authority.AddPerson("owner", "synthetic-password-only", home,
		store.Grants{Read: true, Write: true, Schema: true, Erase: true}, true); err != nil {
		t.Fatal(err)
	}
	// A long-lived token is an installation holding an authorization, which is
	// the thing an administrator withdraws.
	if _, err = runtime.Authority.AddAutomation(t.Context(), "owner", home); err != nil {
		t.Fatal(err)
	}
	overview := runtime.Authority.AdminOverview(t.Context(), "owner")
	if len(overview.Authorizations) != 1 {
		t.Fatal("fixture should hold one authorization", len(overview.Authorizations))
	}
	access := store.Access{Principal: "owner", Authorization: overview.Authorizations[0].ID}
	made, err := runtime.Engine.CreateDataset(t.Context(), access, "Second", p.Limits{}, nil, "", [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = runtime.Engine.Info(t.Context(), access, made.DatasetID); err != nil {
		t.Fatal("creation did not give the caller access to what it created", err)
	}

	if err = runtime.Authority.WithdrawGrants(t.Context(), access.Authorization); err != nil {
		t.Fatal(err)
	}
	// Compared against nil rather than against an empty error code, because
	// p.ErrorCode of a nil error is internal_error, not the empty string: an
	// assertion written the other way passes whatever the server does.
	if _, err = runtime.Engine.Info(t.Context(), access, home); err == nil {
		t.Fatal("withdrawal left the dataset the approval named")
	}
	if _, err = runtime.Engine.Info(t.Context(), access, made.DatasetID); err == nil {
		t.Fatal("withdrawal left the installation everything it had created")
	}
	// Withdrawal that leaves the right to create leaves the way back in: one
	// request replaces every grant it just took away.
	for _, entry := range runtime.Authority.AdminOverview(t.Context(), "owner").Authorizations {
		if entry.ID == access.Authorization && entry.CreateDataset {
			t.Fatal("a withdrawn installation kept the right to create itself a new dataset")
		}
	}
}

// A service acting as itself names its dataset on every request. Reuse keyed on
// the credential alone meant the second request was served the first request's
// authorization, so the requirement held only until a token had been issued.
func TestAServiceTokenIsIssuedForTheDatasetTheRequestNames(t *testing.T) {
	config := Config{Issuer: "https://tessera.example", DataDir: t.TempDir(), Engine: "sqlite", EncryptedStorage: true}
	if err := os.Chmod(config.DataDir, 0700); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	home := sole(t, runtime.Engine)
	secret, err := runtime.Authority.AddService("synthetic-service", home, store.Grants{Read: true, Write: true, Schema: true})
	if err != nil {
		t.Fatal(err)
	}
	// Someone has to administer the dataset for the registry to be readable:
	// the overview answers per administrator, not per server.
	if err = runtime.Authority.AddPerson("owner", "synthetic-password-only", home,
		store.Grants{Read: true, Write: true, Schema: true, Erase: true}, false); err != nil {
		t.Fatal(err)
	}
	owner := store.Access{Principal: "owner", Authorization: "setup"}
	if err = runtime.Engine.SetAuthorization(t.Context(), home, owner,
		store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true, Erase: true}, View: store.FullView()}); err != nil {
		t.Fatal(err)
	}
	other, err := runtime.Engine.CreateDataset(t.Context(), owner, "Somebody else", p.Limits{}, nil, "", [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	ask := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/oauth/token", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.SetBasicAuth("synthetic-service", secret)
		w := httptest.NewRecorder()
		runtime.Handler.ServeHTTP(w, r)
		return w
	}
	if w := ask("grant_type=client_credentials&dataset=" + home); w.Code != 200 {
		t.Fatal("a service was refused the dataset it holds", w.Code, w.Body.String())
	}
	if w := ask("grant_type=client_credentials"); w.Code != 400 {
		t.Fatal("a request naming no dataset was served the previous request's authorization", w.Code)
	}
	if w := ask("grant_type=client_credentials&dataset=" + other.DatasetID); w.Code != 400 {
		t.Fatal("a request naming a dataset the service does not hold was served a different one", w.Code)
	}
	// Reuse is still reuse. Naming the same dataset twice hands back the same
	// installation rather than accumulating one per token request.
	if w := ask("grant_type=client_credentials&dataset=" + home); w.Code != 200 {
		t.Fatal("a repeated request for the dataset the service holds was refused", w.Code)
	}
	installations := 0
	for _, entry := range runtime.Authority.AdminOverview(t.Context(), "owner").Authorizations {
		if entry.Client == "synthetic-service" {
			installations++
		}
	}
	if installations != 1 {
		t.Fatal("repeating a request for one dataset created an installation per request", installations)
	}
}

// A deployment holds several credential files — a TLS key, a configuration, a
// database DSN — and a refusal that names none of them leaves an operator with
// a restart loop and three candidates. Reported by a consumer standing
// PostgreSQL up for the first time: the DSN carries the database password, so
// it is a credential even though it does not look like one, and 0644 is the
// mode a file gets by not being thought about.
func TestARefusedCredentialFileSaysWhichOneAndWhy(t *testing.T) {
	directory := t.TempDir()
	readable := filepath.Join(directory, "postgres.dsn")
	if err := os.WriteFile(readable, []byte("postgres://example"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := PrivateFile(readable)
	if err == nil {
		t.Fatal("a credential file other accounts can read was accepted")
	}
	if !strings.Contains(err.Error(), readable) {
		t.Fatal("the refusal does not name the file", err)
	}
	if !strings.Contains(err.Error(), "0644") || !strings.Contains(err.Error(), "0600") {
		t.Fatal("the refusal does not say what is wrong or what is wanted", err)
	}

	// Absent and not-a-file are different problems and say so.
	if _, err = PrivateFile(filepath.Join(directory, "absent")); err == nil || !strings.Contains(err.Error(), "absent") {
		t.Fatal("the refusal for a missing file does not name it", err)
	}
	if _, err = PrivateFile(directory); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatal("a directory was not refused as one", err)
	}
	if err = os.Chmod(readable, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = PrivateFile(readable); err != nil {
		t.Fatal("a private credential file was refused", err)
	}
}

func TestDeploymentDisplayName(t *testing.T) {
	for _, name := range []string{"", "Synthetic Home", "  Synthetic Hôme  ", strings.Repeat("a", 200)} {
		t.Run(name, func(t *testing.T) {
			config := Config{Listen: "127.0.0.1:8443", Issuer: "https://tessera.example", DataDir: t.TempDir(), DisplayName: name, TLSCertificate: "synthetic-cert", TLSKey: "synthetic-key", EncryptedStorage: true}
			rawConfig, err := json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, rawConfig, 0600); err != nil {
				t.Fatal(err)
			}
			config, err = ReadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(config.DataDir, 0700); err != nil {
				t.Fatal(err)
			}
			runtime, err := Open(t.Context(), config)
			if err != nil {
				t.Fatal(err)
			}
			defer runtime.Close()
			w := httptest.NewRecorder()
			runtime.Handler.ServeHTTP(w, httptest.NewRequest("GET", "/.well-known/tessera", nil))
			var document map[string]json.RawMessage
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &document) != nil {
				t.Fatal("discovery is not readable")
			}
			raw, present := document["display_name"]
			if present != (name != "") {
				t.Fatal("display_name presence does not match configuration")
			}
			if present {
				var got string
				if json.Unmarshal(raw, &got) != nil || got != name {
					t.Fatal("display_name does not preserve configured text")
				}
			}
			var software map[string]json.RawMessage
			if err := json.Unmarshal(document["server"], &software); err != nil {
				t.Fatal(err)
			}
			if _, present := software["display_name"]; present {
				t.Fatal("deployment name is inside software metadata")
			}
		})
	}
}

func TestInvalidDeploymentDisplayNameStopsStartup(t *testing.T) {
	for _, name := range []string{" \t ", "Synthetic\nHome", "Synthetic\x00Home", "Synthetic\x7fHome", string([]byte{0xff}), "\ufffd", strings.Repeat("a", 201)} {
		t.Run(name, func(t *testing.T) {
			config := Config{Issuer: "https://tessera.example", DataDir: t.TempDir(), DisplayName: name}
			if err := os.Chmod(config.DataDir, 0700); err != nil {
				t.Fatal(err)
			}
			runtime, err := Open(t.Context(), config)
			if runtime != nil {
				runtime.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "display_name") {
				t.Fatal("invalid display_name did not stop startup with a configuration error", err)
			}
		})
	}
}

func TestIdentityBrandConfiguration(t *testing.T) {
	for _, valid := range []bool{false, true} {
		t.Run(fmt.Sprint(valid), func(t *testing.T) {
			config := Config{Issuer: "https://tessera.example", Listen: "127.0.0.1:8443", DataDir: t.TempDir(), TLSCertificate: "synthetic-cert", TLSKey: "synthetic-key", EncryptedStorage: true,
				IdentityBrand: &identity.BrandConfig{Name: "Operator Home", Light: &identity.BrandPalette{Accent: "#174b91"}, WebClientIDs: []string{"console"}}}
			if !valid {
				config.IdentityBrand.Light.Accent = "#eeeeee"
			}
			raw, err := json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "config.json")
			if err = os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			config, err = ReadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.Chmod(config.DataDir, 0700); err != nil {
				t.Fatal(err)
			}
			runtime, err := Open(t.Context(), config)
			if !valid {
				if runtime != nil {
					runtime.Close()
				}
				if err == nil || !strings.Contains(err.Error(), "identity_brand") {
					t.Fatal("unsafe branding did not stop startup", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer runtime.Close()
			w := httptest.NewRecorder()
			runtime.Handler.ServeHTTP(w, httptest.NewRequest("GET", "/oauth/account/delete", nil))
			if w.Code != 200 || !strings.Contains(w.Body.String(), "<strong>Operator Home</strong>") || !strings.Contains(w.Body.String(), "#174b91") {
				t.Fatal("configuration did not reach identity pages")
			}
		})
	}
}

func TestFirstPartyConsoleStartupConfiguration(t *testing.T) {
	for _, valid := range []bool{false, true} {
		t.Run(fmt.Sprint(valid), func(t *testing.T) {
			cfg := Config{Issuer: "https://tessera.example", Listen: "127.0.0.1:8443", DataDir: t.TempDir(), TLSCertificate: "synthetic-cert", TLSKey: "synthetic-key", EncryptedStorage: true, FirstPartyConsoles: []identity.ConsoleConfig{{ClientID: "operator-console", Name: "Console", Origin: "https://console.example", RedirectURI: "https://console.example/callback", DataGrants: []string{"read", "write"}, Administer: true}}}
			if !valid {
				cfg.FirstPartyConsoles[0].RedirectURI = "https://other.example/callback"
			}
			raw, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "server.json")
			if err = os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err = ReadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.Chmod(cfg.DataDir, 0700); err != nil {
				t.Fatal(err)
			}
			runtime, err := Open(t.Context(), cfg)
			if !valid {
				if runtime != nil {
					runtime.Close()
				}
				if err == nil || !strings.Contains(err.Error(), "first_party_consoles") {
					t.Fatal("unsafe Console binding did not stop startup", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = runtime.Close(); err != nil {
				t.Fatal(err)
			}
			// A second startup recognizes the reserved identity, rather than rejecting
			// its own persisted client or converting it to public registration.
			runtime, err = Open(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer runtime.Close()
		})
	}
}

func TestDeploymentDeprecationConfiguration(t *testing.T) {
	for _, sunset := range []string{"", "2030-01-01T00:00:00Z", "2000-01-01T00:00:00Z"} {
		t.Run(sunset, func(t *testing.T) {
			config := Config{Listen: "127.0.0.1:8443", Issuer: "https://tessera.example", DataDir: t.TempDir(), TLSCertificate: "synthetic-cert", TLSKey: "synthetic-key", EncryptedStorage: true}
			if sunset != "" {
				config.DeprecatedVersions = []p.DeprecatedVersion{{Version: "1", Sunset: sunset}}
			}
			raw, err := json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			config, err = ReadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(config.DataDir, 0700); err != nil {
				t.Fatal(err)
			}
			runtime, err := Open(t.Context(), config)
			if err != nil {
				t.Fatal(err)
			}
			defer runtime.Close()
			w := httptest.NewRecorder()
			runtime.Handler.ServeHTTP(w, httptest.NewRequest("GET", "/.well-known/tessera", nil))
			var doc struct {
				Deprecated []p.DeprecatedVersion `json:"deprecated_versions"`
			}
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &doc) != nil {
				t.Fatal("discovery failed")
			}
			if sunset == "" {
				if len(doc.Deprecated) != 0 {
					t.Fatal("unexpected deprecation")
				}
			} else if len(doc.Deprecated) != 1 || doc.Deprecated[0].Sunset != sunset {
				t.Fatal("configuration not served")
			}
		})
	}
}

func TestInvalidDeprecationDoesNotCreateStorage(t *testing.T) {
	config := Config{DataDir: filepath.Join(t.TempDir(), "must-not-exist"), DeprecatedVersions: []p.DeprecatedVersion{{Version: "2", Sunset: "2030-01-01T00:00:00Z"}}}
	runtime, err := Open(t.Context(), config)
	if runtime != nil {
		runtime.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "deprecated_versions") {
		t.Fatal("invalid configuration accepted", err)
	}
	if _, err := os.Stat(config.DataDir); !os.IsNotExist(err) {
		t.Fatal("invalid configuration touched storage")
	}
}
