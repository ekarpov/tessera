package sqlengine

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ekarpov/tessera/internal/dbtest"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

func TestContributionInventoryAndErasure(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			var s *Store
			var reopen func() (*Store, error)
			if backend == "postgres" {
				postgresAdmin(t)
				s = postgresFixture(t)
				path := s.path
				reopen = func() (*Store, error) {
					return OpenPostgres(ctx, dbtest.Postgres(t), path, p.DevelopmentLimits())
				}
			} else {
				var path string
				s, path, _ = fixture(t)
				reopen = func() (*Store, error) { return Open(ctx, path, p.DevelopmentLimits()) }
			}
			dataset := sole(t, s)
			member := store.Access{Principal: "member", Authorization: "first"}
			second := store.Access{Principal: member.Principal, Authorization: "second"}
			full := store.Authorization{View: store.FullView(), Grants: store.Grants{Read: true, Write: true, Schema: true}}
			for _, a := range []store.Access{owner, member, second} {
				if err := s.SetAuthorization(ctx, dataset, a, full); err != nil {
					t.Fatal(err)
				}
			}
			const typ = "test.contributions"
			if err := s.Transact(ctx, owner, dataset, func(tx store.Tx) error {
				_, e := tx.CreateMetadata(p.Meta{ID: typ, Fields: []p.Field{
					{ID: "creator", MetaID: typ, Type: p.TextType, Creator: true},
				}})
				return e
			}); err != nil {
				t.Fatal(err)
			}
			if err := s.InstallManagedTypes(ctx, owner, dataset, []string{typ}); err != nil {
				t.Fatal(err)
			}
			ids := make([]string, 0, 3)
			for _, a := range []store.Access{member, second, owner} {
				if err := s.Transact(ctx, a, dataset, func(tx store.Tx) error {
					set, e := tx.Insert(typ, nil)
					if e == nil {
						ids = append(ids, set.Records[0].ID.ID)
					}
					return e
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Transact(ctx, owner, dataset, func(tx store.Tx) error {
				_, e := tx.UpdateMetadata(p.Meta{ID: typ}, typ)
				return e
			}); err != nil {
				t.Fatal(err)
			}
			page, next, err := s.Contributions(ctx, dataset, member.Principal, "", 1)
			if err != nil || len(page) != 1 || page[0] != (store.ObjectKey{TypeID: typ, ObjectID: ids[0]}) || next != ids[0] {
				t.Fatal(page, next, err)
			}
			page, next, err = s.Contributions(ctx, dataset, member.Principal, next, 1)
			if err != nil || len(page) != 1 || page[0].ObjectID != ids[1] || next != "" {
				t.Fatal(page, next, err)
			}

			dir := t.TempDir()
			before := filepath.Join(dir, "before.tessera")
			key := bytes.Repeat([]byte{7}, 32)
			if err = s.Backup(ctx, before, key); err != nil {
				t.Fatal(err)
			}
			job, err := s.BeginContributionErasure(ctx, dataset, member.Principal)
			if err != nil || job == "" {
				t.Fatal(job, err)
			}
			again, err := s.BeginContributionErasure(ctx, dataset, member.Principal)
			if err != nil || again != job {
				t.Fatal(again, err)
			}
			if err = s.SetAuthorization(ctx, dataset, second, full); p.ErrorCode(err) != "permission_denied" {
				t.Fatal("principal could regain a grant", err)
			}
			if done, err := s.StepContributionErasure(ctx, dataset, job, 1); err != nil || done {
				t.Fatal(done, err)
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = reopen()
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			for range 2 {
				if done, err := s.StepContributionErasure(ctx, dataset, job, 1); err != nil || !done {
					t.Fatal(done, err)
				}
			}
			page, _, err = s.Contributions(ctx, dataset, member.Principal, "", 10)
			if err != nil || len(page) != 0 {
				t.Fatal(page, err)
			}
			for i, id := range ids {
				err = s.ReadRecord(ctx, owner, dataset, func(tx store.Tx) error {
					_, e := tx.Object(typ, id, nil)
					return e
				})
				if i < 2 && p.ErrorCode(err) != "not_found" || i == 2 && err != nil {
					t.Fatal(i, err)
				}
			}
			restore := func(ctx context.Context, archive, destination string) error {
				return RestoreBackup(ctx, archive, destination, "", s.path+".retired", key)
			}
			if err = restore(ctx, before, filepath.Join(dir, "refused.db")); p.ErrorCode(err) != "dataset_unavailable" {
				t.Fatal("pre-erasure archive was accepted", err)
			}
			after := filepath.Join(dir, "after.tessera")
			if err = s.Backup(ctx, after, key); err != nil {
				t.Fatal(err)
			}
			reopenRestored := func(archive, path string) (*Store, error) {
				if err := restore(ctx, archive, path); err != nil {
					return nil, err
				}
				return Open(ctx, path, p.DevelopmentLimits())
			}
			if backend == "postgres" {
				dsn := dbtest.PostgresRestore(t)
				if dsn == "" {
					return
				}
				admin, err := sql.Open("tessera-postgres", dsn)
				if err != nil {
					t.Fatal(err)
				}
				defer admin.Close()
				reset := func() {
					if _, err := admin.ExecContext(ctx, `DO $$ DECLARE t record; BEGIN FOR t IN SELECT tablename FROM pg_tables WHERE schemaname='public' AND tablename LIKE 'tessera\_%' LOOP EXECUTE format('DROP TABLE IF EXISTS %I CASCADE', t.tablename); END LOOP; END $$`); err != nil {
						t.Fatal(err)
					}
				}
				reset()
				defer reset()
				reopenRestored = func(archive, path string) (*Store, error) {
					if err := RestorePostgres(ctx, archive, dsn, path, "", s.path+".retired", key); err != nil {
						return nil, err
					}
					return OpenPostgres(ctx, dsn, path, p.DevelopmentLimits())
				}
			}
			checkRestore := func(archive, path string) {
				t.Helper()
				restored, err := reopenRestored(archive, path)
				if err != nil {
					t.Fatal(err)
				}
				defer restored.Close()
				if err = restored.SetAuthorization(ctx, dataset, member, full); p.ErrorCode(err) != "permission_denied" {
					t.Fatal("restore revived a revoked principal", err)
				}
				if rows, _, err := restored.Contributions(ctx, dataset, owner.Principal, "", 2); err != nil || len(rows) != 1 || rows[0].ObjectID != ids[2] {
					t.Fatal(rows, err)
				}
			}
			checkRestore(after, filepath.Join(dir, "restored.db"))
			if backend == "sqlite" {
				flat := filepath.Join(dir, "flat.tessera")
				rewriteArchive(t, after, flat, key, func(m *backupManifest) {
					for i := range m.Tables {
						table := &m.Tables[i]
						if table.Name != "tessera_contributions" {
							continue
						}
						if !slices.Equal(table.Columns, []string{"dataset_id", "creator_id", "object_ids"}) || len(table.Rows) != 1 {
							t.Fatal("expected a packed contribution group")
						}
						group := table.Rows[0]
						var objectIDs []string
						if json.Unmarshal([]byte(*group[2].Text), &objectIDs) != nil || len(objectIDs) != 1 {
							t.Fatal("packed contribution is missing its object")
						}
						table.Columns = []string{"dataset_id", "object_id", "creator_id"}
						table.Rows = [][]backupValue{{group[0], {Kind: "text", Text: &objectIDs[0]}, group[1]}}
					}
				})
				checkRestore(flat, filepath.Join(dir, "flat.db"))
			}
		})
	}
}
