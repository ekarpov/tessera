// Package sqlengine shares bounded evaluation, recovery and content authority
// across the SQLite and PostgreSQL public engine entry points.
package sqlengine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ekarpov/tessera/db/migrations"
	"github.com/ekarpov/tessera/internal/recordstate"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
	_ "github.com/mattn/go-sqlite3"
)

type Store struct {
	// wholeImageWrites evaluates every write from the complete dataset, as
	// all writes once were. Only a test sets it, to compare the two paths.
	wholeImageWrites bool
	// fetchHook, when a test sets it, is told of each record a sourced write
	// fetches by identity.
	fetchHook func()
	// unsortedReads answers sorted queries by reading and sorting the whole
	// scope, as they once were. Only a test sets it, to compare the two paths.
	unsortedReads bool
	// snapshotPages begins every page sequence as a snapshot, as they all
	// once were. Only a test sets it, to compare the two ways of paging.
	snapshotPages bool
	// sortHook, when a test sets it, is told each time a sorted query is
	// answered in SQL order, so a test can tell it was not the fallback.
	sortHook     func()
	db, reader   *sql.DB
	lock         *os.File
	life         sync.RWMutex
	closed       bool
	postgres     bool
	hostRegistry bool
	pgOwner      *sql.Conn
	// limits is read through bounds and written through setBounds: it changes
	// while requests are being served, when a dataset's limits are raised.
	limitsMu sync.RWMutex
	limits   p.Limits
	// boundsRefresh orders refreshBounds: each reads what is committed and
	// publishes it before the next reads.
	boundsRefresh sync.Mutex
	// defaults are the limits a dataset created here takes when its creation
	// names none. They are the deployment's configuration, and separate from
	// limits above, which is the widest any stored dataset declares and
	// governs how much of a request is read before its dataset is known.
	// Taking new datasets' limits from that widest value meant a configured
	// limit reached only the first dataset of a brand-new database.
	defaults p.Limits
	reads    []string
	path     string
	// executor identifies this running process. A claim records the executor
	// that made it, so another process can tell work in flight from work whose
	// executor is gone.
	executor          string
	journalMu         sync.Mutex
	contentDir        string
	cleanupContentDir string
	transferMu        sync.Mutex
	backupMu          sync.Mutex
	backupExportHook  func()
	// renditions bounds how many thumbnails are drawn at once, outside the
	// transfer lock.
	renditions      chan struct{}
	maintenanceMu   sync.Mutex
	stopMaintenance context.CancelFunc
	maintenance     sync.WaitGroup
}

// EnableReads advertises optional read capabilities for every transaction this
// store evaluates. Call it during composition, before serving requests.
func (s *Store) EnableReads(names ...string) error {
	if err := recordstate.CheckReadCapabilities(names); err != nil {
		return err
	}
	// Store them in the order the protocol names them, so discovery does not
	// depend on the order a configuration file happened to use.
	advertised := []string{}
	for _, name := range p.OptionalReadCapabilities {
		for _, enabled := range names {
			if name == enabled {
				advertised = append(advertised, name)
			}
		}
	}
	s.reads = advertised
	return nil
}

