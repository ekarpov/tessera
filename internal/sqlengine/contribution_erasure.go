package sqlengine

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/ekarpov/tessera/pkg/store"
)

const contributionFencePrefix = "contribution:"

func splitContributionFence(value string) (dataset, job string, ok bool) {
	tail, found := strings.CutPrefix(value, contributionFencePrefix)
	if !found {
		return "", "", false
	}
	dataset, job, _ = strings.Cut(tail, ":")
	return dataset, job, true
}

// BeginContributionErasure is a trusted host operation. It durably revokes
// the principal's grants in the dataset and returns a stable job identity.
func (s *Store) BeginContributionErasure(ctx context.Context, dataset, principal string) (string, error) {
	if principal == "" {
		return "", store.Refuse("invalid_field")
	}
	if err := s.enter(); err != nil {
		return "", err
	}
	defer s.life.RUnlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", dbError(ctx, err)
	}
	defer tx.Rollback()
	if err = s.serializeWrites(ctx, tx, dataset); err != nil {
		return "", err
	}
	creator, err := contributionCreator(ctx, tx, dataset, principal)
	if err != nil {
		return "", err
	}
	job, err := cursorToken()
	if err != nil {
		return "", err
	}
	started, err := tx.ExecContext(ctx, `INSERT INTO "tessera_contribution_erasure" ("dataset_id","creator_id","job_id")
		VALUES ($1,$2,$3) ON CONFLICT ("dataset_id","creator_id") DO NOTHING`, dataset, creator, job)
	if err != nil {
		return "", dbError(ctx, err)
	}
	added, err := started.RowsAffected()
	if err != nil {
		return "", dbError(ctx, err)
	}
	if err = tx.QueryRowContext(ctx, `SELECT "job_id" FROM "tessera_contribution_erasure" WHERE "dataset_id"=$1 AND "creator_id"=$2`, dataset, creator).Scan(&job); err != nil {
		return "", dbError(ctx, err)
	}
	for _, table := range []string{`"tessera_access"`, `"tessera_receipts"`} {
		if _, err = tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE "dataset_id"=$1 AND "principal"=$2`, dataset, principal); err != nil {
			return "", dbError(ctx, err)
		}
	}
	// A snapshot can still hold records already removed by ordinary writes.
	// Invalidate it even when the contribution index has nothing left to step.
	if added != 0 {
		if err = invalidateContributionPages(ctx, tx, dataset); err != nil {
			return "", err
		}
	}
	if err = eraseScopedUploads(ctx, tx, dataset, principal); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", dbError(ctx, err)
	}
	if err = s.journalRetirements(ctx); err != nil {
		return "", err
	}
	return job, nil
}

// StepContributionErasure deletes at most limit records atomically. Calls
// resume from the remaining index rows and are safe to repeat after a loss.
func (s *Store) StepContributionErasure(ctx context.Context, dataset, job string, limit int) (bool, error) {
	if job == "" || limit < 1 || limit > maxContributionBatch {
		return false, store.Refuse("invalid_field")
	}
	if err := s.enter(); err != nil {
		return false, err
	}
	defer s.life.RUnlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, dbError(ctx, err)
	}
	defer tx.Rollback()
	if err = s.serializeWrites(ctx, tx, dataset); err != nil {
		return false, err
	}
	var creator string
	var done int
	err = tx.QueryRowContext(ctx, `SELECT "creator_id","done" FROM "tessera_contribution_erasure" WHERE "dataset_id"=$1 AND "job_id"=$2`, dataset, job).Scan(&creator, &done)
	if err == sql.ErrNoRows {
		return false, store.Refuse("not_found")
	}
	if err != nil || done != 0 {
		if err != nil {
			return false, dbError(ctx, err)
		}
		return true, nil
	}
	keys, err := contributionKeys(ctx, tx, dataset, creator, "", limit+1)
	if err != nil {
		return false, err
	}
	if err = eraseContributionRecords(ctx, tx, dataset, keys[:min(len(keys), limit)]); err != nil {
		return false, err
	}
	// An ordinary delete can remove the final indexed row between batches
	// while leaving a snapshot opened since the previous batch readable.
	if err = invalidateContributionPages(ctx, tx, dataset); err != nil {
		return false, err
	}
	finished := len(keys) <= limit
	if finished {
		if _, err = tx.ExecContext(ctx, `UPDATE "tessera_contribution_erasure" SET "done"=1 WHERE "job_id"=$1`, job); err != nil {
			return false, dbError(ctx, err)
		}
	}
	if err = tx.Commit(); err != nil {
		return false, dbError(ctx, err)
	}
	return finished, nil
}

func eraseContributionRecords(ctx context.Context, tx *sql.Tx, dataset string, keys []store.ObjectKey) error {
	if len(keys) == 0 {
		return nil
	}
	var totalBytes int64
	for _, key := range keys {
		// Mark items orphaned by this deletion. A reference from a surviving
		// record keeps the item live, even if it was linked multiple times.
		if _, err := tx.ExecContext(ctx, `UPDATE "tessera_items" SET "deleted"=1 WHERE "deleted"=0
			AND "store_id" IN (SELECT "id" FROM "tessera_content_stores" WHERE "dataset_id"=$1)
			AND ("store_id","item_id") IN (SELECT a."store_id",a."item_id" FROM "tessera_attachments" a
				WHERE a."dataset_id"=$1 AND a."object_id"=$2)
			AND NOT EXISTS (SELECT 1 FROM "tessera_attachments" a WHERE a."store_id"="tessera_items"."store_id"
				AND a."item_id"="tessera_items"."item_id" AND (a."dataset_id"<>$1 OR a."object_id"<>$2))`,
			dataset, key.ObjectID); err != nil {
			return dbError(ctx, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM "tessera_attachments" WHERE "dataset_id"=$1 AND "object_id"=$2`, dataset, key.ObjectID); err != nil {
			return dbError(ctx, err)
		}
		var bytes int64
		if err := tx.QueryRowContext(ctx, `DELETE FROM "tessera_objects" WHERE "dataset_id"=$1 AND "object_id"=$2 RETURNING "wire_bytes"`,
			dataset, key.ObjectID).Scan(&bytes); err != nil {
			return dbError(ctx, err)
		}
		totalBytes += bytes
	}
	watermark, err := cursorToken()
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE "tessera_dataset" SET "watermark"=$1,"object_count"="object_count"-$2,"object_bytes"="object_bytes"-$3 WHERE "dataset_id"=$4`,
		"w:"+watermark, len(keys), totalBytes, dataset); err != nil {
		return dbError(ctx, err)
	}
	return nil
}

func invalidateContributionPages(ctx context.Context, tx *sql.Tx, dataset string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM "tessera_sequences" WHERE "dataset_id"=$1`, dataset); err != nil {
		return dbError(ctx, err)
	}
	return nil
}

