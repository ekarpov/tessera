package recordstate

import (
	"sort"
	"strings"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// scanSorted answers a sorted query by reading its rows already in order and
// stopping once it holds limit matches, where scanFor has to read the whole
// scope and sort it. It answers false, having read nothing, when there is no
// sorted source, when this is a write, or when the source cannot order by
// these terms; the caller then does what it always did.
//
// The filter, the view and the scan bound are applied here exactly as scanFor
// applies them. The source is trusted for one thing only — order — and that is
// checked as rows arrive, against this evaluator's own comparison.
func (t *transaction) scanSorted(typeID string, predicate *p.Predicate, sc reach, terms []p.SortTerm, limit int) ([]store.ObjectKey, bool, error) {
	// A write lays its own changes over storage, and a source ordering
	// storage alone would put a record this write changed where its old value
	// sorted. Reads only.
	if t.engine.sorted == nil || t.originals != nil || t.dirty || t.auth.View.HasCollectionReads() && !t.auth.View.Full() {
		return nil, false, nil
	}
	readable, err := t.readable(typeID, predicate, sc)
	if err != nil {
		return nil, false, err
	}
	meta, err := t.effective(typeID)
	if err != nil {
		return nil, false, err
	}
	declared := map[string]p.Type{}
	for _, f := range meta.Fields {
		declared[f.ID] = f.Type
	}
	keys := make([]SortKey, len(terms))
	for i, term := range terms {
		keys[i] = SortKey{FieldID: term.FieldID, Type: declared[term.FieldID], Ascending: term.Ascending}
	}
	types := make([]string, 0, len(readable))
	for id := range readable {
		types = append(types, id)
	}
	sort.Strings(types)
	if t.streamed == nil {
		t.streamed = map[string]object{}
	}
	result := []store.ObjectKey{}
	var previous object
	previousID, first, held := "", true, 0
	// Reading the whole scope is what a sorted query did before, and it was
	// refused when the scope held more than the scan bound. The same scope is
	// refused now, before a row is read, although far fewer are.
	admit := func(scope int) error {
		if scope > t.engine.limits.MaxScannedRows {
			return store.Refuse("query_result_limit_exceeded")
		}
		return nil
	}
	var from *SortFrom
	var position object
	if t.page != nil && t.page.from.After != "" {
		from = &SortFrom{Values: t.page.from.Values, ID: t.page.from.After}
		position = object{fields: map[string]p.Value{}}
		for i, term := range terms {
			if i < len(from.Values) {
				position.fields[term.FieldID] = from.Values[i]
			}
		}
	}
	handled, err := t.engine.sorted(types, keys, from, admit, func(id string, row Row) (bool, error) {
		if err := t.ctx.Err(); err != nil {
			return false, err
		}
		if !readable[row.TypeID] {
			return false, store.Refuse("internal_error")
		}
		o := object{sequence: row.Sequence, typeID: row.TypeID, fields: row.Fields}
		// Every row, matched or not, must follow the one before in this
		// evaluator's order. A source that disagreed with it anywhere —
		// a collation, a null placement, a numeric comparison — would return
		// rows that are each genuine matches and not the first ones.
		if !first && !inOrder(previous, previousID, o, id, terms) {
			return false, store.Refuse("internal_error")
		}
		first, previous, previousID = false, o, id
		// A page resumes strictly after where the last one ended, and never
		// returns a row the sequence already returned — one whose sort value
		// moved forward past the position would otherwise come round again.
		if from != nil && !inOrder(position, from.ID, o, id, terms) {
			return false, store.Refuse("internal_error")
		}
		if t.page != nil && t.page.returned != nil && t.page.returned(id) {
			return true, nil
		}
		if !t.auth.View.AllowsSequence(store.ObjectKey{TypeID: o.typeID, ObjectID: id}, o.sequence) {
			return true, nil
		}
		match, e := predicate.Match(id, o.fields)
		if e != nil || !match {
			return e == nil, e
		}
		held += row.Bytes
		if held > t.engine.limits.MaxMaterializedBytes {
			return false, store.RefuseWith("query_result_limit_exceeded", "materialization_limit",
				map[string]any{"maximum": t.engine.limits.MaxMaterializedBytes})
		}
		t.streamed[id] = o
		result = append(result, store.ObjectKey{TypeID: o.typeID, ObjectID: id})
		return len(result) < limit, nil
	})
	if err != nil || !handled {
		return nil, handled, err
	}
	return result, true, nil
}

// inOrder reports whether b may follow a in a result sort: each term by the
// protocol's comparison, descending terms reversed, then identity ascending.
func inOrder(a object, aID string, b object, bID string, terms []p.SortTerm) bool {
	for _, term := range terms {
		n, err := p.Compare(a.fields[term.FieldID], b.fields[term.FieldID])
		if err != nil {
			return false
		}
		if !term.Ascending {
			n = -n
		}
		if n != 0 {
			return n < 0
		}
	}
	return strings.Compare(aID, bID) < 0
}