// Open creates or reopens one dataset. Existing databases retain their identity,
// version secret and limits; supplied limits apply only when first created.
func Open(ctx context.Context, path string, limits p.Limits) (result *Store, err error) {
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, store.Refuse("dataset_unavailable")
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, store.Refuse("dataset_unavailable")
	}
	lock, err := lockDatabase(path + ".lock")
	if err != nil {
		return nil, store.Refuse("dataset_unavailable")
	}
	s := &Store{lock: lock, path: path, renditions: make(chan struct{}, renditionSlots())}
	defer func() {
		if err != nil {
			s.Close()
		}
	}()
	file, e := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if e != nil {
		return nil, store.Refuse("dataset_unavailable")
	}
	e = file.Chmod(0600)
	file.Close()
	if e != nil {
		return nil, store.Refuse("dataset_unavailable")
	}
	u := url.URL{Scheme: "file", Path: path}
	q := url.Values{"_busy_timeout": {"5000"}, "_foreign_keys": {"on"}, "_journal_mode": {"WAL"}, "_synchronous": {"FULL"}, "_txlock": {"immediate"}}
	u.RawQuery = q.Encode()
	s.db, err = sql.Open("sqlite3", u.String())
	if err != nil {
		return nil, store.Refuse("dataset_unavailable")
	}
	// One write connection. SQLite admits one writer at a time whatever the
	// pool holds, and extra connections only compete for the lock through
	// SQLite's busy handler, which is not a queue: under a burst one could
	// lose for longer than the busy timeout and answer 503 to a write nothing
	// was wrong with. Waiting for this connection is a queue, and it honours
	// the request's context.
	s.db.SetMaxOpenConns(1)
	q.Set("_txlock", "deferred")
	u.RawQuery = q.Encode()
	s.reader, err = sql.Open("sqlite3", u.String())
	if err != nil {
		return nil, store.Refuse("dataset_unavailable")
	}
	s.reader.SetMaxOpenConns(4)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, dbError(ctx, err)
	}
	defer tx.Rollback()
	var version int
	if err = tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return nil, dbError(ctx, err)
	}
	if version == 0 {
		if _, err = tx.ExecContext(ctx, migrations.SQLiteV1); err != nil {
			return nil, dbError(ctx, err)
		}
		if limits.MaxMaterializedBytes <= 0 {
			limits.MaxMaterializedBytes = 64 << 20
		}
		if limits.MaxScannedRows <= 0 || limits.IdempotencyRetentionSeconds <= 0 {
			return nil, store.Refuse("internal_error")
		}
		engine, e := recordstate.New(limits)
		if e != nil {
			return nil, e
		}
		image, e := engine.Image(ctx)
		if e != nil {
			return nil, e
		}
		raw, e := json.Marshal(limits)
		if e != nil {
			return nil, e
		}
		// Written against the original schema, which the migrations below then
		// reshape: a new database is created at version 1 and migrated forward
		// like any other, so this row still carries the column that keyed it.
		_, err = tx.ExecContext(ctx, `INSERT INTO "tessera_dataset" ("singleton","dataset_id","secret","next_id","watermark","limits") VALUES (1,$1,$2,$3,$4,$5)`, image.ID, image.Secret[:], strconv.FormatUint(image.Next, 10), image.Watermark, raw)
		if err != nil {
			return nil, dbError(ctx, err)
		}
	} else if version > 15 {
		return nil, store.Refuse("not_supported")
	}
	if version < 2 {
		if _, err = tx.ExecContext(ctx, migrations.SQLiteV2); err != nil {
			return nil, dbError(ctx, err)
		}
	}
	if version < 3 {
		if _, err = tx.ExecContext(ctx, migrations.SQLiteV3); err != nil {
			return nil, dbError(ctx, err)
		}
	}
	if version < 4 {
		if _, err = tx.ExecContext(ctx, migrations.SQLiteV4); err != nil {
			return nil, dbError(ctx, err)
		}
	}
	if version < 5 {
		if _, err = tx.ExecContext(ctx, migrations.SQLiteV5); err != nil {
			return nil, dbError(ctx, err)
		}
	}
	if version < 6 {
		if _, err = tx.ExecContext(ctx, migrations.SQLiteV6); err != nil {
			return nil, dbError(ctx, err)
		}
	}
	if version < 7 {
		if _, err = tx.ExecContext(ctx, migrations.SQLiteV7); err != nil {
			return nil, dbError(ctx, err)
		}
	}
	if version < 8 {
		if _, err = tx.ExecContext(ctx, migrations.SQLiteV8); err != nil {
			return nil, dbError(ctx, err)
		}
	}
	if version < 9 {
		if _, err = tx.ExecContext(ctx, migrations.SQLiteV9); err != nil {
			return nil, dbError(ctx, err)
		}
	}
	if version < 10 {
		if _, err = tx.ExecContext(ctx, migrations.SQLiteV10); err != nil {
			return nil, dbError(ctx, err)
		}
	}
	if version < 11 {
		if _, err = tx.ExecContext(ctx, migrations.SQLiteV11); err != nil {
			return nil, dbError(ctx, err)
		}
	}
	if version < 12 {
		if _, err = tx.ExecContext(ctx, migrations.SQLiteV12); err != nil {
			return nil, dbError(ctx, err)
		}
	}
	if version < 13 {
		if _, err = tx.ExecContext(ctx, migrations.SQLiteV13); err != nil {
			return nil, dbError(ctx, err)
		}
	}
	if version < 14 {
		if _, err = tx.ExecContext(ctx, migrations.SQLiteV14); err != nil {
			return nil, dbError(ctx, err)
		}
	}
	if version < 15 {
		if _, err = tx.ExecContext(ctx, migrations.SQLiteV15); err != nil {
			return nil, dbError(ctx, err)
		}
	}
	var bounds p.Limits
	if bounds, err = storageLimits(ctx, tx); err != nil {
		return nil, err
	}
	s.setBounds(bounds)
	s.defaults = creationDefaults(limits)
	// The exclusive lock on the file beside the database proves no other
	// executor is alive, so every in-progress claim was left by a process that
	// is gone. An in-progress claim cannot have committed data: outcomes and
	// data commit atomically. SQLite has no cross-process session lock, so it
	// stays one process per database and answers this by exclusion rather than
	// by asking each executor whether it is still there.
	failure, _ := p.MarshalWire(map[string]any{"error": &p.Error{Code: "internal_error", Message: "Execution was interrupted before commit"}})
	if _, err = tx.ExecContext(ctx, `UPDATE "tessera_receipts" SET "state"='failed',"status"=500,"body"=$1 WHERE "state"='in_progress'`, failure); err != nil {
		return nil, dbError(ctx, err)
	}
	if err = tx.Commit(); err != nil {
		return nil, dbError(ctx, err)
	}
	if err = s.journalRetirements(ctx); err != nil {
		return nil, err
	}
	if err = s.checkRestoreFence(ctx); err != nil {
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error {
	s.maintenanceMu.Lock()
	if s.stopMaintenance != nil {
		s.stopMaintenance()
	}
	s.maintenanceMu.Unlock()
	s.maintenance.Wait()
	s.life.Lock()
	defer s.life.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var err error
	if s.pgOwner != nil {
		s.pgOwner.Close()
	}
	if s.reader != nil {
		err = s.reader.Close()
	}
	if s.db != nil {
		if e := s.db.Close(); e != nil {
			err = e
		}
	}
	if s.lock != nil {
		if e := s.lock.Close(); e != nil {
			err = e
		}
	}
	if err != nil {
		return store.Refuse("dataset_unavailable")
	}
	return nil
}
func (s *Store) enter() error {
	s.life.RLock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if s.closed || (s.pgOwner != nil && s.pgOwner.PingContext(ctx) != nil) {
		s.life.RUnlock()
		return store.Refuse("dataset_unavailable")
	}
	return nil
}
func dbError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return store.Refuse("dataset_unavailable")
}

