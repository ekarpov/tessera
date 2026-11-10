package sqlengine

import (
	"context"
	"database/sql"
	"encoding/json"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// Convert legacy payloads transactionally before dropping the column.
// Record identities, schema, grants and receipts stay where they were.
func (s *Store) migrateRecordFields(ctx context.Context, tx *sql.Tx) error {
	type legacy struct {
		dataset, id string
		raw         []byte
	}
	dataset, object := "", ""
	for {
		rows, err := tx.QueryContext(ctx, `SELECT "dataset_id","object_id","fields" FROM "tessera_objects"
            WHERE "wire_bytes" IS NULL AND ("dataset_id","object_id")>($1,$2)
            ORDER BY "dataset_id","object_id" LIMIT 256`, dataset, object)
		if err != nil {
			return dbError(ctx, err)
		}
		var batch []legacy
		for rows.Next() {
			var row legacy
			if err = rows.Scan(&row.dataset, &row.id, &row.raw); err != nil {
				rows.Close()
				return dbError(ctx, err)
			}
			batch = append(batch, row)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return dbError(ctx, err)
		}
		if len(batch) == 0 {
			break
		}
		for _, row := range batch {
			dataset, object = row.dataset, row.id
			if err = importLegacyFields(ctx, tx, row.dataset, row.id, row.raw); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE "tessera_objects" SET "wire_bytes"=$1 WHERE "dataset_id"=$2 AND "object_id"=$3`, len(row.raw), row.dataset, row.id); err != nil {
				return dbError(ctx, err)
			}
		}
	}
	if s.postgres {
		// Finish the historical objects-to-types constraint checks before
		// ALTER TABLE takes the payload column out of the row layout.
		if _, err := tx.ExecContext(ctx, `SET CONSTRAINTS ALL IMMEDIATE`); err != nil {
			return dbError(ctx, err)
		}
	}
	for _, statement := range []string{
		`ALTER TABLE "tessera_objects" DROP COLUMN "fields"`,
		`ALTER TABLE "tessera_dataset" DROP COLUMN "typed_fields"`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return dbError(ctx, err)
		}
	}
	return s.refreshManagedViews(ctx, tx)
}

func importLegacyFields(ctx context.Context, tx *sql.Tx, dataset, id string, raw []byte) error {
	var fields map[string]p.Value
	if err := json.Unmarshal(raw, &fields); err != nil {
		return dbError(ctx, err)
	}
	return persistTypedFields(ctx, tx, dataset, id, nil, fields)
}

func stripBackupColumn(table *backupTable, name string) {
	for index, column := range table.Columns {
		if column != name {
			continue
		}
		table.Columns = append(table.Columns[:index], table.Columns[index+1:]...)
		for i, row := range table.Rows {
			if index < len(row) {
				table.Rows[i] = append(row[:index], row[index+1:]...)
			}
		}
		return
	}
}

// The legacy format contained JSON records and optionally an encoded byte
// count for objects whose values were already stored in separate rows.
func normalizeObjectArchive(table *backupTable) ([][]byte, error) {
	legacy, wire := -1, -1
	for i, name := range table.Columns {
		switch name {
		case "fields":
			legacy = i
		case "wire_bytes":
			wire = i
		}
	}
	if legacy == -1 {
		return nil, nil
	}
	payloads := make([][]byte, len(table.Rows))
	for i, row := range table.Rows {
		if len(row) != len(table.Columns) {
			return nil, store.Refuse("invalid_field")
		}
		if wire >= 0 && row[wire].Integer != nil {
			if len(row[legacy].Bytes) != 0 || (row[legacy].Text != nil && *row[legacy].Text != "") {
				return nil, store.Refuse("invalid_field")
			}
			continue
		}
		raw := row[legacy].Bytes
		if row[legacy].Text != nil {
			raw = []byte(*row[legacy].Text)
		}
		if len(raw) == 0 {
			return nil, store.Refuse("invalid_field")
		}
		payloads[i] = raw
		size := int64(len(raw))
		value := backupValue{Kind: "integer", Integer: &size}
		if wire < 0 {
			table.Rows[i] = append(row, value)
		} else {
			table.Rows[i][wire] = value
		}
	}
	if wire < 0 {
		table.Columns = append(table.Columns, "wire_bytes")
	}
	stripBackupColumn(table, "fields")
	return payloads, nil
}
