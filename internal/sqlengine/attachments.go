package sqlengine

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/ekarpov/tessera/internal/recordstate"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

func (s *Store) authorizeAttachment(ctx context.Context, tx *sql.Tx, a store.Access, dataset, reference string, target p.AttachmentTarget) error {
	storeID, itemID, err := p.ParseContentReference(reference)
	if err != nil {
		return err
	}
	auth, owning, err := s.contentAuthority(ctx, tx, a, storeID)
	if err != nil {
		return err
	}
	if owning != dataset {
		return store.Refuse("permission_denied")
	}
	var one int
	owner := auth.Grants.Write && auth.View.Full()
	if owner {
		err = tx.QueryRowContext(ctx, `SELECT 1 FROM "tessera_items" WHERE "store_id"=$1 AND "item_id"=$2 AND "deleted"=0`, storeID, itemID).Scan(&one)
		if err == sql.ErrNoRows {
			return store.Refuse("unavailable")
		}
	} else {
		if target.Validate() != nil ||
			target.ObjectID == "" && (!auth.Grants.Insert || !auth.View.AllowsCollectionInsert(target.TypeID, target.CollectionID)) {
			return store.Refuse("permission_denied")
		}
		var raw string
		err = tx.QueryRowContext(ctx, `SELECT u."attachment_target" FROM "tessera_items" i
		  JOIN "tessera_imports" u ON u."upload_id"=i."upload_id" AND u."store_id"=i."store_id"
		  WHERE i."store_id"=$1 AND i."item_id"=$2 AND i."deleted"=0
		    AND u."principal"=$3 AND u."authorization"=$4 AND u."attachment_target" IS NOT NULL`,
			storeID, itemID, a.Principal, a.Authorization).Scan(&raw)
		if err == sql.ErrNoRows {
			return store.Refuse("permission_denied")
		}
		if err == nil {
			var actual p.AttachmentTarget
			if json.Unmarshal([]byte(raw), &actual) != nil || actual != target {
				return store.Refuse("permission_denied")
			}
		}
	}
	if err != nil {
		return dbError(ctx, err)
	}
	return nil
}

// attachment is one association: the record and field that refer to an item,
// and the item referred to. It is comparable, so two of them can be compared
// to decide whether anything about the association changed.
type attachment struct{ typeID, storeID, itemID string }

// attachments is the associations an image implies, keyed by the record and
// field holding each one — which is the association table's own identity.
func attachments(image recordstate.Image) (map[[2]string]attachment, error) {
	found := map[[2]string]attachment{}
	for id, row := range image.Objects {
		for field, value := range row.Fields {
			if value.Type() != p.MediaType {
				continue
			}
			storeID, itemID, err := p.ParseContentReference(value.Payload().(string))
			if err != nil {
				return nil, err
			}
			found[[2]string{id, field}] = attachment{typeID: row.TypeID, storeID: storeID, itemID: itemID}
		}
	}
	return found, nil
}

// persistAttachments writes the difference between two images.
//
// It used to delete every association the dataset had and insert them all
// again, on any change to anything. Changing an unrelated integer field
// therefore rewrote every media association in the dataset — a delete and a
// row per attachment, inside the write transaction, holding the dataset's
// lock for the duration. A record with no attachments cost the attachments of
// every record beside it.
//
// The difference is taken over both images rather than read back from the
// table, because both are already in hand here and the table is not.
func (s *Store) persistAttachments(ctx context.Context, tx *sql.Tx, before, after recordstate.Image) error {
	was, err := attachments(before)
	if err != nil {
		// An association already stored is not this transaction's to refuse.
		// Treating the prior image as empty would delete nothing and insert
		// what is wanted, which is the safe reading of an unreadable past.
		was = map[[2]string]attachment{}
	}
	now, err := attachments(after)
	if err != nil {
		return err
	}
	// Removals first, so an association whose item changed does not collide
	// with itself on the way back in.
	for key, old := range was {
		if current, ok := now[key]; ok && current == old {
			continue
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM "tessera_attachments" WHERE "dataset_id"=$1 AND "object_id"=$2 AND "field_id"=$3`, after.ID, key[0], key[1]); err != nil {
			return dbError(ctx, err)
		}
	}
	for key, current := range now {
		if old, ok := was[key]; ok && old == current {
			continue
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO "tessera_attachments" ("dataset_id","object_id","type_id","field_id","store_id","item_id") VALUES ($1,$2,$3,$4,$5,$6)`, after.ID, key[0], current.typeID, key[1], current.storeID, current.itemID); err != nil {
			return dbError(ctx, err)
		}
	}
	return nil
}

// CleanupContent retries deletion and reclaims idle staging. Private files are
// named only with server-generated upload IDs, never caller-provided paths.
func (s *Store) CleanupContent(ctx context.Context) error {
	directory := s.byteDirectory()
	if directory == "" {
		return nil
	}
	if err := s.enter(); err != nil {
		return err
	}
	defer s.life.RUnlock()
	unlock, err := s.lockTransfers(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := s.db.ExecContext(ctx, `UPDATE "tessera_imports" SET "state"='aborted' WHERE "state"='open' AND "updated_at"<$1`, time.Now().Add(-contentIdle*time.Second).UnixMilli()); err != nil {
		return dbError(ctx, err)
	}
	// Only what has not been cleaned yet, and a bounded batch of it: the
	// transfer lock is held throughout, and what this pass leaves the next
	// one takes.
	rows, err := s.reader.QueryContext(ctx, `SELECT "upload_id" FROM "tessera_imports" WHERE "state"='aborted' AND "cleaned"=0 UNION SELECT "upload_id" FROM "tessera_items" WHERE "deleted"=1 AND "cleaned"=0 LIMIT $1`, cleanupBatch)
	if err != nil {
		return dbError(ctx, err)
	}
	ids := []string{}
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
		return dbError(ctx, err)
	}
	for _, id := range ids {
		if err = os.Remove(filepath.Join(directory, id)); err != nil && !os.IsNotExist(err) {
			return store.Refuse("store_unavailable")
		}
		if _, err = s.db.ExecContext(ctx, `UPDATE "tessera_items" SET "cleaned"=1 WHERE "upload_id"=$1 AND "deleted"=1`, id); err != nil {
			return dbError(ctx, err)
		}
		if _, err = s.db.ExecContext(ctx, `UPDATE "tessera_imports" SET "cleaned"=1 WHERE "upload_id"=$1 AND "state"='aborted'`, id); err != nil {
			return dbError(ctx, err)
		}
	}
	if _, err = s.db.ExecContext(ctx, `DELETE FROM "tessera_transfer_tokens" WHERE "expires_at"<$1`, time.Now().UnixMilli()); err != nil {
		return dbError(ctx, err)
	}
	return nil
}

// cleanupBatch bounds how much one maintenance pass removes while it holds
// the transfer lock. A variable only so a test can make it small.
var cleanupBatch = 500