func (s *Store) SetAuthorization(ctx context.Context, dataset string, a store.Access, g store.Authorization) error {
	if !a.Valid() || !g.View.Valid() || (!g.View.Full() && (g.Grants.Write || g.Grants.Schema || g.Grants.Erase)) {
		return store.Refuse("permission_denied")
	}
	if err := s.enter(); err != nil {
		return err
	}
	defer s.life.RUnlock()
	raw, err := json.Marshal(g.Grants)
	if err != nil {
		return store.Refuse("internal_error")
	}
	view, err := json.Marshal(g.View)
	if err != nil {
		return err
	}
	generation, err := cursorToken()
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return dbError(ctx, err)
	}
	defer tx.Rollback()
	// A grant is read as part of every image, so changing one is a write to
	// that dataset and waits its turn with the rest.
	if err = s.serializeWrites(ctx, tx, dataset); err != nil {
		return err
	}
	// Administration is composition, not protocol, but a grant still has to be
	// over something: the access table has no foreign key to say so.
	var live int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM "tessera_dataset" WHERE "dataset_id"=$1`, dataset).Scan(&live); err != nil {
		return dbError(ctx, err)
	}
	if live == 0 {
		return store.Refuse("not_found")
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO "tessera_access" ("dataset_id","principal","authorization","grants","generation","view") VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT ("dataset_id","principal","authorization") DO UPDATE SET "grants"=excluded."grants","generation"=excluded."generation","view"=excluded."view"`, dataset, a.Principal, a.Authorization, raw, generation, view)
	if err != nil {
		return dbError(ctx, err)
	}
	if err = tx.Commit(); err != nil {
		return dbError(ctx, err)
	}
	return nil
}
func (s *Store) Revoke(ctx context.Context, dataset string, a store.Access) error {
	if err := s.enter(); err != nil {
		return err
	}
	defer s.life.RUnlock()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM "tessera_access" WHERE "dataset_id"=$1 AND "principal"=$2 AND "authorization"=$3`, dataset, a.Principal, a.Authorization); err != nil {
		return dbError(ctx, err)
	}
	return nil
}
func authorization(ctx context.Context, tx *sql.Tx, dataset string, a store.Access) (store.Authorization, error) {
	if !a.Valid() {
		return store.Authorization{}, store.Refuse("permission_denied")
	}
	var raw, view []byte
	err := tx.QueryRowContext(ctx, `SELECT "grants","view" FROM "tessera_access" WHERE "dataset_id"=$1 AND "principal"=$2 AND "authorization"=$3`, dataset, a.Principal, a.Authorization).Scan(&raw, &view)
	if err == sql.ErrNoRows {
		return store.Authorization{}, store.Refuse("permission_denied")
	}
	if err != nil {
		return store.Authorization{}, dbError(ctx, err)
	}
	var g store.Grants
	if json.Unmarshal(raw, &g) != nil {
		return store.Authorization{}, store.Refuse("internal_error")
	}
	if !g.Current() {
		return store.Authorization{}, store.Refuse("permission_denied")
	}
	var v store.View
	if json.Unmarshal(view, &v) != nil {
		return store.Authorization{}, store.Refuse("internal_error")
	}
	return store.Authorization{Grants: g, View: v}, nil
}

// Datasets lists every dataset in this storage that the caller holds a grant
// on, in identity order. A dataset the caller may not use is absent rather than
// refused, so the listing reports nothing about what else is here. An empty
// listing is not an error, and neither is an invalid access.
func (s *Store) Datasets(ctx context.Context, a store.Access) ([]store.Info, error) {
	held := []store.Info{}
	if !a.Valid() {
		return held, nil
	}
	ids, err := s.granted(ctx, a)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		// A grant can expire or be revoked between the two reads, and a dataset
		// can be erased. Either way it is one the caller may not use, and the
		// listing omits it rather than reporting it. `unavailable` is among
		// these because a caller with no usable grant is told that rather than
		// permission_denied, so that a direct address conceals what the listing
		// conceals.
		info, e := s.Info(ctx, a, id)
		switch p.ErrorCode(e) {
		case "permission_denied", "not_found", "unavailable":
			continue
		}
		if e != nil {
			return nil, e
		}
		held = append(held, info)
	}
	return held, nil
}

// Reach names the datasets one authorization holds access on, which is not
// the dataset its approval named: creating a dataset installs access to it
// for the caller that created it.
func (s *Store) Reach(ctx context.Context, a store.Access) ([]string, error) {
	return s.granted(ctx, a)
}

// granted names the datasets the caller holds an access row on. The row alone
// does not mean the grant is current; Info decides that.
func (s *Store) granted(ctx context.Context, a store.Access) ([]string, error) {
	if err := s.enter(); err != nil {
		return nil, err
	}
	defer s.life.RUnlock()
	tx, err := s.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, dbError(ctx, err)
	}
	defer tx.Rollback()
	return s.grantedIn(ctx, tx, a)
}

// grantedIn is the same question asked inside a transaction the caller owns.
func (s *Store) grantedIn(ctx context.Context, tx *sql.Tx, a store.Access) ([]string, error) {
	if !a.Valid() {
		return nil, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT x."dataset_id" FROM "tessera_access" x JOIN "tessera_dataset" d ON d."dataset_id"=x."dataset_id" WHERE x."principal"=$1 AND x."authorization"=$2 ORDER BY x."dataset_id"`, a.Principal, a.Authorization)
	if err != nil {
		return nil, dbError(ctx, err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, dbError(ctx, err)
	}
	return ids, nil
}

