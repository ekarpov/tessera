package sqlengine

import (
	"context"
	"database/sql"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ekarpov/tessera/internal/recordstate"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// sortedObjects streams the records of the named types in a result sort's
// order, so a sorted query that wants ten rows reads about ten rather than the
// whole type. The evaluator still filters, applies the view and checks the
// order of every row against its own comparison; this supplies the order and
// nothing else.
//
// Only orderings that can use the exact protocol representation are pushed
// into SQL. Other types are sorted by the evaluator.
// Nulls are placed explicitly, first ascending and last descending, because
// PostgreSQL's default is the opposite. The final tie-break is the object
// identifier by byte order, as in scanObjects.
func (s *Store) sortedObjects(ctx context.Context, tx *sql.Tx, dataset string, cut uint64, managed managedPlan, types []string, keys []recordstate.SortKey, from *recordstate.SortFrom, admit func(int) error, visit func(string, recordstate.Row) (bool, error)) (bool, error) {
	if len(types) == 0 || len(keys) == 0 {
		return false, nil
	}
	args := []any{dataset}
	placeholders := make([]string, len(types))
	for i, id := range types {
		args = append(args, id)
		placeholders[i] = "$" + strconv.Itoa(len(args))
	}
	scope := `"dataset_id"=$1 AND "type_id" IN (` + strings.Join(placeholders, ",") + `)`
	if cut != 0 {
		// Canonical decimal identities compare numerically by length, then digits.
		// This avoids SQLite signed-integer overflow at a uint64 sequence point.
		bound := strconv.FormatUint(cut, 10)
		args = append(args, len(bound), bound)
		lengthParam, boundParam := "$"+strconv.Itoa(len(args)-1), "$"+strconv.Itoa(len(args))
		identity := `"object_id"`
		if s.postgres {
			identity += ` COLLATE "C"`
		}
		scope += ` AND (length("object_id")<` + lengthParam + ` OR (length("object_id")=` + lengthParam + ` AND ` + identity + `<` + boundParam + `))`
	}
	scopeArgs := len(args)
	order := make([]string, 0, len(keys)+1)
	expressions := make([]string, len(keys))
	source := managed.source + ` AS "o"`
	directTime := false
	for i, key := range keys {
		args = append(args, key.FieldID)
		parameter := "$" + strconv.Itoa(len(args))
		expression, ok := s.typedSortExpression(key.Type, parameter, managed.values)
		if layout, column, found := managed.mapping.field(types, key.FieldID, key.Type); found && ok {
			args = args[:len(args)-1]
			if len(keys) == 1 && key.Type == p.DateTimeType {
				source += " JOIN " + layout.table() + ` AS "m" USING ("dataset_id","object_id")`
				expression = `"m".` + column
				directTime = true
			} else {
				expression = s.managedSortExpression(layout, column, key.Type)
			}
		}
		if !ok {
			return false, nil
		}
		expressions[i] = expression
		if key.Ascending {
			order = append(order, expression+" ASC NULLS FIRST")
		} else {
			order = append(order, expression+" DESC NULLS LAST")
		}
	}
	identity := `"object_id"`
	if directTime {
		identity = `"m"."object_id"`
	}
	if s.postgres {
		identity += ` COLLATE "C"`
	}
	order = append(order, identity+" ASC")
	selected := scope
	if from != nil {
		after, ok := s.sortsAfter(keys, expressions, identity, from, &args, directTime)
		if !ok {
			return false, nil
		}
		selected = scope + " AND " + after
	}

	var count int
	if err := tx.QueryRowContext(ctx, s.numbered(`SELECT count(*) FROM "tessera_objects" WHERE `+scope), args[:scopeArgs]...).Scan(&count); err != nil {

		return true, dbError(ctx, err)
	}
	if err := admit(count); err != nil {
		return true, err
	}
	if s.sortHook != nil {
		s.sortHook()
	}
	limit := "$" + strconv.Itoa(len(args)+1)
	offset := "$" + strconv.Itoa(len(args)+2)
	query := s.numbered(`SELECT "object_id","type_id","fields","wire_bytes" FROM ` + source + ` WHERE ` +
		selected + ` ORDER BY ` + strings.Join(order, ",") + ` LIMIT ` + limit + ` OFFSET ` + offset)
	// Batches grow as scanObjects' do, and resume by offset rather than by
	// key: a sort key is not unique and may be null, and the order is total
	// only with the identity appended. Every batch runs in this transaction's
	// snapshot, so an offset names the same row each time.
	skipped, batch := 0, scanBatchFirst
	for {
		rows, err := tx.QueryContext(ctx, query, append(args, batch, skipped)...)
		if err != nil {
			return true, dbError(ctx, err)
		}
		read := 0
		for rows.Next() {
			var id string
			var row recordstate.Row
			var raw []byte
			if err = rows.Scan(&id, &row.TypeID, &raw, &row.Bytes); err != nil {
				rows.Close()
				return true, dbError(ctx, err)
			}
			read++
			if decodeStoredFields(raw, &row) != nil {
				rows.Close()
				return true, store.Refuse("internal_error")
			}
			row.Sequence, _ = strconv.ParseUint(id, 10, 64)
			more, e := visit(id, row)
			if e != nil || !more {
				rows.Close()
				return true, e
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return true, dbError(ctx, err)
		}
		if read < batch {
			return true, nil
		}
		skipped += read
		batch = min(batch*4, scanBatchLast)
	}
}

// placeholder is a statement parameter written as PostgreSQL numbers them.
var placeholder = regexp.MustCompile(`\$([0-9]+)`)

// numbered makes a statement's parameters numbered for SQLite as they are for
// PostgreSQL. SQLite reads "$3" as a parameter named "3" and numbers named
// parameters in the order they first appear, not by the digits — so a
// statement that uses "$4" before "$3", as the condition resuming a sorted
// page does, binds each argument to the wrong parameter, silently. "?3" is
// numbered by its digits. The JSON path literal's "$." has no digit after the
// dollar and is left alone.
func (s *Store) numbered(statement string) string {
	if s.postgres {
		return statement
	}
	return placeholder.ReplaceAllString(statement, "?$1")
}

// sortsAfter is the condition that a row follows from in the sort's order:
// for some term, every earlier term equal and this one after; or every term
// equal and the identity after. "After" is the order's own: ascending with
// nulls first, a null is followed by every value; descending with nulls last,
// nothing but a null follows a null, and a null follows every value.
func (s *Store) sortsAfter(keys []recordstate.SortKey, expressions []string, identity string, from *recordstate.SortFrom, args *[]any, directTime bool) (string, bool) {
	if len(from.Values) != len(keys) {
		return "", false
	}
	bind := func(key recordstate.SortKey, v p.Value) (string, bool) {
		param, ok := s.sortParameter(key.Type, v)
		if directTime {
			raw, valid := param.(string)
			if !valid {
				return "", false
			}
			if s.postgres {
				stamp, err := time.Parse(time.RFC3339Nano, raw)
				if err != nil {
					return "", false
				}
				*args = append(*args, stamp)
				return "$" + strconv.Itoa(len(*args)) + "::timestamptz", true
			}
			param = []byte(raw)
		}
		if !ok {
			return "", false
		}
		*args = append(*args, param)
		placeholder := "$" + strconv.Itoa(len(*args))
		if s.postgres {
			switch key.Type {
			case p.IntegerType, p.FloatType, p.BooleanType:
				return placeholder + "::bigint", true
			case p.TextType:
				return placeholder + "::bytea", true
			default:
				return placeholder + `::text COLLATE "C"`, true
			}
		}
		return placeholder, true
	}
	equal := make([]string, 0, len(keys))
	alternatives := make([]string, 0, len(keys)+1)
	for i, key := range keys {
		v := from.Values[i]
		var after, same string
		if v.Type() == p.NullType {
			same = expressions[i] + " IS NULL"
			if key.Ascending {
				after = expressions[i] + " IS NOT NULL"
			}
		} else {
			value, ok := bind(key, v)
			if !ok {
				return "", false
			}
			same = expressions[i] + " = " + value
			if key.Ascending {
				after = expressions[i] + " > " + value
			} else {
				after = "(" + expressions[i] + " < " + value + " OR " + expressions[i] + " IS NULL)"
			}
		}
		if after != "" {
			alternatives = append(alternatives, "("+strings.Join(append(append([]string{}, equal...), after), " AND ")+")")
		}
		equal = append(equal, same)
	}
	*args = append(*args, from.ID)
	last := identity + " > $" + strconv.Itoa(len(*args))
	alternatives = append(alternatives, "("+strings.Join(append(equal, last), " AND ")+")")
	return "(" + strings.Join(alternatives, " OR ") + ")", true
}

// Sort parameter encodings match the physical values, including binary64
// order and text byte order. Both signed zeros have the same rank.
func (s *Store) sortParameter(kind p.Type, v p.Value) (any, bool) {
	if v.Type() != kind {
		return nil, false
	}
	switch payload := v.Payload().(type) {
	case int64:
		return payload, true
	case float64:
		bits := int64(math.Float64bits(payload))
		if bits < 0 {
			bits = math.MinInt64 - bits
		}
		return bits, true
	case bool:
		if payload {
			return int64(1), true
		}
		return int64(0), true
	case string:
		if kind == p.TextType {
			return []byte(payload), true
		}
		return payload, true
	}
	return nil, false
}

func floatOrder(column string) string {
	return "(CASE WHEN " + column + "<0 THEN CAST('-9223372036854775808' AS BIGINT)-" + column + " ELSE " + column + " END)"
}

// Typed scalar orderings preserve the evaluator’s comparison. An absent field yields SQL NULL from the scalar subquery. The
// sort reads that field without assembling the rest of each candidate.
func (s *Store) typedSortExpression(kind p.Type, field, values string) (string, bool) {
	var value string
	switch kind {
	case p.IntegerType, p.BooleanType:
		value = `"v"."number"`
	case p.FloatType:
		value = floatOrder(`"v"."number"`)
	case p.TextType:
		value = `"v"."data"`
	case p.DateTimeType:
		value = `CAST("v"."data" AS TEXT)`
		if s.postgres {
			value = `convert_from("v"."data", 'UTF8') COLLATE "C"`
		}
	default:
		return "", false
	}
	expression := `(SELECT ` + value + ` FROM ` + values + ` AS "v"
        WHERE "v"."dataset_id"="o"."dataset_id" AND "v"."object_id"="o"."object_id"
        AND "v"."field_id"=` + field + `)`
	if s.postgres && kind == p.DateTimeType {
		expression += ` COLLATE "C"`
	}
	return expression, true
}

func (s *Store) managedSortExpression(layout installedLayout, column string, kind p.Type) string {
	value := "v." + column
	if kind == p.FloatType {
		value = floatOrder(value)
	}
	if kind == p.DateTimeType {
		if s.postgres {
			value = managedTimestampText(value) + ` COLLATE "C"`
		} else {
			value = "CAST(" + value + " AS TEXT)"
		}
	}
	return "(SELECT " + value + " FROM " + layout.table() + ` AS v WHERE v."dataset_id"="o"."dataset_id" AND v."object_id"="o"."object_id")`
}
