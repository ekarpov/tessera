package sqlengine

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/ekarpov/tessera/internal/recordstate"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

const pageLifetime = 15 * time.Minute

func cursorToken() (string, error) {
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", store.Refuse("internal_error")
	}
	return base64.RawURLEncoding.EncodeToString(token[:]), nil
}

// expirySweep bounds how many lapsed page sequences one call clears, and
// pageBatch how many pages one insert statement carries. Both are limits on
// work done inside a write transaction on behalf of a caller who asked for
// something else.
const (
	expirySweep = 256
	pageBatch   = 64
)

// BeginPage starts a page sequence. Where the query can be continued — every
// row, in identity order or in a sort the storage orders — it computes the
// first page alone and leaves the next pending, so starting costs one page
// rather than the whole result; see NextPage. Otherwise it takes the snapshot
// it always did.
func (s *Store) BeginPage(ctx context.Context, a store.Access, dataset, typeID string, q p.Query, size int) (store.PageStart, error) {
	if s.snapshotPages {
		return s.beginSnapshot(ctx, a, dataset, typeID, q, size)
	}
	start, handled, err := s.beginContinued(ctx, a, dataset, typeID, q, size)
	if err != nil || handled {
		return start, err
	}
	return s.beginSnapshot(ctx, a, dataset, typeID, q, size)
}