// The named dataset is checked by the evaluator restored from the image, which
// carries that image's identity. Deciding it here as well would leave one of
// the two decisions unexercised, and the two could drift apart.
func (s *Store) Info(ctx context.Context, a store.Access, dataset string) (store.Info, error) {
	if err := s.enter(); err != nil {
		return store.Info{}, err
	}
	defer s.life.RUnlock()
	tx, err := s.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return store.Info{}, dbError(ctx, err)
	}
	defer tx.Rollback()
	h, err := s.load(ctx, tx, a, dataset, false)
	if err != nil {
		return store.Info{}, err
	}
	engine, err := recordstate.Restore(h.image, h.limits, a, h.auth, s.reads)
	if err != nil {
		return store.Info{}, err
	}
	info, err := engine.Info(ctx, a, dataset)
	if err != nil {
		return info, err
	}
	var replaced sql.NullString
	if err = tx.QueryRowContext(ctx, `SELECT "display_name","replaced_dataset_id" FROM "tessera_dataset" WHERE "dataset_id"=$1`, dataset).Scan(&info.DisplayName, &replaced); err != nil {
		return info, dbError(ctx, err)
	}
	info.ReplacedDatasetID = replaced.String
	// Optional read capabilities come from the evaluator; sharing and content
	// are properties of this engine and its composition.
	info.Capabilities = append(info.Capabilities, "sharing")
	if s.contentDir != "" {
		info.Capabilities = append(info.Capabilities, "content")
		if err = tx.QueryRowContext(ctx, `SELECT "id" FROM "tessera_content_stores" WHERE "dataset_id"=$1 AND "active"=1`, dataset).Scan(&info.ContentStoreID); err != nil {
			return info, dbError(ctx, err)
		}
	}
	var generation string
	if err = tx.QueryRowContext(ctx, `SELECT "generation" FROM "tessera_access" WHERE "dataset_id"=$1 AND "principal"=$2 AND "authorization"=$3`, dataset, a.Principal, a.Authorization).Scan(&generation); err != nil {
		return info, dbError(ctx, err)
	}
	info.ViewVersion = "view:" + generation
	return info, nil
}
func (s *Store) Transact(ctx context.Context, a store.Access, dataset string, fn func(store.Tx) error) error {
	return s.WithTx(ctx, a, dataset, func(tx store.Tx, _ *sql.Tx) error { return fn(tx) })
}