// A backup that predates a contribution erasure cannot safely restore the
// erased bytes. Keep the fence independent and require a post-erasure archive.
func checkContributionArchive(manifest backupManifest, fence []byte, erasedDatasets []string) error {
	live := map[string]bool{}
	done := map[string]bool{}
	for _, table := range manifest.Tables {
		switch table.Name {
		case "tessera_dataset", "tessera_contribution_erasure":
		default:
			continue
		}
		for _, row := range table.Rows {
			if len(row) != len(table.Columns) {
				return store.Refuse("invalid_field")
			}
			var dataset, job string
			var completed bool
			for i, col := range table.Columns {
				switch col {
				case "dataset_id":
					if row[i].Text != nil {
						dataset = *row[i].Text
					}
				case "job_id":
					if row[i].Text != nil {
						job = *row[i].Text
					}
				case "done":
					completed = row[i].Integer != nil && *row[i].Integer == 1
				}
			}
			if table.Name == "tessera_dataset" {
				live[dataset] = true
			} else if completed {
				done[contributionFencePrefix+dataset+":"+job] = true
			}
		}
	}
	// Whole-dataset retirement removes all contributions during restore.
	for _, dataset := range erasedDatasets {
		delete(live, dataset)
	}
	for _, line := range strings.Split(string(fence), "\n") {
		if line == "" {
			continue
		}
		var id string
		if json.Unmarshal([]byte(line), &id) != nil {
			return store.Refuse("dataset_unavailable")
		}
		if dataset, job, ok := splitContributionFence(id); ok {
			if dataset == "" || job == "" || (live[dataset] && !done[id]) {
				return store.Refuse("dataset_unavailable")
			}
		}
	}
	return nil
}
