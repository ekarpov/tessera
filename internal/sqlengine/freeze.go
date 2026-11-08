package sqlengine

import (
	"context"
	"database/sql"
	"strconv"

	"github.com/ekarpov/tessera/pkg/store"
)

// ReaderCut captures the next mint in the dataset's own serialized sequence.
// A later write can proceed before grants are installed, but its identities
// are already beyond the cut and cannot enter the frozen view.
func (s *Store) ReaderCut(ctx context.Context, dataset string) (uint64, error) {
	if err := s.enter(); err != nil {
		return 0, err
	}
	defer s.life.RUnlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, dbError(ctx, err)
	}
	defer tx.Rollback()
	if err = s.serializeWrites(ctx, tx, dataset); err != nil {
		return 0, err
	}
	var next string
	if err = tx.QueryRowContext(ctx, `SELECT "next_id" FROM "tessera_dataset" WHERE "dataset_id"=$1`, dataset).Scan(&next); err == sql.ErrNoRows {
		return 0, store.Refuse("not_found")
	} else if err != nil {
		return 0, dbError(ctx, err)
	}
	cut, err := strconv.ParseUint(next, 10, 64)
	if err != nil || cut == 0 {
		return 0, store.Refuse("internal_error")
	}
	if err = tx.Commit(); err != nil {
		return 0, dbError(ctx, err)
	}
	return cut, nil
}
