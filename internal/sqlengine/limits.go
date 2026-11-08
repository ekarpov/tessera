package sqlengine

import (
	"context"
	"database/sql"
	"encoding/json"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// creationDefaults fills what an opener left unset, so the defaults are always
// limits a dataset could be created with.
func creationDefaults(limits p.Limits) p.Limits {
	if limits.MaxMaterializedBytes <= 0 {
		limits.MaxMaterializedBytes = 64 << 20
	}
	if limits.MaxAggregateGroups <= 0 {
		limits.MaxAggregateGroups = p.DefaultAggregateGroups
	}
	return limits
}

// DatasetLimits reads one dataset's stored limits, for an operator changing
// them. It takes no access: it is composition, like Provisioned.
func (s *Store) DatasetLimits(ctx context.Context, dataset string) (p.Limits, error) {
	if err := s.enter(); err != nil {
		return p.Limits{}, err
	}
	defer s.life.RUnlock()
	var raw []byte
	if err := s.reader.QueryRowContext(ctx, `SELECT "limits" FROM "tessera_dataset" WHERE "dataset_id"=$1`, dataset).Scan(&raw); err != nil {
		return p.Limits{}, store.Refuse("not_found")
	}
	var limits p.Limits
	if json.Unmarshal(raw, &limits) != nil {
		return p.Limits{}, store.Refuse("internal_error")
	}
	return limits, nil
}

// SetDatasetLimits replaces one dataset's limits.
//
// This is what makes 507 dataset_full an answer an operator can act on: the
// bound a dataset outgrew is stored on it at creation, and without this there
// was no way to raise it short of recreating the dataset.
//
// Every bound may move either way except the retention window, which may only
// grow. A client was promised it and reconciles against it; shortening it
// would void outcomes clients are still entitled to ask about.
func (s *Store) SetDatasetLimits(ctx context.Context, dataset string, limits p.Limits) error {
	if err := limits.Validate(); err != nil {
		return err
	}
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
	var raw []byte
	if err = tx.QueryRowContext(ctx, `SELECT "limits" FROM "tessera_dataset" WHERE "dataset_id"=$1`, dataset).Scan(&raw); err != nil {
		return store.Refuse("not_found")
	}
	var current p.Limits
	if json.Unmarshal(raw, &current) != nil {
		return store.Refuse("internal_error")
	}
	if limits.IdempotencyRetentionSeconds < current.IdempotencyRetentionSeconds {
		return store.Refuse("conflict")
	}
	encoded, err := json.Marshal(limits)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE "tessera_dataset" SET "limits"=$1 WHERE "dataset_id"=$2`, encoded, dataset); err != nil {
		return dbError(ctx, err)
	}
	if err = tx.Commit(); err != nil {
		return dbError(ctx, err)
	}
	return s.refreshBounds(ctx)
}

// refreshBounds works out the storage's declared bound again from what is
// committed, and publishes it. How much of a request body is read before its
// dataset is known is the widest max_request_bytes any dataset declares, so a
// change to any dataset's limits, or a new dataset, can move it.
//
// It is read after the change commits, and reading and publishing happen
// under one lock. Worked out inside each change's own transaction and
// published after it, two changes to different datasets committing at once
// could each miss the other's, and whichever published last left the bound
// short of a limit that had committed — refusing bodies it should accept.
func (s *Store) refreshBounds(ctx context.Context) error {
	s.boundsRefresh.Lock()
	defer s.boundsRefresh.Unlock()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return dbError(ctx, err)
	}
	defer tx.Rollback()
	bounds, err := storageLimits(ctx, tx)
	if err != nil {
		return err
	}
	s.setBounds(bounds)
	return nil
}

// DatasetUsage measures one dataset. Composition only, like DatasetLimits.
func (s *Store) DatasetUsage(ctx context.Context, dataset string) (store.Usage, error) {
	limits, err := s.DatasetLimits(ctx, dataset)
	if err != nil {
		return store.Usage{}, err
	}
	if err = s.enter(); err != nil {
		return store.Usage{}, err
	}
	defer s.life.RUnlock()
	var usage store.Usage
	if err = s.reader.QueryRowContext(ctx, `SELECT "object_count","object_bytes" FROM "tessera_dataset" WHERE "dataset_id"=$1`, dataset).Scan(&usage.Records, &usage.Bytes); err != nil {
		return store.Usage{}, dbError(ctx, err)
	}
	var types int64
	if err = s.reader.QueryRowContext(ctx, `SELECT coalesce(sum(length("body")),0) FROM "tessera_types" WHERE "dataset_id"=$1`, dataset).Scan(&types); err != nil {
		return store.Usage{}, dbError(ctx, err)
	}
	usage.Bytes += types
	if limits.MaxStoredBytes > 0 {
		quota := int64(limits.MaxStoredBytes)
		usage.MaxBytes = &quota
	}
	return usage, nil
}

// RenameDataset changes a dataset's display name. A name is a label for
// people and says nothing about identity, which is why this is allowed at all:
// the dataset identity a client holds is unchanged.
func (s *Store) RenameDataset(ctx context.Context, dataset, displayName string) error {
	if err := ValidateDisplayName(displayName); err != nil {
		return err
	}
	if err := s.enter(); err != nil {
		return err
	}
	defer s.life.RUnlock()
	result, err := s.db.ExecContext(ctx, `UPDATE "tessera_dataset" SET "display_name"=$1 WHERE "dataset_id"=$2`, displayName, dataset)
	if err != nil {
		return dbError(ctx, err)
	}
	if n, e := result.RowsAffected(); e == nil && n == 0 {
		return store.Refuse("not_found")
	}
	return nil
}
