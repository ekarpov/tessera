package sqlengine

import (
	"context"
	"database/sql"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

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
// Only types whose stored values SQL orders exactly as the protocol does are
// taken, and the rest are left to the evaluator:
//
//   - integer and float: SQLite compares integers and reals numerically and
//     exactly; PostgreSQL reads the stored text as numeric, which is exact, and
//     a float is stored in its shortest round-trip spelling, which orders as
//     the doubles do.
//   - boolean: false before true, as 0 before 1.
//   - text and date_time: by code point, which is byte order over UTF-8.
//     SQLite's default collation is that; PostgreSQL is asked for COLLATE "C".
//     A timestamp is one fixed-width spelling, so its text order is its time
//     order.
//
// fixed is a decimal carried as a string, which neither engine orders exactly
// without parsing; a reference orders by three strings. Both are left out.
//
// Nulls are placed explicitly, first ascending and last descending, because
// PostgreSQL's default is the opposite. The final tie-break is the object
// identifier by byte order, as in scanObjects.
func (s *Store) sortedObjects(ctx context.Context, tx *sql.Tx, dataset string, cut uint64, types []string, keys []recordstate.SortKey, from *recordstate.SortFrom, admit func(int) error, visit func(string, recordstate.Row) (bool, error)) (bool, error) {
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
	for i, key := range keys {
		args = append(args, key.FieldID)
		expression, ok := s.sortExpression(key.Type, "$"+strconv.Itoa(len(args)))
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
	if s.postgres {
		identity = `"object_id" COLLATE "C"`
	}
	order = append(order, identity+" ASC")
	selected := scope
	if from != nil {
		after, ok := s.sortsAfter(keys, expressions, identity, from, &args)
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
	query := s.numbered(`SELECT "object_id","type_id","fields" FROM "tessera_objects" WHERE ` + selected +
		` ORDER BY ` + strings.Join(order, ",") + ` LIMIT ` + limit + ` OFFSET ` + offset)
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
			if err = rows.Scan(&id, &row.TypeID, &raw); err != nil {
				rows.Close()
				return true, dbError(ctx, err)
			}
			read++
			if json.Unmarshal(raw, &row.Fields) != nil {
				rows.Close()
				return true, store.Refuse("internal_error")
			}
			row.Sequence, _ = strconv.ParseUint(id, 10, 64)
			row.Bytes = len(raw)
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
func (s *Store) sortsAfter(keys []recordstate.SortKey, expressions []string, identity string, from *recordstate.SortFrom, args *[]any) (string, bool) {
	if len(from.Values) != len(keys) {
		return "", false
	}
	bind := func(key recordstate.SortKey, v p.Value) (string, bool) {
		param, ok := s.sortParameter(key.Type, v)
		if !ok {
			return "", false
		}
		*args = append(*args, param)
		placeholder := "$" + strconv.Itoa(len(*args))
		if s.postgres {
			switch key.Type {
			case p.IntegerType, p.FloatType:
				return placeholder + "::numeric", true
			case p.BooleanType:
				return placeholder + "::boolean", true
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

// sortParameter is a sort value as the statement compares it: PostgreSQL
// casts the text it is given as the field's expression does, and SQLite
// compares the typed values json_extract yields.
func (s *Store) sortParameter(kind p.Type, v p.Value) (any, bool) {
	if v.Type() != kind {
		return nil, false
	}
	switch payload := v.Payload().(type) {
	case int64:
		if s.postgres {
			return strconv.FormatInt(payload, 10), true
		}
		return payload, true
	case float64:
		if s.postgres {
			return strconv.FormatFloat(payload, 'g', -1, 64), true
		}
		return payload, true
	case bool:
		if s.postgres {
			return strconv.FormatBool(payload), true
		}
		if payload {
			return 1, true
		}
		return 0, true
	case string:
		return payload, true
	}
	return nil, false
}

// sortExpression is the SQL value one stored field sorts by, with the field
// identifier bound as a parameter rather than written into the statement.
func (s *Store) sortExpression(kind p.Type, field string) (string, bool) {
	if s.postgres {
		value := `(convert_from("fields",'UTF8')::jsonb -> ` + field + `::text ->> 'value')`
		switch kind {
		case p.IntegerType, p.FloatType:
			return value + `::numeric`, true
		case p.BooleanType:
			return value + `::boolean`, true
		case p.TextType, p.DateTimeType:
			return value + ` COLLATE "C"`, true
		}
		return "", false
	}
	// The column is a BLOB, and this SQLite reads a BLOB given to its JSON
	// functions as its own binary JSON. The stored bytes are JSON text.
	value := `json_extract(CAST("fields" AS TEXT), '$."' || ` + field + ` || '".value')`
	switch kind {
	case p.IntegerType, p.FloatType, p.BooleanType, p.TextType, p.DateTimeType:
		return value, true
	}
	return "", false
}
