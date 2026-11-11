package sqlengine

import (
	"os"
	"testing"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

func TestSchemaCacheSharesDefinitionsAndTracksEdits(t *testing.T) {
	engines := map[string]func(*testing.T) *Store{
		"sqlite": func(t *testing.T) *Store { s, _, _ := fixture(t); return s },
	}
	if os.Getenv("TESSERA_TEST_POSTGRES_DSN") != "" {
		engines["postgres"] = func(t *testing.T) *Store { postgresAdmin(t); return postgresFixture(t) }
	}
	for name, open := range engines {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			s := open(t)
			parent := p.Meta{ID: "test.cache", Fields: []p.Field{
				{ID: "stamp", MetaID: "test.cache", Type: p.DateTimeType},
			}}
			child := p.Meta{ID: "test.cache.child", ParentID: &parent.ID}
			create := func() string {
				t.Helper()
				d, err := s.CreateDataset(ctx, owner, "Cache fixture", p.Limits{}, []p.Meta{parent, child}, "", [32]byte{})
				if err != nil {
					t.Fatal(err)
				}
				return d.DatasetID
			}
			first, second := create(), create()
			key := func(dataset string) string {
				t.Helper()
				var value string
				if err := s.db.QueryRowContext(ctx, `SELECT "schema_cache_key" FROM "tessera_dataset" WHERE "dataset_id"=$1`, dataset).Scan(&value); err != nil {
					t.Fatal(err)
				}
				return value
			}
			check := func(dataset string, count int) {
				t.Helper()
				if err := s.ReadSchema(ctx, owner, dataset, func(tx store.Tx) error {
					meta, err := tx.Metadata(child.ID)
					if err == nil {
						if len(meta.Fields) != count || meta.Fields[0].ID != "stamp" {
							t.Fatalf("unexpected effective fields: %+v", meta.Fields)
						}
						// A caller can mutate its result without changing the cache.
						meta.Fields[0].ID = "changed"
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			original := key(first)
			if original != key(second) {
				t.Fatal("identical datasets did not share a schema key")
			}
			check(first, 1)
			if _, ok := s.schemas.get(original); !ok {
				t.Fatal("first read did not populate the cache")
			}
			entries := len(s.schemas.entries)
			check(second, 1)
			if len(s.schemas.entries) != entries {
				t.Fatal("identical datasets did not share a cache entry")
			}
			parent.Fields = append(parent.Fields, p.Field{ID: "new_field", MetaID: parent.ID, Type: p.TextType})
			if err := s.Transact(ctx, owner, first, func(tx store.Tx) error {
				_, err := tx.UpdateMetadata(parent, "")
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if key(first) == original || key(second) != original {
				t.Fatal("schema change did not isolate its cache key")
			}
			check(first, 2)
			check(second, 1)
			if err := s.Transact(ctx, owner, second, func(tx store.Tx) error {
				_, err := tx.Insert(child.ID, nil)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if key(second) != original {
				t.Fatal("object write changed the schema key")
			}
			if s.postgres {
				// A previously started writer may still write metadata using
				// the released binary, which knows nothing of cache keys.
				raw, err := p.MarshalWire(parent)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = s.db.ExecContext(ctx, `UPDATE "tessera_types" SET "body"=$1 WHERE "dataset_id"=$2 AND "type_id"=$3`, raw, second, parent.ID); err != nil {
					t.Fatal(err)
				}
				check(second, 2)
			}

			erased, err := s.Erase(ctx, owner, first, first)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.ReadSchema(ctx, owner, erased.DatasetID, func(tx store.Tx) error {
				_, err := tx.Metadata(child.ID)
				return err
			}); p.ErrorCode(err) != "meta_type_not_found" {
				t.Fatalf("replacement could read erased schema: %v", err)
			}
		})
	}
}
