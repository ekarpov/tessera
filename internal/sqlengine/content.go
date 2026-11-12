package sqlengine

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"github.com/ekarpov/tessera/internal/recordstate"
	"mime"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

const maxItemBytes int64 = 64 << 20
const maxChunkBytes int64 = 1 << 20
const minChunkBytes int64 = 256 << 10
const contentIdle = 600
const uriLifetime = 300

// EnableContent binds a private filesystem byte store to the SQL authority.
// Call during composition, before publishing the store to concurrent callers.
func (s *Store) EnableContent(ctx context.Context, directory string) error {
	directory, err := filepath.Abs(directory)
	if err != nil {
		return store.Refuse("store_unavailable")
	}
	if err = os.MkdirAll(directory, 0700); err != nil {
		return store.Refuse("store_unavailable")
	}
	if info, e := os.Stat(directory); e != nil || info.Mode().Perm()&0077 != 0 {
		return store.Refuse("store_unavailable")
	}
	if err = s.enter(); err != nil {
		return err
	}
	defer s.life.RUnlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return dbError(ctx, err)
	}
	defer tx.Rollback()
	if err = provisionContent(ctx, tx); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return dbError(ctx, err)
	}
	s.contentDir = directory
	return nil
}

// provisionContent gives every dataset without an active content store one of
// its own. A store belongs to exactly one dataset for the life of that dataset,
// so each gets a fresh identity rather than sharing one; the byte directory
// underneath may be shared, because items are isolated by store.
func provisionContent(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT d."dataset_id" FROM "tessera_dataset" d WHERE NOT EXISTS (SELECT 1 FROM "tessera_content_stores" c WHERE c."dataset_id"=d."dataset_id" AND c."active"=1) ORDER BY d."dataset_id"`)
	if err != nil {
		return dbError(ctx, err)
	}
	var pending []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		pending = append(pending, id)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return dbError(ctx, err)
	}
	for _, dataset := range pending {
		id, e := cursorToken()
		if e != nil {
			return e
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO "tessera_content_stores" ("id","dataset_id","active") VALUES ($1,$2,1)`, "c-"+id, dataset); err != nil {
			return dbError(ctx, err)
		}
	}
	return nil
}
func (s *Store) contentAuthority(ctx context.Context, tx *sql.Tx, a store.Access, id string) (store.Authorization, string, error) {
	var dataset string
	var restrictedPrincipal, restrictedAuthorization sql.NullString
	var active int
	err := tx.QueryRowContext(ctx, `SELECT "dataset_id","active","principal","authorization" FROM "tessera_content_stores" WHERE "id"=$1`, id).Scan(&dataset, &active, &restrictedPrincipal, &restrictedAuthorization)
	if err == sql.ErrNoRows {
		return store.Authorization{}, "", store.Refuse("unavailable")
	}
	if err != nil {
		return store.Authorization{}, "", dbError(ctx, err)
	}
	// A store whose dataset has been retired reveals nothing: erasure mints a
	// new identity and the old one names nothing at all.
	var live int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM "tessera_dataset" WHERE "dataset_id"=$1`, dataset).Scan(&live); err != nil {
		return store.Authorization{}, "", dbError(ctx, err)
	}
	if live == 0 {
		return store.Authorization{}, "", store.Refuse("unavailable")
	}
	if restrictedPrincipal.Valid && (restrictedPrincipal.String != a.Principal || restrictedAuthorization.String != a.Authorization) {
		return store.Authorization{}, "", store.Refuse("unavailable")
	}
	auth, err := authorization(ctx, tx, dataset, a)
	if err != nil {
		// A store on a dataset outside the caller's authorized view is not
		// theirs to be refused from: that answer is the one the three-answer
		// rule reserves for a store they may see and may not use. Refusing
		// told them the identifier named something, which is the difference
		// between an identifier that names nothing and one that names
		// somebody else's — and it is the whole content of the answer.
		if p.ErrorCode(err) == "permission_denied" {
			return store.Authorization{}, "", store.Refuse("unavailable")
		}
		return auth, "", err
	}
	if active == 0 {
		if !auth.View.Full() {
			return auth, dataset, store.Refuse("unavailable")
		}
		return auth, dataset, store.Refuse("not_found")
	}
	return auth, dataset, nil
}
func (s *Store) issueTransfer(ctx context.Context, tx *sql.Tx, storeID, resource, method string, representation ...string) (map[string]any, error) {
	rendition := ""
	if len(representation) == 1 {
		rendition = representation[0]
	}
	token, err := cursorToken()
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(token))
	expires := time.Now().Add(uriLifetime * time.Second)
	if _, err = tx.ExecContext(ctx, `INSERT INTO "tessera_transfer_tokens" ("digest","store_id","resource_id","method","expires_at","representation") VALUES ($1,$2,$3,$4,$5,$6)`, digest[:], storeID, resource, method, expires.UnixMilli(), rendition); err != nil {
		return nil, dbError(ctx, err)
	}
	return map[string]any{"url": "/v1/transfers/" + token, "method": method, "headers": map[string]string{}, "expires_at": expires.UTC().Format("2006-01-02T15:04:05.000Z")}, nil
}
func (s *Store) ContentControl(ctx context.Context, a store.Access, action, storeID, id string, body json.RawMessage) (any, error) {
	if s.contentDir == "" {
		return nil, store.Refuse("not_supported")
	}
	if err := s.enter(); err != nil {
		return nil, err
	}
	defer s.life.RUnlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, dbError(ctx, err)
	}
	defer tx.Rollback()
	// A content action writes items, imports and attachments that a record write
	// authorizes against, so it takes that dataset's turn. Which dataset that is
	// comes from the store, before anything is read of it. A listing reads only.
	if action != "list" {
		var dataset string
		e := tx.QueryRowContext(ctx, `SELECT "dataset_id" FROM "tessera_content_stores" WHERE "id"=$1`, storeID).Scan(&dataset)
		if e == sql.ErrNoRows {
			return nil, store.Refuse("unavailable")
		}
		if e != nil {
			return nil, dbError(ctx, e)
		}
		if e = s.serializeWrites(ctx, tx, dataset); e != nil {
			return nil, e
		}
	}
	var result any
	if action == "list" {
		// One store per dataset the caller may use. A store belonging to a
		// dataset that is not theirs is absent, as that dataset is absent from
		// the dataset listing, rather than refused inside this one.
		stores := []any{}
		datasets, e := s.grantedIn(ctx, tx, a)
		if e != nil {
			return nil, e
		}
		for _, dataset := range datasets {
			auth, err := authorization(ctx, tx, dataset, a)
			if err != nil {
				continue
			}
			var current, replaced string
			err = tx.QueryRowContext(ctx, `SELECT "id",coalesce("replaced_store_id",'') FROM "tessera_content_stores" WHERE "dataset_id"=$1 AND "active"=1`, dataset).Scan(&current, &replaced)
			if err == sql.ErrNoRows {
				continue
			}
			if err != nil {
				return nil, dbError(ctx, err)
			}
			if _, _, err = s.contentAuthority(ctx, tx, a, current); err != nil {
				continue
			}
			stores = append(stores, map[string]any{"id": current, "dataset_id": dataset, "replaced_store_id": replaced, "capabilities": contentCapabilities, "transfer_modes": []string{"offset"}, "access": map[string]bool{"read": auth.Grants.Read, "write": auth.Grants.Write && auth.View.Full(), "delete": auth.Grants.Erase && auth.View.Full()}, "limits": map[string]int64{"max_item_bytes": maxItemBytes, "max_chunk_bytes": maxChunkBytes, "min_chunk_bytes": minChunkBytes, "url_lifetime_seconds": uriLifetime, "import_idle_seconds": contentIdle, "import_retention_seconds": 86400, "max_content_bytes": 1 << 30, "max_open_imports": 64}})
		}
		result = map[string]any{"stores": stores}
	} else {
		auth, owning, e := s.contentAuthority(ctx, tx, a, storeID)
		if e != nil {
			return nil, e
		}
		switch action {
		case "begin":
			result, err = s.beginImport(ctx, tx, a, auth, storeID, body)
		case "commit", "abort":
			result, err = s.finishImport(ctx, tx, a, auth, storeID, id, action)
		case "inspect", "resolve", "delete":
			result, err = s.contentItem(ctx, tx, a, auth, owning, storeID, id, action, body)
		case "erase":
			var request struct {
				Confirm string `json:"confirm"`
			}
			if err = p.DecodeRequest(body, &request, 4096); err != nil {
				return nil, err
			}
			if request.Confirm != storeID {
				return nil, store.Refuse("invalid_field")
			}
			if !auth.Grants.Erase || !auth.View.Full() {
				return nil, store.Refuse("permission_denied")
			}
			result, err = s.eraseContent(ctx, tx, a, storeID)
		default:
			return nil, store.Refuse("not_found")
		}
	}
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, dbError(ctx, err)
	}
	if action == "erase" {
		if err = s.journalRetirements(ctx); err != nil {
			return nil, err
		}
	}
	return result, nil
}
func (s *Store) beginImport(ctx context.Context, tx *sql.Tx, a store.Access, auth store.Authorization, storeID string, body []byte) (any, error) {
	if !auth.Grants.Write || !auth.View.Full() {
		return nil, store.Refuse("permission_denied")
	}
	var request struct {
		Operation   string `json:"operation_id"`
		ContentType string `json:"content_type"`
		Bytes       *int64 `json:"byte_count,omitempty"`
		Maximum     int64  `json:"maximum_bytes"`
		Mode        string `json:"transfer_mode,omitempty"`
	}
	if err := p.DecodeRequest(body, &request, 4096); err != nil {
		return nil, err
	}
	// A zero ceiling is legitimate: it is how an empty item is reserved, and
	// zero is the one total that no completing range can ever establish, so it
	// has to be declared up front or the item can never be committed.
	if p.ValidateIdentifier(p.ContentIdentifier, request.Operation) != nil || request.Maximum < 0 || request.Bytes != nil && (*request.Bytes < 0 || *request.Bytes > request.Maximum || *request.Bytes > maxItemBytes) {
		return nil, store.Refuse("invalid_field")
	}
	if _, _, err := mime.ParseMediaType(request.ContentType); err != nil || strings.ContainsAny(request.ContentType, "\r\n") {
		return nil, store.Refuse("invalid_field")
	}
	if request.Mode != "" && request.Mode != "offset" {
		return nil, store.Refuse("not_supported")
	}
	var upload, state, owner, authorizationID, contentType string
	var declared sql.NullInt64
	var maximum, received, updated int64
	var item sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT "upload_id","state","principal","authorization","content_type","declared_bytes","maximum_bytes","received_bytes","item_id","updated_at" FROM "tessera_imports" WHERE "store_id"=$1 AND "operation_id"=$2`, storeID, request.Operation).Scan(&upload, &state, &owner, &authorizationID, &contentType, &declared, &maximum, &received, &item, &updated)
	if err == nil {
		if a.Principal != owner || a.Authorization != authorizationID {
			return nil, store.Refuse("permission_denied")
		}
		if contentType != request.ContentType || maximum != request.Maximum || declared.Valid != (request.Bytes != nil) || (declared.Valid && declared.Int64 != *request.Bytes) {
			return nil, store.Refuse("conflict")
		}
		if state == "committed" {
			return map[string]any{"state": "committed", "item_id": item.String}, nil
		}
		if state != "open" || time.Now().UnixMilli()-updated > contentIdle*1000 {
			return nil, store.Refuse("expired")
		}
	} else if err == sql.ErrNoRows {
		var reserved, open int64
		if err = tx.QueryRowContext(ctx, `SELECT (SELECT coalesce(sum("byte_count"),0) FROM "tessera_items" WHERE "store_id"=$1 AND "deleted"=0)+coalesce(sum(CASE WHEN "maximum_bytes">67108864 THEN 67108864 ELSE "maximum_bytes" END),0),count(*) FROM "tessera_imports" WHERE "store_id"=$1 AND "state"='open'`, storeID).Scan(&reserved, &open); err != nil {
			return nil, dbError(ctx, err)
		}
		wanted := request.Maximum
		if wanted > maxItemBytes {
			wanted = maxItemBytes
		}
		if open >= 64 || reserved+wanted > 1<<30 {
			return nil, store.Refuse("limit_exceeded")
		}
		upload, err = cursorToken()
		if err != nil {
			return nil, err
		}
		upload = "u-" + upload
		_, err = tx.ExecContext(ctx, `INSERT INTO "tessera_imports" ("upload_id","store_id","operation_id","principal","authorization","content_type","declared_bytes","total_bytes","maximum_bytes","state","updated_at") VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'open',$10)`, upload, storeID, request.Operation, a.Principal, a.Authorization, request.ContentType, request.Bytes, request.Bytes, request.Maximum, time.Now().UnixMilli())
		if err != nil {
			return nil, dbError(ctx, err)
		}
	} else {
		return nil, dbError(ctx, err)
	}
	result, err := s.issueTransfer(ctx, tx, storeID, upload, "PATCH")
	if err != nil {
		return nil, err
	}
	result["upload_id"] = upload
	result["state"] = "open"
	result["received_bytes"] = received
	result["transfer_mode"] = "offset"
	return result, nil
}
func (s *Store) finishImport(ctx context.Context, tx *sql.Tx, a store.Access, auth store.Authorization, storeID, id, action string) (any, error) {
	if !auth.Grants.Write || !auth.View.Full() {
		return nil, store.Refuse("permission_denied")
	}
	var principal, authorizationID, state, contentType string
	var item sql.NullString
	var total sql.NullInt64
	var received, updated int64
	err := tx.QueryRowContext(ctx, `SELECT "principal","authorization","state","item_id","total_bytes","received_bytes","content_type","updated_at" FROM "tessera_imports" WHERE "store_id"=$1 AND "upload_id"=$2`, storeID, id).Scan(&principal, &authorizationID, &state, &item, &total, &received, &contentType, &updated)
	if err == sql.ErrNoRows {
		return nil, store.Refuse("not_found")
	}
	if err != nil {
		return nil, dbError(ctx, err)
	}
	if principal != a.Principal || authorizationID != a.Authorization {
		return nil, store.Refuse("permission_denied")
	}
	if state == "committed" {
		return map[string]any{"item_id": item.String}, nil
	}
	if action == "abort" {
		_, err = tx.ExecContext(ctx, `UPDATE "tessera_imports" SET "state"='aborted' WHERE "upload_id"=$1`, id)
		if err != nil {
			return nil, dbError(ctx, err)
		}
		return nil, nil
	}
	if state != "open" || time.Now().UnixMilli()-updated > contentIdle*1000 {
		return nil, store.Refuse("expired")
	}
	if !total.Valid || total.Int64 != received {
		return nil, store.Refuse("conflict")
	}
	itemID, err := cursorToken()
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO "tessera_items" ("store_id","item_id","upload_id","content_type","byte_count") VALUES ($1,$2,$3,$4,$5)`, storeID, itemID, id, contentType, received); err != nil {
		return nil, dbError(ctx, err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE "tessera_imports" SET "state"='committed',"item_id"=$1,"updated_at"=$2 WHERE "upload_id"=$3`, itemID, time.Now().UnixMilli(), id); err != nil {
		return nil, dbError(ctx, err)
	}
	return map[string]any{"item_id": itemID}, nil
}
func (s *Store) contentItem(ctx context.Context, tx *sql.Tx, a store.Access, auth store.Authorization, owning, storeID, id, action string, body []byte) (any, error) {
	if action == "delete" {
		if !auth.Grants.Erase || !auth.View.Full() {
			return nil, store.Refuse("permission_denied")
		}
	} else if !auth.Grants.Read {
		return nil, store.Refuse("permission_denied")
	}
	if !auth.View.Full() {
		// Constrained to the dataset the store belongs to. A record in another
		// dataset may hold a reference to this item — the reference names a
		// store and an item, and nothing stops one crossing — and without this
		// the row it writes here was read as if it were one of this dataset's
		// own. A share of one dataset then let its type allowance be applied
		// to an object in a dataset the reader cannot reach at all, which
		// turned somebody else's record into a key to these bytes.
		query := `SELECT a."object_id",a."type_id" FROM "tessera_attachments" a`
		var types map[string]p.Meta
		if auth.View.HasCollectionReads() {
			h, err := s.load(ctx, tx, a, owning, false)
			if err != nil {
				return nil, err
			}
			types = h.image.Types
			query = `SELECT a."object_id",a."type_id",o."fields" FROM "tessera_attachments" a JOIN ` +
				h.managed.source + ` AS o ON o."dataset_id"=a."dataset_id" AND o."object_id"=a."object_id" AND o."type_id"=a."type_id"`
		}
		rows, err := tx.QueryContext(ctx, query+` WHERE a."dataset_id"=$1 AND a."store_id"=$2 AND a."item_id"=$3`, owning, storeID, id)
		if err != nil {
			return nil, dbError(ctx, err)
		}
		visible := false
		for rows.Next() {
			var k store.ObjectKey
			var raw []byte
			if types != nil {
				err = rows.Scan(&k.ObjectID, &k.TypeID, &raw)
			} else {
				err = rows.Scan(&k.ObjectID, &k.TypeID)
			}
			if err != nil {
				break
			}
			if recordstate.Visible(auth.View, k.TypeID, k.ObjectID) {
				visible = true
				break
			}
			if types != nil && auth.View.AllowsQueryType(k.TypeID) {
				var row recordstate.Row
				if err = decodeStoredFields(raw, &row); err != nil {
					break
				}
				sequence, _ := strconv.ParseUint(k.ObjectID, 10, 64)
				if auth.View.AllowsCollectionRecord(k, sequence, recordstate.CollectionID(types, k.TypeID, row.Fields)) {
					visible = true
					break
				}
			}
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return nil, dbError(ctx, err)
		}
		if !visible {
			return nil, store.Refuse("unavailable")
		}
	}
	var contentType string
	var size int64
	err := tx.QueryRowContext(ctx, `SELECT "content_type","byte_count" FROM "tessera_items" WHERE "store_id"=$1 AND "item_id"=$2 AND "deleted"=0`, storeID, id).Scan(&contentType, &size)
	if err == sql.ErrNoRows {
		return nil, store.Refuse("not_found")
	}
	if err != nil {
		return nil, dbError(ctx, err)
	}
	if action == "delete" {
		if _, err = tx.ExecContext(ctx, `UPDATE "tessera_items" SET "deleted"=1 WHERE "store_id"=$1 AND "item_id"=$2`, storeID, id); err != nil {
			return nil, dbError(ctx, err)
		}
		return nil, nil
	}
	var request struct {
		Representation string `json:"representation"`
		Maximum        *int   `json:"maximum_pixel_size,omitempty"`
	}
	if err = p.DecodeRequest(body, &request, 4096); err != nil {
		return nil, err
	}
	// Three different answers, resolved by the author in the content protocol.
	// A representation this store never offers is a missing capability, so it
	// is not_supported; 415 unsupported_representation is reserved for a
	// capability that IS advertised but cannot be produced for this item. A
	// nonsensical combination is a malformed request. This backend advertises
	// neither optional representation, so 415 is currently unreachable here —
	// that is an absence of features, not of the distinction.
	switch request.Representation {
	case "original":
		if request.Maximum != nil {
			return nil, store.Refuse("invalid_request")
		}
	case "current":
		if request.Maximum != nil {
			return nil, store.Refuse("invalid_request")
		}
		if !s.advertisesContentCapability("current_representation") {
			return nil, store.Refuse("not_supported")
		}
		return nil, store.Refuse("unsupported_representation")
	case "thumbnail":
		if request.Maximum == nil || *request.Maximum < 1 || *request.Maximum > maxThumbnailSize {
			return nil, store.Refuse("invalid_request")
		}
		if !s.advertisesContentCapability("thumbnails") {
			return nil, store.Refuse("not_supported")
		}
		// The capability exists, so an item this server cannot decode is the
		// 415 case: advertised, but not producible for this particular item.
		if !thumbnailable(contentType) {
			return nil, store.Refuse("unsupported_representation")
		}
		if action == "inspect" {
			return map[string]any{"content_type": contentType, "representation": "thumbnail"}, nil
		}
		return s.issueTransfer(ctx, tx, storeID, id, "GET", thumbnailRepresentation(*request.Maximum))
	default:
		return nil, store.Refuse("invalid_request")
	}
	if action == "inspect" {
		return map[string]any{"content_type": contentType, "byte_count": size, "representation": "original"}, nil
	}
	result, err := s.issueTransfer(ctx, tx, storeID, id, "GET")
	if err != nil {
		return nil, err
	}
	result["content_type"] = contentType
	result["byte_count"] = size
	result["accepts_ranges"] = false
	return result, nil
}
func (s *Store) eraseContent(ctx context.Context, tx *sql.Tx, a store.Access, id string) (any, error) {
	next, err := cursorToken()
	if err != nil {
		return nil, err
	}
	next = "c-" + next
	if _, err = tx.ExecContext(ctx, `INSERT INTO "tessera_content_stores" ("id","dataset_id","active","replaced_store_id","principal","authorization") SELECT $1,"dataset_id",1,"id",$2,$3 FROM "tessera_content_stores" WHERE "id"=$4`, next, a.Principal, a.Authorization, id); err != nil {
		return nil, dbError(ctx, err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE "tessera_content_stores" SET "active"=0 WHERE "id"=$1`, id); err != nil {
		return nil, dbError(ctx, err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE "tessera_items" SET "deleted"=1 WHERE "store_id"=$1`, id); err != nil {
		return nil, dbError(ctx, err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE "tessera_imports" SET "state"='aborted' WHERE "store_id"=$1 AND "state"='open'`, id); err != nil {
		return nil, dbError(ctx, err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO "tessera_retirements" ("dataset_id","replacement_id","retired_at") VALUES ($1,$2,$3)`, id, next, time.Now().UnixMilli()); err != nil {
		return nil, dbError(ctx, err)
	}
	return map[string]any{"store_id": next}, nil
}

// SetContentCleanupDirectory keeps deletion obligations live when the content
// capability is disabled. Call only during host composition.
func (s *Store) SetContentCleanupDirectory(directory string) error {
	path, err := filepath.Abs(directory)
	if err != nil {
		return store.Refuse("store_unavailable")
	}
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil || !info.IsDir() {
		return store.Refuse("store_unavailable")
	}
	s.cleanupContentDir = path
	return nil
}
func (s *Store) byteDirectory() string {
	if s.contentDir != "" {
		return s.contentDir
	}
	return s.cleanupContentDir
}

// contentCapabilities is what this store advertises. Resolve checks a requested
// representation against exactly this list, so a capability cannot be refused
// as missing on one path while being advertised on another.
var contentCapabilities = []string{"writes", "thumbnails"}

func (s *Store) advertisesContentCapability(name string) bool {
	for _, advertised := range contentCapabilities {
		if advertised == name {
			return true
		}
	}
	return false
}
