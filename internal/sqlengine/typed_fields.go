package sqlengine

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/ekarpov/tessera/internal/recordstate"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

func fieldParts(value p.Value) (number any, data []byte) {
	switch payload := value.Payload().(type) {
	case int64:
		number = payload
	case float64:
		// Keep the exact binary64 spelling even on an engine that rounds its
		// JSON transport for REAL values. Float predicates use the evaluator.
		number = int64(math.Float64bits(payload))
	case bool:
		number = int64(0)
		if payload {
			number = int64(1)
		}
	case string:
		// Bytes preserve Unicode NUL and opaque payloads on both engines.
		data = []byte(payload)
	case p.Reference:
		// Protocol identifiers cannot contain NUL.
		data = []byte(strings.Join([]string{payload.DatasetID, payload.MetaID, payload.ObjectID}, "\x00"))
	}
	return number, data
}

type typedField struct {
	Field  string
	Kind   p.Type
	Number int64
	Data   string
}

func (f typedField) value() (p.Value, error) {
	var payload any
	switch f.Kind {
	case p.NullType:
		return p.Value{}, nil
	case p.IntegerType:
		payload = f.Number
	case p.FloatType:
		payload = math.Float64frombits(uint64(f.Number))
	case p.BooleanType:
		payload = f.Number != 0
	default:
		data, err := hex.DecodeString(f.Data)
		if err != nil {
			return p.Value{}, err
		}
		payload = string(data)
		if f.Kind == p.ReferenceType {
			parts := strings.Split(string(data), "\x00")
			if len(parts) != 3 {
				return p.Value{}, store.Refuse("internal_error")
			}
			payload = p.Reference{DatasetID: parts[0], MetaID: parts[1], ObjectID: parts[2]}
		}
	}
	return p.NewValue(f.Kind, payload)
}

func decodeStoredFields(raw []byte, row *recordstate.Row) error {
	var fields []typedField
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	row.Fields = make(map[string]p.Value, len(fields))
	for _, field := range fields {
		value, err := field.value()
		if err != nil {
			return err
		}
		row.Fields[field.Field] = value
	}
	return nil
}

func persistTypedFields(ctx context.Context, tx *sql.Tx, dataset, object string, previous, fields map[string]p.Value) error {
	var changed []string
	for id, value := range fields {
		old, exists := previous[id]
		oldNumber, oldData := fieldParts(old)
		number, data := fieldParts(value)
		if !exists || old.Type() != value.Type() || oldNumber != number || !bytes.Equal(oldData, data) {
			changed = append(changed, id)
		}
	}
	for id := range previous {
		if _, ok := fields[id]; !ok {
			if _, err := tx.ExecContext(ctx, `DELETE FROM "tessera_field_values" WHERE "dataset_id"=$1 AND "object_id"=$2 AND "field_id"=$3`, dataset, object, id); err != nil {
				return dbError(ctx, err)
			}
		}
	}
	slices.Sort(changed)
	// A bounded multi-row statement works with both engines' bind limits.
	for start := 0; start < len(changed); start += 128 {
		args := []any{}
		tuples := []string{}
		for _, id := range changed[start:min(start+128, len(changed))] {
			v := fields[id]
			number, data := fieldParts(v)
			params := make([]string, 6)
			for i := range params {
				params[i] = "$" + strconv.Itoa(len(args)+i+1)
			}
			tuples = append(tuples, "("+strings.Join(params, ",")+")")
			args = append(args, dataset, object, id, v.Type(), number, data)
		}
		query := `INSERT INTO "tessera_field_values" ("dataset_id","object_id","field_id","kind","number","data") VALUES ` +
			strings.Join(tuples, ",") + ` ON CONFLICT ("dataset_id","object_id","field_id")
			DO UPDATE SET "kind"=excluded."kind","number"=excluded."number","data"=excluded."data"`
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return dbError(ctx, err)
		}
	}
	return nil
}
