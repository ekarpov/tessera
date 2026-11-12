package sqlengine

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ekarpov/tessera/pkg/conformance"
	"github.com/ekarpov/tessera/pkg/server"
	"github.com/ekarpov/tessera/pkg/store"
)

func TestCollectionConformance(t *testing.T) {
	for _, engine := range []string{"sqlite", "postgres"} {
		t.Run(engine, func(t *testing.T) {
			var s *Store
			if engine == "postgres" {
				postgresAdmin(t)
				s = postgresFixture(t)
			} else {
				s, _, _ = fixture(t)
			}
			dataset := sole(t, s)
			full := store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true}, View: store.FullView()}
			if err := s.SetAuthorization(t.Context(), dataset, owner, full); err != nil {
				t.Fatal(err)
			}
			access := func(name string) store.Access { return store.Access{Principal: name, Authorization: name} }
			h, err := server.New(s, func(_ context.Context, token string) (store.Access, error) {
				switch token {
				case "owner":
					return owner, nil
				case "reader", "contributor":
					return access(token), nil
				default:
					return store.Access{}, store.Refuse("authentication_failed")
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			listener := httptest.NewServer(h)
			defer listener.Close()
			err = conformance.CheckCollections(t.Context(), conformance.CollectionFixture{
				URL: listener.URL, Owner: "owner", Client: listener.Client(),
				SetOwn: func(ctx context.Context, name string, read, insert, update, remove []store.CollectionScope) (string, error) {
					view, err := store.SharedView(nil).WithCollections(read, insert)
					if err == nil {
						view, err = view.WithOwnMutations(update, remove)
					}
					if err != nil {
						return "", err
					}
					grants := store.Grants{Read: len(read) > 0, Insert: len(insert) > 0, UpdateOwn: len(update) > 0, DeleteOwn: len(remove) > 0}
					return name, s.SetAuthorization(ctx, dataset, access(name), store.Authorization{View: view, Grants: grants})
				},
				Set: func(ctx context.Context, name string, read, insert []store.CollectionScope, expires *time.Time) (string, error) {
					view, err := store.SharedView(nil).WithCollections(read, insert)
					if err != nil {
						return "", err
					}
					grant := store.Authorization{View: view, Grants: store.Grants{Read: len(read) > 0, Insert: len(insert) > 0, ExpiresAt: expires}}
					return name, s.SetAuthorization(ctx, dataset, access(name), grant)
				},
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
