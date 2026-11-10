package sqlengine

import (
	"testing"

	"github.com/ekarpov/tessera/internal/recordstate"
	p "github.com/ekarpov/tessera/pkg/protocol"
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
		`DROP VIEW "tessera_read_objects"`,
		`DROP VIEW "tessera_all_field_values"`,
		`DROP TABLE "tessera_managed_installations"`,
		`DROP TABLE "tessera_managed_layouts"`,
		`DROP TABLE "tessera_field_values"`,
		`ALTER TABLE "tessera_objects" DROP COLUMN "wire_bytes"`,
		`ALTER TABLE "tessera_dataset" DROP COLUMN "managed_key"`,
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
