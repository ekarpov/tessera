package standalone

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ekarpov/tessera/internal/dbtest"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
	"github.com/ekarpov/tessera/pkg/store/postgres"
	"github.com/ekarpov/tessera/pkg/store/sqlite"
)

// Includes the real archive/retirement fence, startup, OAuth chooser, PKCE
// approval, old-token refusal and a second restart. A missing chooser entry
// alone would hide the stale grant without fixing recovery authority.
func TestRestoreReconcilesIdentityBeforeAuthorization(t *testing.T) {
	for _, engine := range []string{"sqlite", "postgres"} {
		t.Run(engine, func(t *testing.T) {
			dir := t.TempDir()
			backups := filepath.Join(dir, "backups")
			if err := os.Mkdir(backups, 0700); err != nil {
				t.Fatal(err)
			}
			key := bytes.Repeat([]byte{7}, 32)
			keyFile := filepath.Join(dir, "key")
			if err := os.WriteFile(keyFile, key, 0600); err != nil {
				t.Fatal(err)
			}
			sourceConfig := Config{Issuer: "https://tessera.example", DataDir: filepath.Join(dir, "source"), Engine: engine, EncryptedStorage: true, Backup: &BackupConfig{Directory: backups, KeyFile: keyFile, Keep: 3, EveryHours: 0}}
			restoreConfig := Config{Issuer: sourceConfig.Issuer, DataDir: filepath.Join(dir, "restored"), Engine: engine, EncryptedStorage: true}
			var destinationDSN string
			if engine == "postgres" {
				sourceDSN := dbtest.Postgres(t)
				destinationDSN = dbtest.PostgresRestore(t)
				if destinationDSN == "" {
					t.Skip("requires disposable restore database")
				}
				restoreIdentityDatabase(t, sourceDSN)
				restoreIdentityDatabase(t, destinationDSN)
				sourceConfig.PostgresDSNFile = filepath.Join(dir, "source-dsn")
				restoreConfig.PostgresDSNFile = filepath.Join(dir, "restore-dsn")
				if err := os.WriteFile(sourceConfig.PostgresDSNFile, []byte(sourceDSN), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(restoreConfig.PostgresDSNFile, []byte(destinationDSN), 0600); err != nil {
					t.Fatal(err)
				}
			}
			runtime, err := Open(t.Context(), sourceConfig)
			if err != nil {
				t.Fatal(err)
			}
			defer runtime.Close()
			erased := sole(t, runtime.Engine)
			full := store.Grants{Read: true, Write: true, Schema: true, Erase: true}
			if err := runtime.Authority.AddServerAdministrator("owner", "synthetic-password-only", erased, full, false); err != nil {
				t.Fatal(err)
			}
			setup := store.Access{Principal: "setup", Authorization: "setup"}
			made, err := runtime.Engine.CreateDataset(t.Context(), setup, "Kept", p.Limits{}, []p.Meta{{ID: "test.kept", Fields: []p.Field{{ID: "n", MetaID: "test.kept", Type: p.IntegerType}}}}, "", [32]byte{})
			if err != nil {
				t.Fatal(err)
			}
			if err := runtime.Authority.RecordCreation("owner", made.DatasetID, full); err != nil {
				t.Fatal(err)
			}
			value, _ := p.NewValue(p.IntegerType, 42)
			if err := runtime.Engine.Transact(t.Context(), setup, made.DatasetID, func(tx store.Tx) error { _, err := tx.Insert("test.kept", map[string]p.Value{"n": value}); return err }); err != nil {
				t.Fatal(err)
			}
			oldToken := restoredOAuth(t, runtime, erased, erased, "")
			archive, err := runtime.BackupNow(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			eraser := store.Access{Principal: "owner", Authorization: "erasing"}
			if err := runtime.Engine.SetAuthorization(t.Context(), erased, eraser, store.Authorization{Grants: full, View: store.FullView()}); err != nil {
				t.Fatal(err)
			}
			if _, err := runtime.Engine.Erase(t.Context(), eraser, erased, erased); err != nil {
				t.Fatal(err)
			}
			fenceName := "dataset.db.retired"
			if engine == "postgres" {
				fenceName = "postgres-fence.retired"
			}
			fence := filepath.Join(sourceConfig.DataDir, fenceName)
			if err := runtime.Close(); err != nil {
				t.Fatal(err)
			}
			if engine == "postgres" {
				err = postgres.RestoreBackup(t.Context(), archive, destinationDSN, filepath.Join(restoreConfig.DataDir, "postgres-fence"), "", fence, key, restoreConfig.DataDir)
			} else {
				err = sqlite.RestoreBackup(t.Context(), archive, filepath.Join(restoreConfig.DataDir, "dataset.db"), "", fence, key, restoreConfig.DataDir)
			}
			if err != nil {
				t.Fatal(err)
			}
			for restart := 0; restart < 2; restart++ {
				restored, err := Open(t.Context(), restoreConfig)
				if err != nil {
					t.Fatal(err)
				}
				func() {
					defer restored.Close()
					token := restoredOAuth(t, restored, "", made.DatasetID, erased)
					call := func(path, bearer string, status int) []byte {
						t.Helper()
						req := httptest.NewRequest("GET", path, nil)
						req.Header.Set("Authorization", "Bearer "+bearer)
						w := httptest.NewRecorder()
						restored.Handler.ServeHTTP(w, req)
						if w.Code != status {
							t.Fatalf("restored access status: want %d got %d", status, w.Code)
						}
						return w.Body.Bytes()
					}
					call("/v1/datasets", oldToken, 401)
					call("/v1/datasets/"+erased+"/meta", token, 404)
					raw := call("/v1/datasets", token, 200)
					var listing struct {
						Datasets []struct {
							ID string `json:"id"`
						} `json:"datasets"`
					}
					if json.Unmarshal(raw, &listing) != nil || len(listing.Datasets) != 1 || listing.Datasets[0].ID != made.DatasetID {
						t.Fatal("restored installation lost live access or acquired replacement")
					}
					access, err := restored.Authority.Authenticate(t.Context(), token)
					if err != nil {
						t.Fatal(err)
					}
					if err := restored.Engine.Transact(t.Context(), access, made.DatasetID, func(tx store.Tx) error {
						set, err := tx.Query("test.kept", p.Query{}, 10)
						if err != nil {
							return err
						}
						if len(set.Records) != 1 || set.Records[0].Values[0].Payload() != int64(42) {
							t.Fatal("unaffected record changed")
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}()
			}
		})
	}
}

func restoredOAuth(t *testing.T, runtime *Runtime, named, chosen, absent string) string {
	t.Helper()
	srv := httptest.NewServer(runtime.Handler)
	defer srv.Close()
	client := srv.Client()
	client.Jar, _ = cookiejar.New(nil)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	call := func(method, path, body string, status int) ([]byte, http.Header) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal("OAuth request failed")
		}
		defer resp.Body.Close()
		var buf bytes.Buffer
		buf.ReadFrom(resp.Body)
		if resp.StatusCode != status {
			t.Fatalf("OAuth expected %d got %d", status, resp.StatusCode)
		}
		return buf.Bytes(), resp.Header
	}
	req := httptest.NewRequest("POST", "/oauth/register", strings.NewReader(`{"client_name":"Synthetic restore","redirect_uris":["com.example:/callback"],"token_endpoint_auth_method":"none","grant_types":["authorization_code"],"response_types":["code"]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	runtime.Handler.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatal("client registration failed")
	}
	var registration struct {
		ID string `json:"client_id"`
	}
	if json.Unmarshal(w.Body.Bytes(), &registration) != nil {
		t.Fatal("client registration encoding")
	}
	verifier := strings.Repeat("x", 43)
	hash := sha256.Sum256([]byte(verifier))
	query := url.Values{"client_id": {registration.ID}, "redirect_uri": {"com.example:/callback"}, "response_type": {"code"}, "state": {"restore"}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(hash[:])}, "scope": {"read write schema erase administer"}}
	if named != "" {
		query.Set("dataset", named)
	}
	hidden := func(raw []byte, name string) string {
		t.Helper()
		m := regexp.MustCompile(`name="` + name + `"\s+value="([^"]*)"`).FindSubmatch(raw)
		if len(m) != 2 {
			t.Fatal("missing OAuth field")
		}
		return string(m[1])
	}
	raw, _ := call("GET", "/oauth/authorize?"+query.Encode(), "", 200)
	flow := hidden(raw, "flow")
	approval, _ := call("POST", "/oauth/authorize", url.Values{"flow": {flow}, "csrf": {hidden(raw, "csrf")}, "username": {"owner"}, "password": {"synthetic-password-only"}, "decision": {"continue"}}.Encode(), 200)
	if absent != "" && bytes.Contains(approval, []byte(absent)) {
		t.Fatal("restored OAuth offers a retired dataset")
	}
	_, headers := call("POST", "/oauth/authorize", url.Values{"flow": {flow}, "csrf": {hidden(approval, "csrf")}, "decision": {"allow"}, "dataset": {chosen}, "read": {"yes"}, "write": {"yes"}, "schema": {"yes"}, "erase": {"yes"}, "administer": {"yes"}}.Encode(), 303)
	target, err := url.Parse(headers.Get("Location"))
	if err != nil {
		t.Fatal("callback encoding")
	}
	raw, _ = call("POST", "/oauth/token", url.Values{"grant_type": {"authorization_code"}, "client_id": {registration.ID}, "redirect_uri": {"com.example:/callback"}, "code": {target.Query().Get("code")}, "code_verifier": {verifier}}.Encode(), 200)
	var token struct {
		Access string `json:"access_token"`
	}
	if json.Unmarshal(raw, &token) != nil || token.Access == "" {
		t.Fatal("no restored token")
	}
	return token.Access
}

// Shares the database-wide test guard with sqlengine and pkg/store/postgres.
func restoreIdentityDatabase(t *testing.T, dsn string) {
	t.Helper()
	db, err := sql.Open("tessera-postgres", dsn)
	if err != nil {
		t.Fatal("open disposable database")
	}
	guard, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal("connect disposable database")
	}
	const key int64 = 0x7e57000000000001
	if _, err := guard.ExecContext(context.Background(), `SELECT pg_advisory_lock($1)`, key); err != nil {
		t.Fatal("lock disposable database")
	}
	reset := func() {
		if _, err := db.ExecContext(context.Background(), `DO $$ DECLARE t record; BEGIN FOR t IN SELECT tablename FROM pg_tables WHERE schemaname='public' AND tablename LIKE 'tessera\_%' LOOP EXECUTE format('DROP TABLE IF EXISTS %I CASCADE', t.tablename); END LOOP; END $$`); err != nil {
			t.Fatal("reset disposable database")
		}
	}
	t.Cleanup(func() {
		reset()
		guard.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, key)
		guard.Close()
		db.Close()
	})
	reset()
}
