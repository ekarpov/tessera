package sqlengine

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"

	"github.com/ekarpov/tessera/db/migrations"
	"github.com/ekarpov/tessera/internal/recordstate"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// OpenPostgres opens a database this process shares with any other process
// opening it. Each holds a session advisory lock under its own executor
// identity, and a claim records the executor that made it, so a process can
// tell another's work in flight from work whose executor is gone without
// consulting a clock. fencePath must remain on durable storage independently of
// database backups.
func OpenPostgres(ctx context.Context, dsn, fencePath string, limits p.Limits) (result *Store, err error) {
	return openPostgres(ctx, dsn, fencePath, limits, false)
}

func openPostgres(ctx context.Context, dsn, fencePath string, limits p.Limits, requireEmpty bool) (result *Store, err error) {
	if fencePath == "" {
		return nil, store.Refuse("invalid_field")
	}
	fencePath, err = filepath.Abs(fencePath)
	if err != nil {
		return nil, store.Refuse("dataset_unavailable")
	}
	if err = os.MkdirAll(filepath.Dir(fencePath), 0700); err != nil {
		return nil, store.Refuse("dataset_unavailable")
	}
	executor, err := newExecutor()
	if err != nil {
		return nil, err
	}
	s := &Store{postgres: true, path: fencePath, executor: executor, renditions: make(chan struct{}, renditionSlots())}
	defer func() {
		if err != nil {
			s.Close()
		}
	}()
	s.db, err = sql.Open("tessera-postgres", dsn)
	if err != nil {
		return nil, store.Refuse("dataset_unavailable")
	}
	s.db.SetMaxOpenConns(8)
	s.reader, err = sql.Open("tessera-postgres", dsn)
	if err != nil {
		return nil, store.Refuse("dataset_unavailable")
	}
	s.reader.SetMaxOpenConns(4)
	s.pgOwner, err = s.db.Conn(ctx)
	if err != nil {
		return nil, dbError(ctx, err)
	}
	// A process of the previous kind holds this key for as long as it runs and
	// resolves every in-progress claim as abandoned, including ones this process
	// would be working on. Taking the key proves none is running; it is let go
	// immediately, because this process does not want to exclude its own kind.
	var alone bool
	if err = s.pgOwner.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, legacyProcessKey).Scan(&alone); err != nil {
		return nil, dbError(ctx, err)
	}
	if !alone {
		return nil, store.Refuse("dataset_unavailable")
	}
	if _, err = s.pgOwner.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, legacyProcessKey); err != nil {
		return nil, dbError(ctx, err)
	}
	// This executor's own key, held on this connection for the life of the
	// process and taken before any claim can be made under it. Another process
	// learns this one has died by taking it. enter refuses every operation if
	// this connection is gone, because a lost lock means another process may
	// already have resolved this one's claims.
	var mine bool
	if err = s.pgOwner.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, executorKey(s.executor)).Scan(&mine); err != nil {
		return nil, dbError(ctx, err)
	}
	if !mine {
		return nil, store.Refuse("internal_error")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, dbError(ctx, err)
	}
	defer tx.Rollback()
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT to_regclass('tessera_migration') IS NOT NULL`).Scan(&exists); err != nil {
		return nil, dbError(ctx, err)
	}
	if exists && requireEmpty {
		return nil, store.Refuse("conflict")
	}
	if !exists {
		if _, err = tx.ExecContext(ctx, migrations.PostgresV1); err != nil {
			return nil, dbError(ctx, err)
		}
		if limits.MaxMaterializedBytes <= 0 || limits.MaxScannedRows <= 0 || limits.IdempotencyRetentionSeconds <= 0 {
			return nil, store.Refuse("invalid_field")
		}
		state, e := recordstate.New(limits)
		if e != nil {
			return nil, e
		}
		image, e := state.Image(ctx)
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
		if _, err = tx.ExecContext(ctx, `INSERT INTO "tessera_dataset" ("singleton","dataset_id","secret","next_id","watermark","limits") VALUES (1,$1,$2,$3,$4,$5)`, image.ID, image.Secret[:], strconv.FormatUint(image.Next, 10), image.Watermark, raw); err != nil {
			return nil, dbError(ctx, err)
		}
	}
	var version int
	if err = tx.QueryRowContext(ctx, `SELECT "version" FROM "tessera_migration"`).Scan(&version); err != nil || version < 1 || version > 16 {
		return nil, store.Refuse("not_supported")
	}
	if version < 2 {
		if _, err = tx.ExecContext(ctx, migrations.PostgresV2); err != nil {
			return nil, dbError(ctx, err)
		}
	}
	if version < 3 {
		if _, err = tx.ExecContext(ctx, migrations.PostgresV3); err != nil {
			return nil, dbError(ctx, err)
		}
	}
	if version < 4 {
		if _, err = tx.ExecContext(ctx, migrations.PostgresV4); err != nil {
			return nil, dbError(ctx, err)
		}
	}
	if version < 5 {
		if _, err = tx.ExecContext(ctx, migrations.PostgresV5); err != nil {
			return nil, dbError(ctx, err)
		}
	}
	if version < 6 {
		if _, err = tx.ExecContext(ctx, migrations.PostgresV6); err != nil {
			return nil, dbError(ctx, err)
		}
	}
	if version < 7 {
		if _, err = tx.ExecContext(ctx, migrations.PostgresV7); err != nil {
			return nil, dbError(ctx, err)
		}
	}
	if version < 8 {
		if _, err = tx.ExecContext(ctx, migrations.PostgresV8); err != nil {
			return nil, dbError(ctx, err)
		}
	}
	if version < 9 {
		if _, err = tx.ExecContext(ctx, migrations.PostgresV9); err != nil {
			return nil, dbError(ctx, err)
		}
	}
	if version < 10 {
		if _, err = tx.ExecContext(ctx, migrations.PostgresV10); err != nil {
			return nil, dbError(ctx, err)
		}
	}
	if version < 11 {
		if _, err = tx.ExecContext(ctx, migrations.PostgresV11); err != nil {
			return nil, dbError(ctx, err)
		}
	}
	if version < 12 {
		if _, err = tx.ExecContext(ctx, migrations.PostgresV12); err != nil {
			return nil, dbError(ctx, err)
		}
	}
	if version < 13 {
		if _, err = tx.ExecContext(ctx, migrations.PostgresV13); err != nil {
			return nil, dbError(ctx, err)
		}
	}
	if version < 14 {
		if _, err = tx.ExecContext(ctx, migrations.PostgresV14); err != nil {
			return nil, dbError(ctx, err)
		}
	}
	if version < 15 {
		if _, err = tx.ExecContext(ctx, migrations.PostgresV15); err != nil {
			return nil, dbError(ctx, err)
		}
	}
	if version < 16 {
		if _, err = tx.ExecContext(ctx, migrations.PostgresV16); err != nil {
			return nil, dbError(ctx, err)
		}
		if err = s.migrateRecordFields(ctx, tx); err != nil {
			return nil, err
		}
	}
	var bounds p.Limits
	if bounds, err = storageLimits(ctx, tx); err != nil {
		return nil, err
	}
	s.setBounds(bounds)
	s.defaults = creationDefaults(limits)
	if err = tx.Commit(); err != nil {
		return nil, dbError(ctx, err)
	}
	// Only claims whose executor is gone. Another process may be mid-transaction
	// on the rest, which is the whole reason this is not a blanket update.
	if err = s.resolveAbandonedClaims(ctx); err != nil {
		return nil, err
	}
	if err = s.journalRetirements(ctx); err != nil {
		return nil, err
	}
	if err = s.checkRestoreFence(ctx); err != nil {
		return nil, err
	}
	return s, nil
}