// WithTx commits core records and host SQL together. The SQL transaction belongs
// to this store: callers must not commit, roll back, retain, or concurrently use
// it. A callback must perform no network I/O and must return every operation error.
//
// fn may run twice. A write is first evaluated against only the records it
// touches; one that needs the whole dataset is rolled back to a savepoint —
// the host's SQL with it — and fn runs again from the complete image. So fn
// must keep nothing outside itself that a second run would add to.
func (s *Store) WithTx(ctx context.Context, a store.Access, dataset string, fn func(store.Tx, *sql.Tx) error) error {
	if err := s.enter(); err != nil {
		return err
	}
	defer s.life.RUnlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return dbError(ctx, err)
	}
	defer tx.Rollback()
	if err = s.evaluate(ctx, tx, a, dataset, fn); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return dbError(ctx, err)
	}
	return nil
}

// Read evaluates a request that changes nothing. It takes no writer's turn,
// runs on the read pool in a read-only transaction, and never diffs or saves
// the state it built — three things a read was paying for and using none of.
//
// The image it builds is still the whole dataset; narrowing that is a separate
// change. What this removes is the writer's lock, the defensive copy the
// evaluator makes so a failed write can be dropped, and the second copy taken
// afterwards to compare against.
// ReadSchema answers from the types alone. The objects are not fetched or
// decoded, which is the whole of the saving: describing one type used to cost
// reading every record in the dataset.
func (s *Store) ReadSchema(ctx context.Context, a store.Access, dataset string, fn func(store.Tx) error) error {
	return s.read(ctx, a, dataset, false, func(tx store.Tx) error {
		return fn(store.SchemaOnly{ReadOnly: store.ReadOnly{Tx: tx}})
	})
}

// ReadRecord fetches the records it is asked for and no others, so reading
// one record costs one record rather than the dataset holding it.
func (s *Store) ReadRecord(ctx context.Context, a store.Access, dataset string, fn func(store.Tx) error) error {
	return s.read(ctx, a, dataset, false, func(tx store.Tx) error {
		return fn(store.RecordOnly{ReadOnly: store.ReadOnly{Tx: tx}})
	})
}

