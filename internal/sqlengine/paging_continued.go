package sqlengine

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/ekarpov/tessera/internal/recordstate"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// Continued page sequences.
//
// A sequence used to be a snapshot: starting one evaluated the whole query and
// stored every page, so the first page cost the whole result — 440 ms over
// 100,000 records — and a caller who read one page paid for all of them. The
// protocol does not promise a snapshot, and says exactly what keyset
// continuation owes instead; this is that.
//
// Each page is computed the first time its cursor is asked for, from the
// position the page before ended at, and stored; asking again replays what was
// stored. A page not yet asked for holds its position and no body. A sorted
// sequence also keeps the identities it has returned, since a row whose sort
// value moves forward past the position would otherwise be returned again.

// pagedQuery is what a sequence stores of its query, and is how it was always
// stored.
type pagedQuery struct {
	Type  string  `json:"type"`
	Query p.Query `json:"query"`
	Size  int     `json:"page_size"`
}

// pageEngine is an evaluator over one dataset's image, able to page, reading
// within tx.
func (s *Store) pageEngine(ctx context.Context, tx *sql.Tx, a store.Access, dataset string) (*recordstate.Store, held, error) {
	h, err := s.load(ctx, tx, a, dataset, false)
	if err != nil {
		return nil, h, err
	}
	engine, err := recordstate.Adopt(h.image, h.limits, a, h.auth, s.reads)
	if err != nil {
		return nil, h, err
	}
	engine.SetObjectSource(func(id string) (recordstate.Row, bool, error) { return fetchRow(ctx, tx, dataset, id) })
	engine.SetScanSource(func(types []string, visit func(string, recordstate.Row) (bool, error)) error {
		return s.scanObjects(ctx, tx, dataset, types, visit)
	})
	engine.SetScanFromSource(func(types []string, after string, visit func(string, recordstate.Row) (bool, error)) error {
		return s.scanObjectsFrom(ctx, tx, dataset, types, after, visit)
	})
	engine.SetSortedScanSource(func(types []string, keys []recordstate.SortKey, from *recordstate.SortFrom, admit func(int) error, visit func(string, recordstate.Row) (bool, error)) (bool, error) {
		return s.sortedObjects(ctx, tx, dataset, h.auth.View.SequenceCut(), types, keys, from, admit, visit)
	})
	return engine, h, nil
}

// computePage evaluates one page of a sequence.
func computePage(ctx context.Context, engine *recordstate.Store, a store.Access, dataset string, pq pagedQuery, from recordstate.Continuation, returned func(string) bool) (p.RecordSet, *recordstate.Continuation, bool, error) {
	var set p.RecordSet
	var next *recordstate.Continuation
	handled := false
	err := engine.Evaluate(ctx, a, dataset, func(core store.Tx) error {
		pager, ok := core.(recordstate.Pager)
		if !ok {
			return nil
		}
		var e error
		set, next, handled, e = pager.PageAfter(pq.Type, pq.Query, pq.Size, from, returned)
		return e
	})
	return set, next, handled, err
}

// pageUsage is what this authorization holds in page sets on the dataset.
// It used to be every page set on the server, compared with this dataset's
// bound, so one caller refused paging to everybody else (finding 35).
func pageUsage(ctx context.Context, tx *sql.Tx, a store.Access, dataset string) (int64, error) {
	var pages, queries int64
	if err := tx.QueryRowContext(ctx, `SELECT
		(SELECT coalesce(sum(length(p."body")),0) FROM "tessera_pages" p JOIN "tessera_sequences" q ON q."id"=p."sequence_id" WHERE q."dataset_id"=$1 AND q."principal"=$2 AND q."authorization"=$3),
		(SELECT coalesce(sum(length("query"))+sum(coalesce(length("meta"),0)),0) FROM "tessera_sequences" WHERE "dataset_id"=$1 AND "principal"=$2 AND "authorization"=$3)`,
		dataset, a.Principal, a.Authorization).Scan(&pages, &queries); err != nil {
		return 0, dbError(ctx, err)
	}
	return pages + queries, nil
}

