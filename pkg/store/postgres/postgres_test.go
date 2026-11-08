package postgres_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"github.com/ekarpov/tessera/internal/dbtest"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/ekarpov/tessera/pkg/conformance"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/server"
	"github.com/ekarpov/tessera/pkg/store"
	"github.com/ekarpov/tessera/pkg/store/postgres"
)

func TestPostgresConformance(t *testing.T) {
	dsn := dbtest.Postgres(t)
	// The engine package also tests against this database, and package tests run
	// at the same time. This both excludes them and starts from nothing.
	disposable(t, dsn)
	fence := filepath.Join(t.TempDir(), "retirement-fence")
	s, err := postgres.Open(t.Context(), dsn, fence, p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	owner := store.Access{Principal: "synthetic", Authorization: "owner"}
	for name, g := range map[string]store.Grants{"owner": {Read: true, Write: true, Schema: true, Erase: true}, "refused": {Write: true}} {
		if err = s.SetAuthorization(t.Context(), sole(t, s), store.Access{Principal: "synthetic", Authorization: name}, store.Authorization{Grants: g, View: store.FullView()}); err != nil {
			t.Fatal(err)
		}
	}
	authenticate := func(_ context.Context, token string) (store.Access, error) {
		if token == "guest" || token == "contributor" {
			return store.Access{Principal: token, Authorization: token}, nil
		}
		if token != "owner" && token != "refused" {
			return store.Access{}, store.Refuse("authentication_failed")
		}
		return store.Access{Principal: "synthetic", Authorization: token}, nil
	}
	httpServer := httptest.NewUnstartedServer(nil)
	if err = s.EnableContent(t.Context(), filepath.Join(t.TempDir(), "content")); err != nil {
		t.Fatal(err)
	}
	h, err := server.NewWithOptions(s, authenticate, server.Options{DisplayName: "Synthetic Home", AuthMethods: []string{"bearer_token"}, ContentOrigin: "http://" + httpServer.Listener.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	httpServer.Config.Handler = h
	httpServer.Start()
	defer httpServer.Close()
	// Grant administration is outside the protocol, so the host installs the
	// guest the sharing suite reads as.
	share := func(ctx context.Context, types []string) (string, error) {
		access := store.Access{Principal: "guest", Authorization: "guest"}
		auth := store.Authorization{Grants: store.Grants{Read: true}, View: store.SharedView(types)}
		if e := s.SetAuthorization(ctx, sole(t, s), access, auth); e != nil {
			return "", e
		}
		return "guest", nil
	}
	contribute := func(ctx context.Context, types []string) (string, error) {
		access := store.Access{Principal: "contributor", Authorization: "contributor"}
		auth := store.Authorization{Grants: store.Grants{Insert: true}, View: store.SharedView(nil).WithInserts(types)}
		if e := s.SetAuthorization(ctx, sole(t, s), access, auth); e != nil {
			return "", e
		}
		return "contributor", nil
	}
	fixture := conformance.CoreFixture{URL: httpServer.URL, Token: "owner", RefusedToken: "refused", Client: httpServer.Client(), Share: share, Contribute: contribute}
	for _, check := range []func(context.Context, conformance.CoreFixture) error{conformance.CheckCore, conformance.CheckRecovery, conformance.CheckPaging, conformance.CheckContent} {
		if err = check(t.Context(), fixture); err != nil {
			t.Fatal(err)
		}
	}
	// Withheld first, so the refusals are checked against this engine too, and
	// then advertised, so the behaviour is checked on the same rows.
	if err = conformance.CheckSharing(t.Context(), fixture); err != nil {
		t.Fatal(err)
	}
	if err = conformance.CheckOptionalReads(t.Context(), fixture); err != nil {
		t.Fatal(err)
	}
	if err = conformance.CheckOptionalReads(t.Context(), fixture); err != nil {
		t.Fatal(err)
	}

	// The dataset this composition writes to, named through the listing of what
	// the owner may use.
	listed, err := s.Datasets(t.Context(), owner)
	if err != nil || len(listed) != 1 {
		t.Fatal("expected exactly one dataset", err)
	}
	dataset := listed[0].ID
	if err = s.WithTx(t.Context(), owner, dataset, func(_ store.Tx, host *sql.Tx) error {
		_, e := host.ExecContext(t.Context(), `CREATE TABLE IF NOT EXISTS "host_events" ("id" TEXT PRIMARY KEY)`)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	hostFailure := errors.New("synthetic host rollback")
	err = s.WithTx(t.Context(), owner, dataset, func(core store.Tx, host *sql.Tx) error {
		if _, e := core.CreateMetadata(p.Meta{ID: "test.host_rollback"}); e != nil {
			return e
		}
		if _, e := host.ExecContext(t.Context(), `INSERT INTO "host_events" ("id") VALUES ($1)`, "rolled-back"); e != nil {
			return e
		}
		return hostFailure
	})
	if !errors.Is(err, hostFailure) {
		t.Fatal(err)
	}
	if err = s.WithTx(t.Context(), owner, dataset, func(core store.Tx, host *sql.Tx) error {
		var count int
		if e := host.QueryRowContext(t.Context(), `SELECT count(*) FROM "host_events"`).Scan(&count); e != nil {
			return e
		}
		if count != 0 {
			t.Fatal("host write escaped rollback")
		}
		_, e := core.Metadata("test.host_rollback")
		if p.ErrorCode(e) != "meta_type_not_found" {
			t.Fatal("record schema escaped rollback", e)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	info, err := s.Info(t.Context(), owner, dataset)
	if err != nil {
		t.Fatal(err)
	}

	restoreDSN := dbtest.PostgresRestore(t)
	archive := filepath.Join(t.TempDir(), "postgres.enc")
	backupKey := bytes.Repeat([]byte{17}, 32)
	var restoreAdmin *sql.DB
	if restoreDSN != "" {
		// The restore target is made disposable for the same reason the source
		// is. A restore refuses a database that already holds a deployment,
		// which is right — and it means this branch passed once and failed on
		// every run after it, against the same database. It had never run
		// twice: the variable it needs was usually unset, and the branch was
		// silent about that until it stopped being.
		restoreAdmin = disposable(t, restoreDSN)
		if err = s.Backup(t.Context(), archive, backupKey); err != nil {
			t.Fatal(err)
		}
		restoredFence := filepath.Join(t.TempDir(), "restore-fence")
		restoredContent := filepath.Join(t.TempDir(), "restored-content")
		if err = postgres.RestoreBackup(t.Context(), archive, restoreDSN, restoredFence, restoredContent, fence+".retired", backupKey); err != nil {
			t.Fatal(err)
		}
		restored, e := postgres.Open(t.Context(), restoreDSN, restoredFence, p.DevelopmentLimits())
		if e != nil {
			t.Fatal(e)
		}
		restoredInfo, e := restored.Info(t.Context(), owner, info.ID)
		if e != nil || restoredInfo.ID != info.ID || restoredInfo.Watermark == info.Watermark {
			t.Fatal("PostgreSQL restore changed identity or retained generation", e)
		}
		if e = restored.EnableContent(t.Context(), restoredContent); e != nil {
			t.Fatal(e)
		}
		if e = restored.Close(); e != nil {
			t.Fatal(e)
		}
	}
	result, err := s.Erase(t.Context(), owner, info.ID, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if restoreDSN != "" {
		// A backup from before the erasure restores, and the erasure is applied
		// again on the way in: the erased identity does not come back.
		dropTessera(t, restoreAdmin)
		reappliedFence := filepath.Join(t.TempDir(), "reapplied-fence")
		if err = postgres.RestoreBackup(t.Context(), archive, restoreDSN, reappliedFence, "", fence+".retired", backupKey); err != nil {
			t.Fatal("a PostgreSQL backup from before an erasure could not be restored", err)
		}
		restored, e := postgres.Open(t.Context(), restoreDSN, reappliedFence, p.DevelopmentLimits())
		if e != nil {
			t.Fatal(e)
		}
		held, e := restored.Provisioned(t.Context())
		for _, d := range held {
			if d.ID == info.ID {
				e = errors.New("resurrected")
			}
		}
		_ = restored.Close()
		if e != nil {
			t.Fatal("PostgreSQL restore resurrected an erased dataset", e)
		}
	}
	if result.DatasetID == info.ID {
		t.Fatal("erase reused identity")
	}
	if err = s.CheckpointCleanup(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = postgres.Open(t.Context(), dsn, fence, p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	info, err = s.Info(t.Context(), owner, result.DatasetID)
	if err != nil || info.ID != result.DatasetID {
		t.Fatal("restart lost replacement", err)
	}
}

// sole names the dataset a store holds, through the composition call rather
// than the listing: a grant has to be installed on a dataset before any listing
// can report one.
func sole(t *testing.T, s *postgres.Store) string {
	t.Helper()
	dataset, err := s.SoleDataset(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return dataset.ID
}