// beginSnapshot evaluates the whole query and stores every page.
func (s *Store) beginSnapshot(ctx context.Context, a store.Access, dataset, typeID string, q p.Query, size int) (store.PageStart, error) {
	if err := s.enter(); err != nil {
		return store.PageStart{}, err
	}
	defer s.life.RUnlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.PageStart{}, dbError(ctx, err)
	}
	defer tx.Rollback()
	// A page set is a snapshot of the image, so it is taken while no write to
	// this dataset is in flight.
	if err = s.serializeWrites(ctx, tx, dataset); err != nil {
		return store.PageStart{}, err
	}
	// Built without the records: the query streams the types it reads, in
	// identity order, rather than the whole dataset being loaded for it.
	h, err := s.load(ctx, tx, a, dataset, false)
	if err != nil {
		return store.PageStart{}, err
	}
	// The page size is bounded by the dataset being paged, which is why it is
	// checked here rather than before the dataset is known.
	if size < 1 || size > h.limits.MaxPageSize {
		return store.PageStart{}, store.Refuse("query_result_limit_exceeded")
	}
	before := h.image
	// Snapshot evaluation has the declared scan bound, not the ordinary one-shot
	// response limit. It holds no database transaction across client think-time.
	limits := h.limits
	limits.MaxOverallLimit = limits.MaxScannedRows
	engine, err := recordstate.Adopt(before, limits, a, h.auth, s.reads)
	if err != nil {
		return store.PageStart{}, err
	}
	engine.SetObjectSource(func(id string) (recordstate.Row, bool, error) { return fetchRow(ctx, tx, dataset, id) })
	engine.SetScanSource(func(types []string, visit func(string, recordstate.Row) (bool, error)) error {
		return s.scanObjects(ctx, tx, dataset, types, visit)
	})
	var result p.RecordSet
	if err = engine.Evaluate(ctx, a, dataset, func(core store.Tx) error {
		var e error
		result, e = core.Query(typeID, q, limits.MaxScannedRows)
		return e
	}); err != nil {
		return store.PageStart{}, err
	}
	meta, err := p.MarshalWire(result.Meta)
	if err != nil {
		return store.PageStart{}, err
	}
	rawQuery, err := p.MarshalWire(struct {
		Type  string  `json:"type"`
		Query p.Query `json:"query"`
		Size  int     `json:"page_size"`
	}{typeID, q, size})
	if err != nil {
		return store.PageStart{}, err
	}
	var generation string
	if err = tx.QueryRowContext(ctx, `SELECT "generation" FROM "tessera_access" WHERE "dataset_id"=$1 AND "principal"=$2 AND "authorization"=$3`, dataset, a.Principal, a.Authorization).Scan(&generation); err != nil {
		return store.PageStart{}, dbError(ctx, err)
	}
	now := time.Now()
	// Expiry is swept a bounded number of rows at a time. Beginning one page
	// set is not the moment to pay for every sequence that has ever lapsed,
	// and what this pass leaves behind the next one takes: the rows are dead
	// either way and nothing reads them.
	if _, err = tx.ExecContext(ctx, `DELETE FROM "tessera_sequences" WHERE "id" IN (SELECT "id" FROM "tessera_sequences" WHERE "expires_at"<$1 LIMIT $2)`, now.UnixMilli(), expirySweep); err != nil {
		return store.PageStart{}, dbError(ctx, err)
	}
	// What this authorization already holds in page sets on this dataset.
	// It used to be every page set on the server, compared with this
	// dataset's bound, so one caller holding large page sets refused paging
	// to everybody else until theirs expired (finding 35).
	var used, queries int64
	if err = tx.QueryRowContext(ctx, `SELECT
		(SELECT coalesce(sum(length(p."body")),0) FROM "tessera_pages" p JOIN "tessera_sequences" q ON q."id"=p."sequence_id" WHERE q."dataset_id"=$1 AND q."principal"=$2 AND q."authorization"=$3),
		(SELECT coalesce(sum(length("query"))+sum(coalesce(length("meta"),0)),0) FROM "tessera_sequences" WHERE "dataset_id"=$1 AND "principal"=$2 AND "authorization"=$3)`,
		dataset, a.Principal, a.Authorization).Scan(&used, &queries); err != nil {
		return store.PageStart{}, dbError(ctx, err)
	}
	used += queries + int64(len(rawQuery)) + int64(len(meta))
	count := (len(result.Records) + size - 1) / size
	if count == 0 {
		count = 1
	}
	tokens := make([]string, count)
	for i := range tokens {
		tokens[i], err = cursorToken()
		if err != nil {
			return store.PageStart{}, err
		}
	}
	expires := now.Add(pageLifetime)
	if _, err = tx.ExecContext(ctx, `INSERT INTO "tessera_sequences" ("id","dataset_id","principal","authorization","generation","expires_at","query","meta") VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, tokens[0], dataset, a.Principal, a.Authorization, generation, expires.UnixMilli(), rawQuery, meta); err != nil {
		return store.PageStart{}, dbError(ctx, err)
	}
	// Written in batches rather than a statement per page. A page size of one
	// over a large result made this one round trip per matching record, all
	// of them inside the write transaction holding the dataset's lock.
	values, arguments := make([]string, 0, pageBatch), make([]any, 0, 3*pageBatch)
	flush := func() error {
		if len(values) == 0 {
			return nil
		}
		if _, e := tx.ExecContext(ctx, `INSERT INTO "tessera_pages" ("cursor","sequence_id","body") VALUES `+strings.Join(values, ","), arguments...); e != nil {
			return dbError(ctx, e)
		}
		values, arguments = values[:0], arguments[:0]
		return nil
	}
	for i, token := range tokens {
		end := (i + 1) * size
		if end > len(result.Records) {
			end = len(result.Records)
		}
		page := result
		page.Records = result.Records[i*size : end]
		var next *string
		if i+1 < count {
			next = &tokens[i+1]
		}
		// The schema is kept once, on the sequence, and put back when a page
		// is served.
		body, e := p.MarshalWire(map[string]any{"records": page.Records, "watermark": page.Watermark, "next_cursor": next})
		if e != nil {
			return store.PageStart{}, e
		}
		used += int64(len(body))
		if used > int64(h.limits.MaxMaterializedBytes) {
			return store.PageStart{}, store.Refuse("query_result_limit_exceeded")
		}
		n := len(arguments)
		values = append(values, "($"+strconv.Itoa(n+1)+",$"+strconv.Itoa(n+2)+",$"+strconv.Itoa(n+3)+")")
		arguments = append(arguments, token, tokens[0], body)
		if len(values) == pageBatch {
			if err = flush(); err != nil {
				return store.PageStart{}, err
			}
		}
	}
	if err = flush(); err != nil {
		return store.PageStart{}, err
	}
	if err = tx.Commit(); err != nil {
		return store.PageStart{}, dbError(ctx, err)
	}
	return store.PageStart{Cursor: tokens[0], Watermark: result.Watermark}, nil
}

func (s *Store) page(ctx context.Context, tx *sql.Tx, a store.Access, dataset, cursor string) (string, json.RawMessage, error) {
	if !a.Valid() {
		return "", nil, store.Refuse("authentication_failed")
	}
	if len(cursor) != 43 {
		return "", nil, store.Refuse("invalid_cursor")
	}
	var owner store.Access
	var seq, gen, ds string
	var expires int64
	var body, meta []byte
	err := tx.QueryRowContext(ctx, `SELECT s."id",s."dataset_id",s."principal",s."authorization",s."generation",s."expires_at",p."body",s."meta" FROM "tessera_pages" p JOIN "tessera_sequences" s ON s."id"=p."sequence_id" WHERE p."cursor"=$1`, cursor).Scan(&seq, &ds, &owner.Principal, &owner.Authorization, &gen, &expires, &body, &meta)
	if err == sql.ErrNoRows {
		return "", nil, store.Refuse("invalid_cursor")
	}
	if err != nil {
		return "", nil, dbError(ctx, err)
	}
	if owner != a || ds != dataset {
		return "", nil, store.Refuse("invalid_cursor")
	}
	if time.Now().UnixMilli() >= expires {
		return "", nil, store.Refuse("cursor_expired")
	}
	var current string
	err = tx.QueryRowContext(ctx, `SELECT "generation" FROM "tessera_access" WHERE "dataset_id"=$1 AND "principal"=$2 AND "authorization"=$3`, dataset, a.Principal, a.Authorization).Scan(&current)
	if err == sql.ErrNoRows {
		return "", nil, store.Refuse("view_changed")
	}
	if err != nil {
		return "", nil, dbError(ctx, err)
	}
	auth, err := authorization(ctx, tx, dataset, a)
	if err != nil && p.ErrorCode(err) != "permission_denied" {
		return "", nil, err
	}
	if err != nil || !auth.Grants.Read || current != gen {
		return "", nil, store.Refuse("view_changed")
	}
	return seq, withMeta(body, meta), nil
}

// withMeta puts a page set's schema back into a page stored without it. The
// page was encoded with its keys in order and "meta" sorts first, so the
// result is byte for byte the page as it was once stored whole. A page stored
// before the schema moved to the sequence carries its own and has none there.
func withMeta(body, meta []byte) []byte {
	if len(meta) == 0 || len(body) < 2 || body[0] != '{' {
		return body
	}
	served := make([]byte, 0, len(body)+len(meta)+9)
	served = append(served, `{"meta":`...)
	served = append(served, meta...)
	if body[1] != '}' {
		served = append(served, ',')
	}
	return append(served, body[1:]...)
}
func (s *Store) NextPage(ctx context.Context, a store.Access, dataset, cursor string) (json.RawMessage, error) {
	if err := s.enter(); err != nil {
		return nil, err
	}
	defer s.life.RUnlock()
	tx, err := s.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, dbError(ctx, err)
	}
	defer tx.Rollback()
	_, body, err := s.page(ctx, tx, a, dataset, cursor)
	if err != nil || len(body) > 0 {
		return body, err
	}
	// Pending: this page is computed now, once, and kept.
	_ = tx.Rollback()
	return s.continuePage(ctx, a, dataset, cursor)
}
func (s *Store) EndPage(ctx context.Context, a store.Access, dataset, cursor string) error {
	if err := s.enter(); err != nil {
		return err
	}
	defer s.life.RUnlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return dbError(ctx, err)
	}
	defer tx.Rollback()
	if err = s.serializeWrites(ctx, tx, dataset); err != nil {
		return err
	}
	seq, _, err := s.page(ctx, tx, a, dataset, cursor)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM "tessera_sequences" WHERE "id"=$1`, seq); err != nil {
		return dbError(ctx, err)
	}
	if err = tx.Commit(); err != nil {
		return dbError(ctx, err)
	}
	return nil
}
