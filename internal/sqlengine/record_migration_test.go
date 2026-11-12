package sqlengine

import (
	"testing"

	"github.com/ekarpov/tessera/internal/recordstate"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

func legacyRecordPayloads(t *testing.T, s *Store) map[string]map[string][]byte {
	t.Helper()
	rows, err := s.db.QueryContext(t.Context(), `SELECT "dataset_id","object_id","fields" FROM "tessera_read_objects"`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	payloads := map[string]map[string][]byte{}
	for rows.Next() {
		var dataset, id string
		var raw []byte
		var record recordstate.Row
		if err = rows.Scan(&dataset, &id, &raw); err != nil {
			t.Fatal(err)
		}
		if err = decodeStoredFields(raw, &record); err != nil {
			t.Fatal(err)
		}
		body, err := p.MarshalWire(record.Fields)
		if err != nil {
			t.Fatal(err)
		}
		if payloads[dataset] == nil {
			payloads[dataset] = map[string][]byte{}
		}
		payloads[dataset][id] = body
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return payloads
}

func demoteRecordColumns(t *testing.T, s *Store, payloads map[string]map[string][]byte) {
	t.Helper()
	for _, statement := range []string{
		`ALTER TABLE "tessera_imports" DROP COLUMN "attachment_target"`, `DROP TABLE "tessera_contributions"`, `DROP TABLE "tessera_contribution_erasure"`,
		`DROP VIEW "tessera_read_objects"`,
		`DROP VIEW "tessera_all_field_values"`,
		`DROP TABLE "tessera_managed_installations"`,
		`DROP TABLE "tessera_managed_layouts"`,
		`DROP TABLE "tessera_field_values"`,
		`ALTER TABLE "tessera_objects" DROP COLUMN "wire_bytes"`,
		`ALTER TABLE "tessera_dataset" DROP COLUMN "managed_key"`,
		`ALTER TABLE "tessera_dataset" DROP COLUMN "schema_cache_key"`,
		`ALTER TABLE "tessera_objects" ADD COLUMN "fields" BLOB NOT NULL DEFAULT X'7B7D'`,
	} {
		if _, err := s.db.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(statement, err)
		}
	}
	for dataset, records := range payloads {
		for id, body := range records {
			if _, err := s.db.ExecContext(t.Context(), `UPDATE "tessera_objects" SET "fields"=$1 WHERE "dataset_id"=$2 AND "object_id"=$3`, body, dataset, id); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestCreatorStorageUpgrade(t *testing.T) {
	for _, version := range []string{"release", "creator-only"} {
		t.Run(version, func(t *testing.T) {
			s, path, dataset := fixture(t)
			var creator any
			if version == "creator-only" {
				if err := s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
					meta, err := tx.Metadata(typeID)
					if err != nil {
						return err
					}
					meta.Fields = append(meta.Fields, p.Field{ID: "created_by", MetaID: typeID, Type: p.TextType, Creator: true})
					_, err = tx.UpdateMetadata(meta, "")
					return err
				}); err != nil {
					t.Fatal(err)
				}
				// Model a database whose version key was rotated by an earlier restore.
				if _, err := s.db.ExecContext(t.Context(), `UPDATE "tessera_dataset" SET "creator_secret"=$1`, make([]byte, 32)); err != nil {
					t.Fatal(err)
				}
			}
			insert := func(engine *Store) p.Record {
				t.Helper()
				reply, err := engine.Mutate(t.Context(), owner, mutation(t, dataset, "", insertBody))
				if err != nil {
					t.Fatal(err)
				}
				return record(t, engine, dataset, identity(t, reply)).Records[0]
			}
			first := insert(s)
			if version == "creator-only" {
				creator = first.Values[1].Payload()
				demoteRecordColumns(t, s, legacyRecordPayloads(t, s))
				if _, err := s.db.ExecContext(t.Context(), `PRAGMA user_version=16`); err != nil {
					t.Fatal(err)
				}
			} else {
				for _, statement := range []string{`ALTER TABLE "tessera_imports" DROP COLUMN "attachment_target"`, `DROP TABLE "tessera_contributions"`, `DROP TABLE "tessera_contribution_erasure"`, `ALTER TABLE "tessera_dataset" DROP COLUMN "creator_secret"`, `PRAGMA user_version=18`} {
					if _, err := s.db.ExecContext(t.Context(), statement); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			upgraded, err := Open(t.Context(), path, p.DevelopmentLimits())
			if err != nil {
				t.Fatal(err)
			}
			defer upgraded.Close()
			carried := record(t, upgraded, dataset, first.ID.ID).Records[0]
			if carried.Version != first.Version || carried.Values[0].Payload() != first.Values[0].Payload() {
				t.Fatal("migration changed an existing record")
			}
			if version == "creator-only" && (carried.Values[1].Payload() != creator || insert(upgraded).Values[1].Payload() != creator) {
				t.Fatal("migration did not preserve creator attribution")
			}
			if version == "creator-only" {
				rows, _, err := upgraded.Contributions(t.Context(), dataset, owner.Principal, "", 10)
				if err != nil || len(rows) != 2 || rows[0].ObjectID != first.ID.ID {
					t.Fatal("migration did not backfill contributions", rows, err)
				}
			}
		})
	}
}
