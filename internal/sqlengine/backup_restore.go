package sqlengine

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"strconv"
	"strings"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// restoreInsert batches inserts within the restore transaction, bounding both
// bind parameters and byte payloads. A single wide row still fits on its own.
type restoreInsert struct {
	prefix string
	args   []any
	tuples []string
	bytes  int
}

func (i *restoreInsert) add(ctx context.Context, tx *sql.Tx, row []any) error {
	size := 0
	for _, value := range row {
		switch value := value.(type) {
		case []byte:
			size += len(value)
		case string:
			size += len(value)
		case *string:
			if value != nil {
				size += len(*value)
			}
		}
	}
	if len(i.tuples) == 128 || len(i.args)+len(row) > 960 || i.bytes+size > 1<<20 {
		if err := i.flush(ctx, tx); err != nil {
			return err
		}
	}
	marks := make([]string, len(row))
	for column := range row {
		marks[column] = "$" + strconv.Itoa(len(i.args)+column+1)
	}
	i.args = append(i.args, row...)
	i.tuples = append(i.tuples, "("+strings.Join(marks, ",")+")")
	i.bytes += size
	return nil
}

func (i *restoreInsert) flush(ctx context.Context, tx *sql.Tx) error {
	if len(i.tuples) == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, i.prefix+strings.Join(i.tuples, ","), i.args...); err != nil {
		return dbError(ctx, err)
	}
	clear(i.args)
	i.args = i.args[:0]
	i.tuples = i.tuples[:0]
	i.bytes = 0
	return nil
}

func (s *Store) restorePackedFields(ctx context.Context, tx *sql.Tx, objects backupTable, payloads [][]byte, mappings map[string]managedMapping) error {
	if len(payloads) == 0 {
		return nil
	}
	datasetColumn := slices.Index(objects.Columns, "dataset_id")
	idColumn := slices.Index(objects.Columns, "object_id")
	typeColumn := slices.Index(objects.Columns, "type_id")
	shared := restoreInsert{prefix: `INSERT INTO "tessera_field_values" ("dataset_id","object_id","field_id","kind","number","data") VALUES `}
	managed := map[string]*restoreInsert{}
	for index, payload := range payloads {
		if payload == nil {
			return store.Refuse("invalid_field")
		}
		row := objects.Rows[index]
		dataset, id, typeID := row[datasetColumn].Text, row[idColumn].Text, row[typeColumn].Text
		if dataset == nil || id == nil || typeID == nil {
			return store.Refuse("invalid_field")
		}
		var fields map[string]p.Value
		if err := json.Unmarshal(payload, &fields); err != nil {
			return store.Refuse("invalid_field")
		}
		for _, layout := range mappings[*dataset][*typeID] {
			insert := managed[layout.id]
			if insert == nil {
				columns := []string{`"dataset_id"`, `"object_id"`}
				for column := range layout.Fields {
					columns = append(columns, managedColumn(column))
				}
				insert = &restoreInsert{prefix: "INSERT INTO " + layout.table() + " (" + strings.Join(columns, ",") + ") VALUES "}
				managed[layout.id] = insert
			}
			values := []any{*dataset, *id}
			for _, field := range layout.Fields {
				value := fields[field.ID]
				var arg any
				if value.Type() == field.Kind {
					number, data := fieldParts(value)
					var err error
					arg, err = s.managedFieldArgument(field.Kind, number, data)
					if err != nil {
						return err
					}
					delete(fields, field.ID)
				}
				values = append(values, arg)
			}
			if err := insert.add(ctx, tx, values); err != nil {
				return err
			}
		}
		for name, value := range fields {
			number, data := fieldParts(value)
			if err := shared.add(ctx, tx, []any{*dataset, *id, name, value.Type(), number, data}); err != nil {
				return err
			}
		}
	}
	for _, insert := range managed {
		if err := insert.flush(ctx, tx); err != nil {
			return err
		}
	}
	return shared.flush(ctx, tx)
}
