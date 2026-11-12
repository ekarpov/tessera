package sqlengine

import (
	"bytes"
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/ekarpov/tessera/pkg/conformance"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/server"
	"github.com/ekarpov/tessera/pkg/store"
)

func TestScopedAttachmentConformance(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var s *Store
			if backend == "postgres" {
				postgresAdmin(t)
				s = postgresFixture(t)
			} else {
				s, _, _ = fixture(t)
			}
			ctx := t.Context()
			dataset := sole(t, s)
			full := store.Authorization{View: store.FullView(), Grants: store.Grants{Read: true, Write: true, Schema: true}}
			if err := s.SetAuthorization(ctx, dataset, owner, full); err != nil {
				t.Fatal(err)
			}
			if err := s.EnableContent(ctx, filepath.Join(t.TempDir(), "bytes")); err != nil {
				t.Fatal(err)
			}
			access := func(token string) store.Access {
				return store.Access{Principal: token, Authorization: "test"}
			}
			http := httptest.NewUnstartedServer(nil)
			h, err := server.NewWithOptions(s, func(_ context.Context, token string) (store.Access, error) {
				switch token {
				case "owner":
					return owner, nil
				case "contributor", "other":
					return access(token), nil
				default:
					return store.Access{}, store.Refuse("authentication_failed")
				}
			}, server.Options{AuthMethods: []string{"bearer_token"}, ContentOrigin: "https://" + http.Listener.Addr().String()})
			if err != nil {
				t.Fatal(err)
			}
			http.Config.Handler = h
			http.StartTLS()
			defer http.Close()
			err = conformance.CheckScopedAttachments(ctx, conformance.ScopedAttachmentFixture{
				URL: http.URL, Client: http.Client(), Owner: "owner", Contributor: "contributor", Other: "other",
				Grant: func(ctx context.Context, token, typ string, insert, update bool) error {
					scopes := []store.CollectionScope{{TypeID: typ, ID: "alpha"}, {TypeID: typ, ID: "beta"}}
					var insertScopes, updateScopes []store.CollectionScope
					if insert {
						insertScopes = scopes
					}
					if update {
						updateScopes = scopes
					}
					view, err := store.SharedView(nil).WithCollections(scopes, insertScopes)
					if err != nil {
						return err
					}
					view, err = view.WithOwnMutations(updateScopes, nil)
					if err != nil {
						return err
					}
					return s.SetAuthorization(ctx, dataset, access(token), store.Authorization{
						View: view, Grants: store.Grants{Read: true, Insert: insert, UpdateOwn: update},
					})
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if backend == "sqlite" {
				checkScopedAttachmentRestore(t, s, dataset, access("contributor"))
			}
		})
	}
}

func checkScopedAttachmentRestore(t *testing.T, s *Store, dataset string, contributor store.Access) {
	t.Helper()
	ctx := t.Context()
	dir := t.TempDir()
	archive, destination, content := filepath.Join(dir, "snapshot.enc"), filepath.Join(dir, "restored.db"), filepath.Join(dir, "bytes")
	key := bytes.Repeat([]byte{23}, 32)
	if err := s.Backup(ctx, archive, key); err != nil {
		t.Fatal(err)
	}
	if err := RestoreBackup(ctx, archive, destination, content, s.path+".retired", key); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(ctx, destination, p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	var count int
	if err = restored.db.QueryRowContext(ctx, `SELECT count(*) FROM "tessera_imports" WHERE "attachment_target" IS NOT NULL`).Scan(&count); err != nil || count != 3 {
		t.Fatal("archive lost scoped imports", count, err)
	}
	job, err := restored.BeginContributionErasure(ctx, dataset, contributor.Principal)
	if err != nil {
		t.Fatal(err)
	}
	for {
		done, err := restored.StepContributionErasure(ctx, dataset, job, 1)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			break
		}
	}
	if err = restored.db.QueryRowContext(ctx, `SELECT count(*) FROM "tessera_items" WHERE "deleted"=0`).Scan(&count); err != nil || count != 0 {
		t.Fatal("erasure retained a scoped item", count, err)
	}
	if err = restored.db.QueryRowContext(ctx, `SELECT count(*) FROM "tessera_imports" WHERE "state"='open'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("erasure retained an open scoped import", count, err)
	}
}
