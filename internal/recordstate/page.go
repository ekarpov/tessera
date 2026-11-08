package recordstate

import (
	"errors"

	p "github.com/ekarpov/tessera/pkg/protocol"
)

// Continuation is where a page sequence resumes: the last identity it
// returned and, under a result sort, that row's values for the sort's
// fields. The zero value is the beginning.
type Continuation struct {
	After  string    `json:"after,omitempty"`
	Values []p.Value `json:"values,omitempty"`
}

type pageState struct {
	from     Continuation
	returned func(id string) bool
}

// errUnpaged stops a page that cannot be answered by continuing, so the
// caller can page the query some other way.
var errUnpaged = errors.New("query cannot be paged by continuation")

// Pager is implemented by an evaluation that can answer one page of a
// sequence by continuing from where the last one ended.
type Pager interface {
	PageAfter(typeID string, q p.Query, size int, from Continuation, returned func(id string) bool) (set p.RecordSet, next *Continuation, handled bool, err error)
}

// PageAfter answers up to size rows of a query that follow from, excluding
// any the sequence has returned, and where the next page would resume — nil
// when the source scan found nothing further. It answers handled false,
// having read nothing it keeps, for a query it cannot page this way: a
// per-type selection, whose rows depend on others of their type, or a sort
// the storage cannot order. Those are paged by the caller as a snapshot.
//
// This is keyset continuation, and it meets what the protocol asks of one:
// the scan follows the query's global order, tie-break included; the
// position advances only past rows returned; returned identities are
// excluded; and whether there is more is decided by the scan finding a row
// beyond the page, never by a page coming back short.
func (t *transaction) PageAfter(typeID string, q p.Query, size int, from Continuation, returned func(id string) bool) (p.RecordSet, *Continuation, bool, error) {
	if q.Selection != nil && q.Selection.Kind != "all" {
		return p.RecordSet{}, nil, false, nil
	}
	if len(q.ResultSort) > 0 && t.engine.sorted == nil {
		return p.RecordSet{}, nil, false, nil
	}
	t.page = &pageState{from: from, returned: returned}
	defer func() { t.page = nil }()
	// One row beyond the page is how the scan says there is more.
	set, err := t.Query(typeID, q, size+1)
	if errors.Is(err, errUnpaged) {
		return p.RecordSet{}, nil, false, nil
	}
	if err != nil {
		return p.RecordSet{}, nil, true, err
	}
	if len(set.Records) <= size {
		return set, nil, true, nil
	}
	set.Records = set.Records[:size]
	last := set.Records[size-1].ID.ID
	next := &Continuation{After: last}
	if len(q.ResultSort) > 0 {
		o := t.held(last)
		for _, term := range q.ResultSort {
			next.Values = append(next.Values, o.fields[term.FieldID])
		}
	}
	return set, next, true, nil
}

var _ Pager = (*transaction)(nil)
