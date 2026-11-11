package standalone

import (
	"bytes"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekarpov/tessera/internal/dbtest"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
	"github.com/ekarpov/tessera/pkg/store/postgres"
	"github.com/ekarpov/tessera/pkg/store/sqlite"
)

func TestManagedDatasetProvisioningAndRestore(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			dir := t.TempDir()
			backups := filepath.Join(dir, "backups")
			if err := os.Mkdir(backups, 0700); err != nil {
				t.Fatal(err)
			}
			writePrivate := func(name string, data []byte) string {
				t.Helper()
				path := filepath.Join(dir, name)
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				return path
			}
			key := bytes.Repeat([]byte{7}, 32)
			const approved = `[
				{"id":"example.base","fields":[{"id":"stamp","meta_id":"example.base","type":"date_time"}]},
				{"id":"example.item","parent_id":"example.base","fields":[{"id":"rank","meta_id":"example.item","type":"integer"}]}
			]`
			source := Config{
				Issuer: "https://tessera.example", DataDir: filepath.Join(dir, "source"), Engine: backend,
				EncryptedStorage: true, DatasetCreation: true, ManagedTypesFile: writePrivate("managed.json", []byte(`{"types":`+approved+`}`)),
				Backup: &BackupConfig{Directory: backups, KeyFile: writePrivate("key", key), Keep: 1},
			}
			destination := Config{Issuer: source.Issuer, DataDir: filepath.Join(dir, "destination"), Engine: backend, EncryptedStorage: true}
			driver, from, to := "sqlite3", filepath.Join(source.DataDir, "dataset.db"), filepath.Join(destination.DataDir, "dataset.db")
			fence := from + ".retired"
			if backend == "postgres" {
				driver, from, to = "tessera-postgres", dbtest.Postgres(t), dbtest.PostgresRestore(t)
				if to == "" {
					t.Skip("requires disposable restore database")
				}
				restoreIdentityDatabase(t, from)
				restoreIdentityDatabase(t, to)
				source.PostgresDSNFile = writePrivate("source-dsn", []byte(from))
				destination.PostgresDSNFile = writePrivate("restore-dsn", []byte(to))
				fence = filepath.Join(source.DataDir, "postgres-fence.retired")
			}
			runtime, err := Open(ctx, source)
			if err != nil {
				t.Fatal(err)
			}
			defer runtime.Close()
			home := sole(t, runtime.Engine)
			full := store.Grants{Read: true, Write: true, Schema: true, Erase: true}
			if err := runtime.Authority.AddServerAdministrator("owner", "synthetic-password-only", home, full, true); err != nil {
				t.Fatal(err)
			}
			token, err := runtime.Authority.AddAutomation(ctx, "owner", home)
			if err != nil {
				t.Fatal(err)
			}
			call := func(runtime *Runtime, token, method, path, body, requestKey string, status int) []byte {
				t.Helper()
				req := httptest.NewRequestWithContext(ctx, method, path, strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer "+token)
				req.Header.Set("Content-Type", "application/json")
				if requestKey != "" {
					req.Header.Set("Idempotency-Key", requestKey)
				}
				w := httptest.NewRecorder()
				runtime.Handler.ServeHTTP(w, req)
				if w.Code != status {
					t.Fatalf("%s: got %d, want %d", method, w.Code, status)
				}
				return w.Body.Bytes()
			}
			create := func(types, requestKey string) string {
				t.Helper()
				raw := call(runtime, token, http.MethodPost, "/v1/datasets", `{"display_name":"Synthetic managed","types":`+types+`}`, requestKey, 201)
				var result struct {
					ID string `json:"dataset_id"`
				}
				if json.Unmarshal(raw, &result) != nil || result.ID == "" {
					t.Fatal("missing dataset identity")
				}
				return result.ID
			}
			const creationKey = "67d71648-418d-470f-9234-350d60775dbb"
			id := create(approved, creationKey)
			if got := create(approved, creationKey); got != id {
				t.Fatal("creation replay minted a different dataset")
			}
			mismatched := create(strings.Replace(approved, `"date_time"`, `"text"`, 1), "ef5cb184-8b30-42e3-a738-8bb665e3131d")
			path := "/v1/datasets/" + id
			raw := call(runtime, token, http.MethodPost, path+":transact", `{"operations":[
				{"op":"define_type","meta":{"id":"example.extra","parent_id":"example.item","fields":[{"id":"label","meta_id":"example.extra","type":"text"}]}},
				{"op":"insert","type":"example.extra","fields":{"stamp":{"type":"date_time","value":"2026-01-01T00:00:00.123Z"},"rank":{"type":"integer","value":42},"label":{"type":"text","value":"initial"}}}
			]}`, "", 200)
			var tx p.TransactionResult
			if json.Unmarshal(raw, &tx) != nil || len(tx.Results) != 2 || len(tx.Results[1].Objects) != 1 {
				t.Fatal("missing inserted object")
			}
			object := tx.Results[1].Objects[0].ID
			call(runtime, token, http.MethodPatch, path+"/objects/example.extra/"+object, `{"fields":{"rank":{"type":"integer","value":43},"label":{"type":"text","value":"updated"}}}`, "", 200)
			checkRead := func(runtime *Runtime, token string) {
				t.Helper()
				raw := call(runtime, token, http.MethodPost, path+"/objects/example.extra:query", `{"query":{"fields":["stamp","rank","label"],"result_sort":[{"field_id":"stamp","ascending":false}]},"limit":10}`, "", 200)
				var set p.RecordSet
				if json.Unmarshal(raw, &set) != nil || len(set.Records) != 1 || len(set.Records[0].Values) != 3 {
					t.Fatal("query did not return all fields on the descendant")
				}
				values := set.Records[0].Values
				if values[0].Payload() != "2026-01-01T00:00:00.123Z" || values[1].Payload() != int64(43) || values[2].Payload() != "updated" {
					t.Fatal("query did not return the updated managed and custom values")
				}
			}
			checkStorage := func(dsn string) {
				t.Helper()
				db, err := sql.Open(driver, dsn)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				var count int
				if err := db.QueryRowContext(ctx, `SELECT count(*) FROM "tessera_dataset"`).Scan(&count); err != nil || count != 3 {
					t.Fatal("creation replay created an additional dataset", err)
				}
				if err := db.QueryRowContext(ctx, `SELECT count(*) FROM "tessera_managed_installations" WHERE "dataset_id"=$1`, mismatched).Scan(&count); err != nil || count != 0 {
					t.Fatal("an unapproved ancestor or child was managed", err)
				}
				var label string
				if err := db.QueryRowContext(ctx, `SELECT count(*), coalesce(min("field_id"),'') FROM "tessera_field_values" WHERE "dataset_id"=$1`, id).Scan(&count, &label); err != nil || count != 1 || label != "label" {
					t.Fatal("shared storage should contain only the custom field", err)
				}
				for _, typeID := range []string{"example.base", "example.item"} {
					var layout string
					err := db.QueryRowContext(ctx, `SELECT "layout_id" FROM "tessera_managed_installations" WHERE "dataset_id"=$1 AND "type_id"=$2`, id, typeID).Scan(&layout)
					if raw, decodeErr := hex.DecodeString(layout); err != nil || decodeErr != nil || len(raw) != 16 {
						t.Fatal("missing managed layout", err)
					}
					if err = db.QueryRowContext(ctx, `SELECT count(*) FROM "tessera_m_`+layout+`" WHERE "dataset_id"=$1 AND "object_id"=$2 AND "c0" IS NOT NULL`, id, object).Scan(&count); err != nil || count != 1 {
						t.Fatal("expected physical managed field", err)
					}
				}
			}
			checkRead(runtime, token)
			checkStorage(from)
			archive, err := runtime.BackupNow(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err = runtime.Close(); err != nil {
				t.Fatal(err)
			}
			if backend == "postgres" {
				err = postgres.RestoreBackup(ctx, archive, to, filepath.Join(destination.DataDir, "postgres-fence"), "", fence, key, destination.DataDir)
			} else {
				err = sqlite.RestoreBackup(ctx, archive, to, "", fence, key, destination.DataDir)
			}
			if err != nil {
				t.Fatal(err)
			}
			restored, err := Open(ctx, destination)
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			checkRead(restored, restoredOAuth(t, restored, id, id, ""))
			checkStorage(to)
		})
	}
}

func TestManagedDatasetRejectsInvalidOperatorConfig(t *testing.T) {
	for _, body := range []string{
		`{"types":[]}`,
		`{"types":[{"id":"example.child","parent_id":"example.missing","fields":[]}]}`,
	} {
		dir := t.TempDir()
		path := filepath.Join(dir, "managed.json")
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		runtime, err := Open(t.Context(), Config{
			Issuer: "https://tessera.example", DataDir: filepath.Join(dir, "server"), ManagedTypesFile: path,
		})
		if err == nil {
			runtime.Close()
			t.Fatal("started with an invalid managed type file")
		}
	}
}