// Read evaluates a request that reads and may scan. It no longer loads the
// dataset: records are fetched by identity or streamed by type as the request
// needs them, so a query for ten rows reads about ten rows.
//
// Only reads. A write computes what changed by comparing the image before and
// after, so an image missing records would describe their absence as a
// deletion — which is why the write path still loads everything, and why that
// is a separate change rather than part of this one.
func (s *Store) Read(ctx context.Context, a store.Access, dataset string, fn func(store.Tx) error) error {
	return s.read(ctx, a, dataset, false, func(tx store.Tx) error { return fn(store.ReadOnly{Tx: tx}) })
}

func (s *Store) read(ctx context.Context, a store.Access, dataset string, objects bool, fn func(store.Tx) error) error {
	if err := s.enter(); err != nil {
		return err
	}
	defer s.life.RUnlock()
	tx, err := s.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return dbError(ctx, err)
	}
	defer tx.Rollback()
	h, err := s.load(ctx, tx, a, dataset, objects)
	if err != nil {
		return err
	}
	// Adopted rather than copied: this image was decoded a moment ago, from
	// this transaction, and nothing else will ever look at it.
	engine, err := recordstate.Adopt(h.image, h.limits, a, h.auth, s.reads)
	if err != nil {
		return err
	}
	if s.contentDir != "" {
		engine.SetMediaAuthority(func(ref string) error { return s.authorizeAttachment(ctx, tx, a, ref) })
	}
	if !objects {
		// Built without the records, so a request for one by identity reads
		// that row and stops. An evaluation that would scan is refused by the
		// wrapper the caller supplied rather than answered from what is here.
		engine.SetObjectSource(func(id string) (recordstate.Row, bool, error) { return fetchRow(ctx, tx, dataset, id) })
		engine.SetScanSource(func(types []string, visit func(string, recordstate.Row) (bool, error)) error {
			return s.scanObjects(ctx, tx, dataset, types, visit)
		})
		if !s.unsortedReads {
			engine.SetSortedScanSource(func(types []string, keys []recordstate.SortKey, from *recordstate.SortFrom, admit func(int) error, visit func(string, recordstate.Row) (bool, error)) (bool, error) {
				return s.sortedObjects(ctx, tx, dataset, h.auth.View.SequenceCut(), types, keys, from, admit, visit)
			})
		}
	}
	return engine.Evaluate(ctx, a, dataset, fn)
}

// scanObjects streams the records of the named types in identity order and
// stops when visit says it has what it needs.
//
// Identity order is byte order over the object identifier, because that is
// what the evaluator sorts by and what it verifies as rows arrive. SQLite's
// default text collation is already byte order. PostgreSQL's is the database
// collation, which a locale can make case-folding and punctuation-blind — and
// an object identifier may contain both — so it is asked for COLLATE "C"
// explicitly, and migration 9 gives that order an index to read.
//
// Rows are decoded one at a time, so a query that stops after ten matches
// decodes roughly ten rows rather than every row of the type.
func (s *Store) scanObjects(ctx context.Context, tx *sql.Tx, dataset string, types []string, visit func(string, recordstate.Row) (bool, error)) error {
	return s.scanObjectsFrom(ctx, tx, dataset, types, "", visit)
}

