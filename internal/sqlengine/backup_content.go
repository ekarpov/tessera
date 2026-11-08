package sqlengine

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ekarpov/tessera/pkg/store"
)

// lockTransfers orders filesystem checkpoints across processes sharing the same
// local byte directory. The file must never be unlinked while a server runs.
func (s *Store) lockTransfers(ctx context.Context) (func(), error) {
	s.transferMu.Lock()
	if s.byteDirectory() == "" {
		return s.transferMu.Unlock, nil
	}
	for {
		f, err := lockDatabase(filepath.Join(s.byteDirectory(), ".transfer.lock"))
		if err == nil {
			return func() { f.Close(); s.transferMu.Unlock() }, nil
		}
		// Retry only contention, not permission, unsupported locking or I/O failure.
		if !lockContended(err) {
			s.transferMu.Unlock()
			return nil, store.Refuse("store_unavailable")
		}
		select {
		case <-ctx.Done():
			s.transferMu.Unlock()
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

type backupPin struct {
	name string
	size int64
}

const maxBackupFiles = 256

// Called under the filesystem transfer lock and using the already pinned SQL
// snapshot. Committed prefixes are immutable: PATCH may truncate only a tail
// beyond the latest checkpoint, compare a replay, or append. Hard links retain
// the inode through cleanup; CopyN later excludes concurrent appends.
func (s *Store) pinBackupContent(ctx context.Context, tx *sql.Tx) ([]backupPin, string, error) {
	if s.byteDirectory() == "" {
		return nil, "", nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT "upload_id","received_bytes" FROM "tessera_imports" WHERE "state"='open' UNION ALL SELECT "upload_id","byte_count" FROM "tessera_items" WHERE "deleted"=0 LIMIT $1`, maxBackupFiles+1)
	if err != nil {
		return nil, "", dbError(ctx, err)
	}
	var pins []backupPin
	var total int64
	for rows.Next() {
		var pin backupPin
		if err = rows.Scan(&pin.name, &pin.size); err != nil {
			break
		}
		if !strings.HasPrefix(pin.name, "u-") || filepath.Base(pin.name) != pin.name || pin.size < 0 || pin.size > maxItemBytes {
			err = store.Refuse("store_unavailable")
			break
		}
		total += pin.size
		pins = append(pins, pin)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, "", dbError(ctx, err)
	}
	if len(pins) > maxBackupFiles || total > maxBackupBytes {
		return nil, "", store.Refuse("query_result_limit_exceeded")
	}
	// A fixed directory refuses concurrent exports and orphan pins after crash.
	dir := filepath.Join(s.byteDirectory(), ".backup-pins")
	err = os.Mkdir(dir, 0700)
	if err != nil {
		return nil, "", store.Refuse("store_unavailable")
	}
	for _, pin := range pins {
		if err = ctx.Err(); err != nil {
			return nil, dir, err
		}
		if pin.size == 0 {
			continue
		} // a zero-byte import need not have a file yet
		source := filepath.Join(s.byteDirectory(), pin.name)
		info, e := os.Lstat(source)
		if e != nil || !info.Mode().IsRegular() || info.Size() < pin.size {
			return nil, dir, store.Refuse("store_unavailable")
		}
		if err = os.Link(source, filepath.Join(dir, pin.name)); err != nil {
			return nil, dir, store.Refuse("store_unavailable")
		}
	}
	return pins, dir, nil
}
