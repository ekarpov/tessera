package sqlengine

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/ekarpov/tessera/internal/recordstate"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

func (s *Store) authorizeScopedUpload(ctx context.Context, tx *sql.Tx, a store.Access, dataset, raw string) error {
	var target p.AttachmentTarget
	if json.Unmarshal([]byte(raw), &target) != nil || target.Validate() != nil {
		return store.Refuse("invalid_field")
	}
	h, err := s.load(ctx, tx, a, dataset, false)
	if err != nil {
		return err
	}
	engine, err := recordstate.Adopt(h.image, h.limits, a, h.auth, s.reads)
	if err != nil {
		return err
	}
	if target.ObjectID != "" {
		engine.SetObjectSource(func(id string) (recordstate.Row, bool, error) {
			return fetchRow(ctx, tx, h.managed.source, dataset, id)
		})
	}
	return engine.AuthorizeAttachmentTarget(ctx, a, dataset, target)
}

func (s *Store) recheckScopedTransfer(ctx context.Context, tx *sql.Tx, storeID string, a store.Access, target string, serialize bool) error {
	if serialize {
		var dataset string
		if err := tx.QueryRowContext(ctx, `SELECT "dataset_id" FROM "tessera_content_stores" WHERE "id"=$1`, storeID).Scan(&dataset); err != nil {
			return dbError(ctx, err)
		}
		if err := s.serializeWrites(ctx, tx, dataset); err != nil {
			return err
		}
	}
	_, dataset, err := s.contentAuthority(ctx, tx, a, storeID)
	if err != nil {
		return err
	}
	return s.authorizeScopedUpload(ctx, tx, a, dataset, target)
}

// Contribution erasure also owns reservations and unlinked items from scoped
// uploads. Attached items are handled with their records and surviving links.
func eraseScopedUploads(ctx context.Context, tx *sql.Tx, dataset, principal string) error {
	scope := `"principal"=$1 AND "attachment_target" IS NOT NULL AND "store_id" IN
		(SELECT "id" FROM "tessera_content_stores" WHERE "dataset_id"=$2)`
	if _, err := tx.ExecContext(ctx, `UPDATE "tessera_imports" SET "state"='aborted'
		WHERE "state"='open' AND `+scope, principal, dataset); err != nil {
		return dbError(ctx, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE "tessera_items" SET "deleted"=1 WHERE "deleted"=0
		AND "upload_id" IN (SELECT "upload_id" FROM "tessera_imports" WHERE `+scope+`)
		AND NOT EXISTS (SELECT 1 FROM "tessera_attachments" a
			WHERE a."store_id"="tessera_items"."store_id" AND a."item_id"="tessera_items"."item_id")`,
		principal, dataset); err != nil {
		return dbError(ctx, err)
	}
	return nil
}
