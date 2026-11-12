package sqlengine

import (
	"bytes"
	"context"
	"path/filepath"
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
			if backend == "sqlite" {
				after := filepath.Join(dir, "after.tessera")
				if err = s.Backup(ctx, after, key); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, "restored.db")
				if err = restore(ctx, after, path); err != nil {
					t.Fatal(err)
				}
				restored, err := Open(ctx, path, p.DevelopmentLimits())
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
		})
	}
}
