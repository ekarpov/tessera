package recordstate

import (
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
	"sort"
	"strings"
)

func (t *transaction) Query(typeID string, q p.Query, limit int) (p.RecordSet, error) {
	if err := t.check(t.auth.Grants.Read); err != nil {
		return p.RecordSet{}, err
	}
	limits := t.engine.limits
	if limit < 1 || limit > limits.MaxOverallLimit {
		return p.RecordSet{}, store.Refuse("query_result_limit_exceeded")
	}
	if !t.auth.View.AllowsType(typeID) {
		return p.RecordSet{}, store.Refuse("permission_denied")
	}
	meta, err := t.effective(typeID)
	if err != nil {
		return p.RecordSet{}, err
	}
	scope := p.TypeScope{Kind: "exact"}
	if q.TypeScope != nil {
		scope = *q.TypeScope
	}
	if scope.Kind != "exact" && scope.Kind != "descendants" {
		return p.RecordSet{}, store.Refuse("invalid_filter")
	}
	allowed := map[string]bool{}
	if scope.AllowedConcreteTypeIDs != nil {
		if scope.Kind != "descendants" || len(scope.AllowedConcreteTypeIDs) == 0 || len(scope.AllowedConcreteTypeIDs) > limits.MaxAllowedConcreteTypes {
			return p.RecordSet{}, store.Refuse("invalid_filter")
		}
		for _, id := range scope.AllowedConcreteTypeIDs {
			if allowed[id] || p.ValidateIdentifier(p.MetaIdentifier, id) != nil {
				return p.RecordSet{}, store.Refuse("invalid_filter")
			}
			if _, ok := t.state.types[id]; !ok || !t.descendant(id, typeID) {
				return p.RecordSet{}, store.Refuse("invalid_filter")
			}
			allowed[id] = true
		}
	}
	selection := p.Selection{Kind: "all"}
	if q.Selection != nil {
		selection = *q.Selection
	}
	if selection.Kind != "all" && selection.Kind != "first_per_concrete_type" {
		return p.RecordSet{}, store.Refuse("invalid_filter")
	}
	if selection.Kind == "first_per_concrete_type" && (selection.Count < 1 || selection.Count > limits.MaxSelectionPerConcreteType || len(selection.OrderedBy) == 0) {
		return p.RecordSet{}, store.Refuse("invalid_filter")
	}
	if selection.Kind == "all" && (selection.Count != 0 || len(selection.OrderedBy) > 0) {
		return p.RecordSet{}, store.Refuse("invalid_filter")
	}
	if q.MaximumTextLength != nil && (*q.MaximumTextLength < 1 || *q.MaximumTextLength > 1024 || q.Fields == nil || selection.Kind != "all") {
		return p.RecordSet{}, store.Refuse("invalid_field")
	}
	fieldTypes := map[string]p.Type{}
	for _, f := range meta.Fields {
		fieldTypes[f.ID] = f.Type
	}
	for _, terms := range [][]p.SortTerm{q.ResultSort, selection.OrderedBy} {
		if len(terms) > limits.MaxSortTerms {
			return p.RecordSet{}, store.Refuse("query_result_limit_exceeded")
		}
		for _, term := range terms {
			kind, ok := fieldTypes[term.FieldID]
			if !ok {
				return p.RecordSet{}, store.Refuse("invalid_field")
			}
			if kind == p.BlobType || kind == p.JSONType || kind == p.MediaType {
				return p.RecordSet{}, store.Refuse("invalid_filter")
			}
		}
	}
	// Validate projection even when the query matches no records.
	projection, err := t.record(typeID, "", object{typeID: typeID, fields: map[string]p.Value{}}, q.Fields, false)
	if err != nil {
		return p.RecordSet{}, err
	}
	predicate, err := p.ParsePredicate(q.Filter, limits)
	if err != nil {
		return p.RecordSet{}, err
	}
	// Tell the scan what the answer needs, so it reads no further.
	//
	// The types: an exact query reads one type rather than every descendant
	// and then discarding them, and a query naming allowed types reads those.
	// That also charges max_scanned_rows only for rows the query could return,
	// which is more permissive than before and more accurate — a declared scan
	// bound is a bound on work done, and discarded descendants were work
	// nobody asked for.
	//
	// When it may stop: only when the final order is identity order, which is
	// the order the scan yields. A result sort or a per-type selection needs
	// every match before it can know which ones come first, so those read the
	// whole scope.
	sc := reach{}
	if scope.Kind == "exact" {
		sc.types = map[string]bool{typeID: true}
	} else if len(allowed) > 0 {
		sc.types = allowed
	}
	if len(q.ResultSort) == 0 && selection.Kind == "all" {
		sc.stopAfter = limit
	}
	// A result sort over all rows can read its rows already in order and stop
	// at the limit, where a storage offers that. A per-type selection cannot:
	// its first rows are per type, which one ordered stream does not give.
	var keys []store.ObjectKey
	presorted := false
	if len(q.ResultSort) > 0 && selection.Kind == "all" {
		if keys, presorted, err = t.scanSorted(typeID, predicate, sc, q.ResultSort, limit); err != nil {
			return p.RecordSet{}, err
		}
	}
	if !presorted {
		// A page continuing a sorted sequence needs its rows in order from
		// the storage; without that it is paged as a snapshot instead.
		if t.page != nil && len(q.ResultSort) > 0 {
			return p.RecordSet{}, errUnpaged
		}
		if keys, err = t.scanFor(typeID, predicate, true, sc); err != nil {
			return p.RecordSet{}, err
		}
	}
	chosen := make([]store.ObjectKey, 0, len(keys))
	for _, key := range keys {
		if scope.Kind == "exact" && key.TypeID != typeID {
			continue
		}
		if len(allowed) > 0 && !allowed[key.TypeID] {
			continue
		}
		chosen = append(chosen, key)
	}
	order := func(terms []p.SortTerm) {
		sort.Slice(chosen, func(i, j int) bool {
			a, b := t.held(chosen[i].ObjectID), t.held(chosen[j].ObjectID)
			for _, term := range terms {
				n, _ := p.Compare(a.fields[term.FieldID], b.fields[term.FieldID])
				if n != 0 {
					if term.Ascending {
						return n < 0
					}
					return n > 0
				}
			}
			return strings.Compare(chosen[i].ObjectID, chosen[j].ObjectID) < 0
		})
	}
	if selection.Kind == "first_per_concrete_type" {
		order(selection.OrderedBy)
		counts := map[string]int{}
		selected := make([]store.ObjectKey, 0, len(chosen))
		for _, key := range chosen {
			if counts[key.TypeID] < selection.Count {
				selected = append(selected, key)
				counts[key.TypeID]++
			}
		}
		chosen = selected
	}
	// Matching answers in identity order and the filtering above preserves
	// it, so sorting again by nothing but the identity tie-break is a full
	// sort to arrive at the order already held. The selection branch above
	// reorders, so after it the sort is doing something.
	if !presorted && (len(q.ResultSort) > 0 || selection.Kind == "first_per_concrete_type") {
		order(q.ResultSort)
	}
	if len(chosen) > limit {
		chosen = chosen[:limit]
	}
	result := p.RecordSet{Meta: projection.Meta, Records: make([]p.Record, 0, len(chosen)), Watermark: t.Watermark()}
	for _, key := range chosen {
		if err = t.ctx.Err(); err != nil {
			return p.RecordSet{}, err
		}
		// Built from the projection settled once above rather than through
		// record, which resolves the effective type and re-derives the
		// projected fields from scratch for every row it is handed. The
		// metadata is the same for every row in the answer — record was
		// asked for the queried type each time, not the row's own.
		o := t.held(key.ObjectID)
		values := make([]p.Value, len(projection.Meta.Fields))
		for i, f := range projection.Meta.Fields {
			values[i] = o.fields[f.ID]
		}
		version := ""
		if q.IncludeVersions {
			version = t.version(o)
		}
		row := p.Record{ID: p.ObjectID{ID: key.ObjectID, DatasetID: t.engine.id}, MetaID: o.typeID, Values: values, Version: version}
		if q.MaximumTextLength != nil {
			for i, v := range row.Values {
				if v.Type() == p.TextType {
					runes := []rune(v.Payload().(string))
					if len(runes) > *q.MaximumTextLength {
						row.Values[i], _ = p.NewValue(p.TextType, string(runes[:*q.MaximumTextLength]))
					}
				}
			}
		}
		result.Records = append(result.Records, row)
	}
	return result, nil
}