// scanObjectsFrom is scanObjects starting after an identity, for a page that
// continues a sequence from where the last one ended.
func (s *Store) scanObjectsFrom(ctx context.Context, tx *sql.Tx, dataset string, types []string, after string, visit func(string, recordstate.Row) (bool, error)) error {
	if len(types) == 0 {
		return nil
	}
	args := make([]any, 0, len(types)+3)
	args = append(args, dataset)
	placeholders := make([]string, len(types))
	for i, id := range types {
		args = append(args, id)
		placeholders[i] = "$" + strconv.Itoa(i+2)
	}
	identity := `"object_id"`
	if s.postgres {
		identity = `"object_id" COLLATE "C"`
	}
	resume := "$" + strconv.Itoa(len(types)+2)
	limit := "$" + strconv.Itoa(len(types)+3)
	query := `SELECT "object_id","type_id","fields" FROM "tessera_objects" WHERE "dataset_id"=$1 AND "type_id" IN (` +
		strings.Join(placeholders, ",") + `) AND ` + identity + ` > ` + resume + ` ORDER BY ` + identity + ` LIMIT ` + limit
	// Read in batches that start small and grow, each resuming after the last
	// identity the one before returned. One unbounded statement let a query
	// that stops after ten rows cost the whole type on PostgreSQL: the server
	// sent every row, and closing the cursor early still read them all off the
	// connection. Now an early stop reads at most one batch past what it kept,
	// and a scan to the end pays one round trip per batch.
	last, batch := after, scanBatchFirst
	for {
		rows, err := tx.QueryContext(ctx, query, append(args, last, batch)...)
		if err != nil {
			return dbError(ctx, err)
		}
		read := 0
		for rows.Next() {
			var id string
			var row recordstate.Row
			var raw []byte
			if err = rows.Scan(&id, &row.TypeID, &raw); err != nil {
				rows.Close()
				return dbError(ctx, err)
			}
			read++
			last = id
			if json.Unmarshal(raw, &row.Fields) != nil {
				rows.Close()
				return store.Refuse("internal_error")
			}
			row.Sequence, _ = strconv.ParseUint(id, 10, 64)
			row.Bytes = len(raw)
			more, e := visit(id, row)
			if e != nil || !more {
				rows.Close()
				return e
			}
		}
		// dbError maps every error it is given to dataset_unavailable and does
		// not check for nil, so rows.Err() is checked before it is passed on.
		err = rows.Err()
		rows.Close()
		if err != nil {
			return dbError(ctx, err)
		}
		if read < batch {
			return nil
		}
		batch = min(batch*4, scanBatchLast)
	}
}

// A scan's first batch is small because most scans that stop early stop
// soon; later ones grow so that a scan to the end is not a round trip a row.
const (
	scanBatchFirst = 64
	scanBatchLast  = 4096
)

func (s *Store) evaluate(ctx context.Context, sqltx *sql.Tx, a store.Access, dataset string, fn func(store.Tx, *sql.Tx) error) error {
	// Before the image is read, because the image is read, changed and written
	// back: two of these against one dataset must not interleave.
	if err := s.serializeWrites(ctx, sqltx, dataset); err != nil {
		return err
	}
	if !s.wholeImageWrites {
		if done, err := s.evaluateSourced(ctx, sqltx, a, dataset, fn); done {
			return err
		}
	}
	h, err := s.load(ctx, sqltx, a, dataset, true)
	var refusal *p.Error
	if errors.As(err, &refusal) && refusal.Details["reason"] == "materialization_limit" {
		// The write needs the whole dataset in memory and the dataset is
		// larger than this server holds at once: a refused write, not a
		// query the caller could shrink.
		return tooLargeToWrite(h.limits)
	}
	if err != nil {
		return err
	}
	before := h.image
	engine, err := recordstate.Restore(before, h.limits, a, h.auth, s.reads)
	if err != nil {
		return err
	}
	if s.contentDir != "" {
		engine.SetMediaAuthority(func(ref string) error { return s.authorizeAttachment(ctx, sqltx, a, ref) })
	}
	if err = engine.Transact(ctx, a, dataset, func(tx store.Tx) error { return fn(tx, sqltx) }); err != nil {
		return err
	}
	after, err := engine.Image(ctx)
	if err != nil {
		return err
	}
	if err = s.persist(ctx, sqltx, h.limits, before, after, outside{}); err != nil {
		return err
	}
	if s.contentDir != "" && before.Watermark != after.Watermark {
		return s.persistAttachments(ctx, sqltx, before, after)
	}
	return nil
}

