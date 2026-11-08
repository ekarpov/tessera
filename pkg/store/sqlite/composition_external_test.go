package sqlite_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
	"github.com/ekarpov/tessera/pkg/store/sqlite"
)

// This host imports only public packages and joins its own SQL/event repository
// to core writes and receipt outcomes, without reaching into an engine type.
func TestExternalHostComposition(t *testing.T) {
	ctx := t.Context()
	s, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "host.db"), p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	access := store.Access{Principal: "host", Authorization: "application"}
	// Composition names the dataset the first grant is installed on.
	provisioned, err := s.SoleDataset(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetAuthorization(ctx, provisioned.ID, access, store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true}, View: store.FullView()}); err != nil {
		t.Fatal(err)
	}
	dataset := provisioned.ID
	err = s.WithTx(ctx, access, dataset, func(core store.Tx, host *sql.Tx) error {
		if _, e := host.ExecContext(ctx, `CREATE TABLE "host_events" ("id" INTEGER PRIMARY KEY)`); e != nil {
			return e
		}
		_, e := core.CreateMetadata(p.Meta{ID: "test.host"})
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	info, _ := s.Info(ctx, access, dataset)
	body := []byte(`{"operations":[{"op":"insert","type":"test.host","fields":{}}]}`)
	request, err := p.DecodeTransaction(body, p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	m := store.Mutation{Dataset: info.ID, Key: "ffffffff-ffff-ffff-ffff-000000000001", Digest: p.RequestDigest("POST", "/host/create", body), Transaction: request, SuccessStatus: 200}
	outcome, err := s.MutateWith(ctx, access, m, func(_ store.Tx, host *sql.Tx) error {
		_, e := host.ExecContext(ctx, `INSERT INTO "host_events" ("id") VALUES (1)`)
		return e
	})
	if err != nil || outcome.Status != 200 {
		t.Fatal(outcome, err)
	}
	m.Key = "ffffffff-ffff-ffff-ffff-000000000002"
	outcome, err = s.MutateWith(ctx, access, m, func(_ store.Tx, host *sql.Tx) error {
		if _, e := host.ExecContext(ctx, `INSERT INTO "host_events" ("id") VALUES (2)`); e != nil {
			return e
		}
		return store.Refuse("conflict")
	})
	if err != nil || outcome.Status != 409 {
		t.Fatal(outcome, err)
	}
	err = s.WithTx(ctx, access, dataset, func(core store.Tx, host *sql.Tx) error {
		rows, e := core.Matching("test.host", nil)
		if e != nil {
			return e
		}
		if len(rows) != 1 {
			t.Fatal("core partial commit")
		}
		var n int
		if e = host.QueryRowContext(ctx, `SELECT count(*) FROM "host_events"`).Scan(&n); e != nil {
			return e
		}
		if n != 1 {
			t.Fatal("host partial commit")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := s.Status(ctx, access, info.ID, m.Key)
	if err != nil || receipt.State != "failed" {
		t.Fatal("host rollback left committed receipt", err)
	}
}
