package sqlengine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/ekarpov/tessera/pkg/store"
)

func (s *Store) ContentTransfer(ctx context.Context, token, method, rangeHeader string, body io.Reader) (store.ContentRead, error) {
	var result store.ContentRead
	if s.contentDir == "" {
		return result, store.Refuse("not_supported")
	}
	if len(token) != 43 {
		return result, store.Refuse("authentication_failed")
	}
	if err := s.enter(); err != nil {
		return result, err
	}
	defer s.life.RUnlock()
	digest := sha256.Sum256([]byte(token))
	var storeID, resource, allowed, representation string
	var expires int64
	var active int
	err := s.reader.QueryRowContext(ctx, `SELECT t."store_id",t."resource_id",t."method",t."expires_at",t."representation",s."active" FROM "tessera_transfer_tokens" t JOIN "tessera_content_stores" s ON s."id"=t."store_id" WHERE t."digest"=$1`, digest[:]).Scan(&storeID, &resource, &allowed, &expires, &representation, &active)
	if err == sql.ErrNoRows {
		return result, store.Refuse("authentication_failed")
	}
	if err != nil {
		return result, dbError(ctx, err)
	}
	if allowed != method {
		return result, store.Refuse("permission_denied")
	}
	if active != 1 || time.Now().UnixMilli() >= expires {
		return result, store.Refuse("expired")
	}
	if method == "GET" {
		// Serialize filesystem checkpoints independently of SQL transactions.
		// No transfer bytes flow while a record transaction is open. A read
		// hands back an open file and the bytes leave after this returns, so
		// the lock covers deciding what to send rather than the sending.
		unlockTransfer, lockErr := s.lockTransfers(ctx)
		if lockErr != nil {
			return result, lockErr
		}
		locked := true
		unlock := func() {
			if locked {
				locked = false
				unlockTransfer()
			}
		}
		defer unlock()
		var upload string
		err = s.reader.QueryRowContext(ctx, `SELECT "upload_id","content_type","byte_count" FROM "tessera_items" WHERE "store_id"=$1 AND "item_id"=$2 AND "deleted"=0`, storeID, resource).Scan(&upload, &result.ContentType, &result.Size)
		if err == sql.ErrNoRows {
			return result, store.Refuse("not_found")
		}
		if err != nil {
			return result, dbError(ctx, err)
		}
		// A thumbnail is produced here rather than stored, so the original
		// never reaches the client and no derivative outlives the item.
		if size, wanted := parseThumbnailRepresentation(representation); wanted {
			if result.Size == 0 || !thumbnailable(result.ContentType) {
				return result, store.Refuse("unsupported_representation")
			}
			original, e := os.Open(filepath.Join(s.contentDir, upload))
			if e != nil {
				return result, store.Refuse("store_unavailable")
			}
			defer original.Close()
			// The file is open, which keeps its bytes whatever cleanup does
			// next, so the lock is let go before the image work. Decoding,
			// resizing and encoding a large image under it held every other
			// transfer on the server — uploads and downloads on every dataset
			// — for as long as the image took, past the caller's own deadline.
			// Renditions have a bound of their own instead.
			unlock()
			select {
			case s.renditions <- struct{}{}:
			case <-ctx.Done():
				return result, ctx.Err()
			}
			defer func() { <-s.renditions }()
			if err = ctx.Err(); err != nil {
				return result, err
			}
			rendition, contentType, e := thumbnail(original, size)
			if e != nil {
				return result, e
			}
			result.ContentType, result.Size = contentType, int64(len(rendition))
			result.Body = io.NopCloser(bytes.NewReader(rendition))
			return result, nil
		}
		if result.Size == 0 {
			result.Body = io.NopCloser(bytes.NewReader(nil))
			return result, nil
		}
		result.Body, err = os.Open(filepath.Join(s.contentDir, upload))
		if err != nil {
			return result, store.Refuse("store_unavailable")
		}
		return result, nil
	}
	if method != "PATCH" {
		return result, store.Refuse("permission_denied")
	}
	start, end, total, err := parseContentRange(rangeHeader)
	if err != nil {
		return result, err
	}
	count := end - start + 1
	if count > maxChunkBytes {
		return result, store.Refuse("limit_exceeded")
	}
	// The chunk is read from the network before the transfer lock is taken.
	// Reading it under the lock meant one client sending its bytes slowly held
	// a process-wide mutex for as long as it chose to take, and every other
	// transfer on the server — every dataset, uploads and downloads alike —
	// waited behind it. The read is bounded by the chunk ceiling checked
	// above, and it happens after the token is validated, so an unauthenticated
	// caller still cannot make this buffer anything.
	data, err := io.ReadAll(io.LimitReader(body, count+1))
	if err != nil || int64(len(data)) != count {
		return result, store.Refuse("invalid_field")
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	unlockTransfer, lockErr := s.lockTransfers(ctx)
	if lockErr != nil {
		return result, lockErr
	}
	defer unlockTransfer()
	var state string
	var received, maximum, updated int64
	var established sql.NullInt64
	err = s.reader.QueryRowContext(ctx, `SELECT "state","received_bytes","maximum_bytes","total_bytes","updated_at" FROM "tessera_imports" WHERE "store_id"=$1 AND "upload_id"=$2`, storeID, resource).Scan(&state, &received, &maximum, &established, &updated)
	if err == sql.ErrNoRows {
		return result, store.Refuse("not_found")
	}
	if err != nil {
		return result, dbError(ctx, err)
	}
	if state != "open" || time.Now().UnixMilli()-updated > contentIdle*1000 {
		return result, store.Refuse("expired")
	}
	if established.Valid && total.Valid && established.Int64 != total.Int64 {
		return result, store.Refuse("conflict")
	}
	if total.Valid && total.Int64 <= end {
		return result, store.Refuse("conflict")
	}
	if end >= maximum || end >= maxItemBytes || total.Valid && (total.Int64 > maximum || total.Int64 > maxItemBytes) {
		return result, store.Refuse("limit_exceeded")
	}
	completes := total.Valid && end+1 == total.Int64
	if count < minChunkBytes && !completes {
		return result, store.Refuse("limit_exceeded")
	}
	if start > received || start < received && end >= received {
		return result, store.Refuse("conflict")
	}
	path := filepath.Join(s.contentDir, resource)
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return result, store.Refuse("store_unavailable")
	}
	defer file.Close()
	// A crashed transfer may have durable tail bytes beyond its SQL checkpoint.
	// They are uncommitted and cannot be exposed or counted on resume.
	if err = file.Truncate(received); err != nil {
		return result, store.Refuse("store_unavailable")
	}
	newReceived := received
	if start < received {
		old := make([]byte, len(data))
		if _, err = file.ReadAt(old, start); err != nil || !bytes.Equal(old, data) {
			return result, store.Refuse("conflict")
		}
	} else {
		if _, err = file.WriteAt(data, start); err != nil {
			return result, store.Refuse("store_unavailable")
		}
		if err = file.Sync(); err != nil {
			return result, store.Refuse("store_unavailable")
		}
		newReceived = end + 1
	}
	if err = file.Sync(); err != nil {
		return result, store.Refuse("store_unavailable")
	}
	if err = syncDirectory(s.contentDir); err != nil {
		return result, store.Refuse("store_unavailable")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, dbError(ctx, err)
	}
	defer tx.Rollback()
	// Re-check retirement after filesystem I/O. An erase may have run while bytes
	// arrived; its persistent deletion queue owns this path in that case.
	var stillActive int
	if err = tx.QueryRowContext(ctx, `SELECT "active" FROM "tessera_content_stores" WHERE "id"=$1`, storeID).Scan(&stillActive); err != nil {
		return result, dbError(ctx, err)
	}
	if stillActive != 1 {
		return result, store.Refuse("expired")
	}
	finalTotal := established
	if !finalTotal.Valid && total.Valid && completes {
		finalTotal = total
	}
	updatedRow, err := tx.ExecContext(ctx, `UPDATE "tessera_imports" SET "received_bytes"=$1,"total_bytes"=$2,"updated_at"=$3 WHERE "upload_id"=$4 AND "state"='open' AND "received_bytes"=$5`, newReceived, finalTotal, time.Now().UnixMilli(), resource, received)
	if err != nil {
		return result, dbError(ctx, err)
	}
	n, err := updatedRow.RowsAffected()
	if err != nil || n != 1 {
		return result, store.Refuse("conflict")
	}
	if err = tx.Commit(); err != nil {
		return result, dbError(ctx, err)
	}
	return result, nil
}
func parseContentRange(value string) (start, end int64, total sql.NullInt64, err error) {
	if !strings.HasPrefix(value, "bytes ") {
		err = store.Refuse("invalid_field")
		return
	}
	interval, size, ok := strings.Cut(strings.TrimPrefix(value, "bytes "), "/")
	if !ok {
		err = store.Refuse("invalid_field")
		return
	}
	first, last, ok := strings.Cut(interval, "-")
	if !ok {
		err = store.Refuse("invalid_field")
		return
	}
	start, err = strconv.ParseInt(first, 10, 64)
	if err != nil || start < 0 {
		err = store.Refuse("invalid_field")
		return
	}
	end, err = strconv.ParseInt(last, 10, 64)
	if err != nil || end < start || end >= maxItemBytes {
		err = store.Refuse("limit_exceeded")
		return
	}
	if fmt.Sprintf("%d-%d", start, end) != interval {
		err = store.Refuse("invalid_field")
		return
	}
	if size != "*" {
		total.Int64, err = strconv.ParseInt(size, 10, 64)
		total.Valid = true
		if err != nil || total.Int64 < 1 || strconv.FormatInt(total.Int64, 10) != size {
			err = store.Refuse("invalid_field")
			return
		}
	}
	return
}

// renditionSlots is how many thumbnails may be drawn at once: half the
// processors, at least one, so image work cannot take the whole machine.
func renditionSlots() int {
	return max(1, runtime.NumCPU()/2)
}