// evaluateSourced runs a write against the records it touches, fetched by
// identity, rather than against the whole dataset, and writes back only what
// changed. Writes cost what they touch instead of what the dataset holds.
//
// A query streams from the storage with the write's own changes laid over it.
// A schema change that rewrites records needs the whole dataset and cannot be
// answered this way. The evaluator says so, the
// attempt is rolled back to a savepoint, including anything a host did in it,
// and done is false: the caller evaluates again from a complete image, as
// every write used to.
func (s *Store) evaluateSourced(ctx context.Context, sqltx *sql.Tx, a store.Access, dataset string, fn func(store.Tx, *sql.Tx) error) (done bool, err error) {
	if _, err = sqltx.ExecContext(ctx, `SAVEPOINT "tessera_sourced"`); err != nil {
		return true, dbError(ctx, err)
	}
	h, err := s.load(ctx, sqltx, a, dataset, false)
	if err != nil {
		return true, err
	}
	engine, err := recordstate.Restore(h.image, h.limits, a, h.auth, s.reads)
	if err != nil {
		return true, err
	}
	engine.SetObjectSource(func(id string) (recordstate.Row, bool, error) {
		if s.fetchHook != nil {
			s.fetchHook()
		}
		return fetchRow(ctx, sqltx, dataset, id)
	})
	engine.SetScanSource(func(types []string, visit func(string, recordstate.Row) (bool, error)) error {
		return s.scanObjects(ctx, sqltx, dataset, types, visit)
	})
	engine.EnableSourcedWrites()
	if s.contentDir != "" {
		engine.SetMediaAuthority(func(ref string) error { return s.authorizeAttachment(ctx, sqltx, a, ref) })
	}
	failed := engine.Transact(ctx, a, dataset, func(tx store.Tx) error { return fn(tx, sqltx) })
	if engine.NeededImage() {
		if _, err = sqltx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT "tessera_sourced"`); err != nil {
			return true, dbError(ctx, err)
		}
		if _, err = sqltx.ExecContext(ctx, `RELEASE SAVEPOINT "tessera_sourced"`); err != nil {
			return true, dbError(ctx, err)
		}
		return false, nil
	}
	if _, err = sqltx.ExecContext(ctx, `RELEASE SAVEPOINT "tessera_sourced"`); err != nil {
		return true, dbError(ctx, err)
	}
	if failed != nil {
		return true, failed
	}
	after, err := engine.Image(ctx)
	if err != nil {
		return true, err
	}
	before := h.image
	before.Objects = engine.Originals()
	rest, err := outsideOf(ctx, sqltx, dataset, before)
	if err != nil {
		return true, err
	}
	if err = s.persist(ctx, sqltx, h.limits, before, after, rest); err != nil {
		return true, err
	}
	if s.contentDir != "" && before.Watermark != after.Watermark {
		return true, s.persistAttachments(ctx, sqltx, before, after)
	}
	return true, nil
}

// fetchRow reads one record by identity.
func fetchRow(ctx context.Context, tx *sql.Tx, dataset, id string) (recordstate.Row, bool, error) {
	var row recordstate.Row
	var raw []byte
	var mintedID string
	err := tx.QueryRowContext(ctx, `SELECT "object_id","type_id","fields" FROM "tessera_objects" WHERE "dataset_id"=$1 AND "object_id"=$2`, dataset, id).Scan(&mintedID, &row.TypeID, &raw)
	if err == sql.ErrNoRows {
		return row, false, nil
	}
	if err != nil {
		return row, false, dbError(ctx, err)
	}
	if json.Unmarshal(raw, &row.Fields) != nil {
		return row, false, store.Refuse("internal_error")
	}
	row.Sequence, _ = strconv.ParseUint(mintedID, 10, 64)
	return row, true, nil
}

// outsideOf measures the dataset beyond the records a write touched: the
// count and stored bytes kept on its row, less the touched records' own.
func outsideOf(ctx context.Context, tx *sql.Tx, dataset string, touched recordstate.Image) (outside, error) {
	var rows, size int64
	if err := tx.QueryRowContext(ctx, `SELECT "object_count","object_bytes" FROM "tessera_dataset" WHERE "dataset_id"=$1`, dataset).Scan(&rows, &size); err != nil {
		return outside{}, dbError(ctx, err)
	}
	for _, row := range touched.Objects {
		raw, err := p.MarshalWire(row.Fields)
		if err != nil {
			return outside{}, err
		}
		rows--
		size -= int64(len(raw))
	}
	return outside{rows: int(rows), bytes: int(size)}, nil
}

// bounds is the storage's declared bound: the widest any stored dataset
// declares, which governs how much of a request is read before its dataset
// is known.
func (s *Store) bounds() p.Limits {
	s.limitsMu.RLock()
	defer s.limitsMu.RUnlock()
	return s.limits
}

func (s *Store) setBounds(limits p.Limits) {
	s.limitsMu.Lock()
	defer s.limitsMu.Unlock()
	s.limits = limits
}