// storePage writes one computed page body under its cursor, the cursor after
// it as pending where the scan found more, and the rows it returned where the
// sequence needs to remember them. It answers the body as stored.
func storePage(ctx context.Context, tx *sql.Tx, sequence, cursor string, set p.RecordSet, next *recordstate.Continuation, sorted bool, fresh bool) ([]byte, error) {
	var nextCursor *string
	var position []byte
	if next != nil {
		token, err := cursorToken()
		if err != nil {
			return nil, err
		}
		nextCursor = &token
		if position, err = json.Marshal(next); err != nil {
			return nil, err
		}
	}
	// The schema is kept once, on the sequence, and put back when a page is
	// served.
	body, err := p.MarshalWire(map[string]any{"records": set.Records, "watermark": set.Watermark, "next_cursor": nextCursor})
	if err != nil {
		return nil, err
	}
	if fresh {
		_, err = tx.ExecContext(ctx, `INSERT INTO "tessera_pages" ("cursor","sequence_id","body") VALUES ($1,$2,$3)`, cursor, sequence, body)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE "tessera_pages" SET "body"=$1,"position"=NULL WHERE "cursor"=$2`, body, cursor)
	}
	if err != nil {
		return nil, dbError(ctx, err)
	}
	if nextCursor != nil {
		if _, err = tx.ExecContext(ctx, `INSERT INTO "tessera_pages" ("cursor","sequence_id","body","position") VALUES ($1,$2,$3,$4)`, *nextCursor, sequence, []byte{}, position); err != nil {
			return nil, dbError(ctx, err)
		}
	}
	if sorted && len(set.Records) > 0 {
		values, arguments := make([]string, 0, len(set.Records)), make([]any, 0, 2*len(set.Records))
		for _, record := range set.Records {
			n := len(arguments)
			values = append(values, "($"+strconv.Itoa(n+1)+",$"+strconv.Itoa(n+2)+")")
			arguments = append(arguments, sequence, record.ID.ID)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO "tessera_page_rows" ("sequence_id","object_id") VALUES `+strings.Join(values, ","), arguments...); err != nil {
			return nil, dbError(ctx, err)
		}
	}
	return body, nil
}

// beginContinued starts a continued sequence, answering handled false when
// the query cannot be continued and must be a snapshot.
func (s *Store) beginContinued(ctx context.Context, a store.Access, dataset, typeID string, q p.Query, size int) (store.PageStart, bool, error) {
	if err := s.enter(); err != nil {
		return store.PageStart{}, true, err
	}
	defer s.life.RUnlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.PageStart{}, true, dbError(ctx, err)
	}
	defer tx.Rollback()
	if err = s.serializeWrites(ctx, tx, dataset); err != nil {
		return store.PageStart{}, true, err
	}
	engine, h, err := s.pageEngine(ctx, tx, a, dataset)
	if err != nil {
		return store.PageStart{}, true, err
	}
	if size < 1 || size > h.limits.MaxPageSize {
		return store.PageStart{}, true, store.Refuse("query_result_limit_exceeded")
	}
	pq := pagedQuery{Type: typeID, Query: q, Size: size}
	set, next, handled, err := computePage(ctx, engine, a, dataset, pq, recordstate.Continuation{}, nil)
	if err != nil || !handled {
		return store.PageStart{}, handled, err
	}
	meta, err := p.MarshalWire(set.Meta)
	if err != nil {
		return store.PageStart{}, true, err
	}
	rawQuery, err := p.MarshalWire(pq)
	if err != nil {
		return store.PageStart{}, true, err
	}
	var generation string
	if err = tx.QueryRowContext(ctx, `SELECT "generation" FROM "tessera_access" WHERE "dataset_id"=$1 AND "principal"=$2 AND "authorization"=$3`, dataset, a.Principal, a.Authorization).Scan(&generation); err != nil {
		return store.PageStart{}, true, dbError(ctx, err)
	}
	now := time.Now()
	if _, err = tx.ExecContext(ctx, `DELETE FROM "tessera_sequences" WHERE "id" IN (SELECT "id" FROM "tessera_sequences" WHERE "expires_at"<$1 LIMIT $2)`, now.UnixMilli(), expirySweep); err != nil {
		return store.PageStart{}, true, dbError(ctx, err)
	}
	first, err := cursorToken()
	if err != nil {
		return store.PageStart{}, true, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO "tessera_sequences" ("id","dataset_id","principal","authorization","generation","expires_at","query","meta") VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, first, dataset, a.Principal, a.Authorization, generation, now.Add(pageLifetime).UnixMilli(), rawQuery, meta); err != nil {
		return store.PageStart{}, true, dbError(ctx, err)
	}
	if _, err = storePage(ctx, tx, first, first, set, next, len(q.ResultSort) > 0, true); err != nil {
		return store.PageStart{}, true, err
	}
	if used, e := pageUsage(ctx, tx, a, dataset); e != nil {
		return store.PageStart{}, true, e
	} else if used > int64(h.limits.MaxMaterializedBytes) {
		return store.PageStart{}, true, store.Refuse("query_result_limit_exceeded")
	}
	if err = tx.Commit(); err != nil {
		return store.PageStart{}, true, dbError(ctx, err)
	}
	return store.PageStart{Cursor: first, Watermark: set.Watermark}, true, nil
}

// continuePage computes a page that was pending when its cursor was asked
// for, stores it, and answers it as page answers a stored one. A page another
// request computed in the meantime is answered as stored.
func (s *Store) continuePage(ctx context.Context, a store.Access, dataset, cursor string) (json.RawMessage, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, dbError(ctx, err)
	}
	defer tx.Rollback()
	if err = s.serializeWrites(ctx, tx, dataset); err != nil {
		return nil, err
	}
	sequence, body, err := s.page(ctx, tx, a, dataset, cursor)
	if err != nil {
		return nil, err
	}
	var position, rawQuery, meta []byte
	if err = tx.QueryRowContext(ctx, `SELECT p."position",s."query",s."meta" FROM "tessera_pages" p JOIN "tessera_sequences" s ON s."id"=p."sequence_id" WHERE p."cursor"=$1`, cursor).Scan(&position, &rawQuery, &meta); err != nil {
		return nil, dbError(ctx, err)
	}
	if len(position) == 0 {
		return body, nil
	}
	var pq pagedQuery
	var from recordstate.Continuation
	if json.Unmarshal(rawQuery, &pq) != nil || json.Unmarshal(position, &from) != nil {
		return nil, store.Refuse("invalid_cursor")
	}
	sorted := len(pq.Query.ResultSort) > 0
	var returned func(string) bool
	if sorted {
		seen := map[string]bool{}
		rows, e := tx.QueryContext(ctx, `SELECT "object_id" FROM "tessera_page_rows" WHERE "sequence_id"=$1`, sequence)
		if e != nil {
			return nil, dbError(ctx, e)
		}
		for rows.Next() {
			var id string
			if e = rows.Scan(&id); e != nil {
				rows.Close()
				return nil, dbError(ctx, e)
			}
			seen[id] = true
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return nil, dbError(ctx, e)
		}
		returned = func(id string) bool { return seen[id] }
	}
	engine, h, err := s.pageEngine(ctx, tx, a, dataset)
	if err != nil {
		return nil, err
	}
	set, next, handled, err := computePage(ctx, engine, a, dataset, pq, from, returned)
	if err != nil {
		return nil, err
	}
	if !handled {
		return nil, store.Refuse("internal_error")
	}
	// The sequence's schema was fixed when it began, and its pages are
	// served with it. A schema changed since cannot describe this page, so
	// the sequence ends and the client starts the query again.
	now, err := p.MarshalWire(set.Meta)
	if err != nil {
		return nil, err
	}
	if string(now) != string(meta) {
		return nil, store.Refuse("cursor_expired")
	}
	stored, err := storePage(ctx, tx, sequence, cursor, set, next, sorted, false)
	if err != nil {
		return nil, err
	}
	if used, e := pageUsage(ctx, tx, a, dataset); e != nil {
		return nil, e
	} else if used > int64(h.limits.MaxMaterializedBytes) {
		return nil, store.Refuse("query_result_limit_exceeded")
	}
	if err = tx.Commit(); err != nil {
		return nil, dbError(ctx, err)
	}
	return withMeta(stored, meta), nil
}
