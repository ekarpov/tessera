package sqlengine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"strconv"

	"github.com/ekarpov/tessera/internal/recordstate"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// held is one dataset as this engine sees it: the image an evaluator is
// restored from, the caller's authorization over it, and the bounds it was
// created with. Bounds belong to the dataset, not to the storage: they are
// stored on its row and advertised in its description.
type held struct {
	image   recordstate.Image
	auth    store.Authorization
	limits  p.Limits
	managed managedPlan
}

// load reads one dataset.
//
// Absence is decided before authorization, and deliberately. Erasure retires an
// identity and takes its authorizations with it, so a caller that held the
// dataset a moment ago holds nothing now; were the grant checked first, an
// erased dataset would answer permission_denied and the client would never
// learn it was gone. The protocol requires that absence be observable, and
// recovery through replaced_dataset_id depends on it.
//
// The cost is that an identity naming no dataset is distinguishable from one
// naming a dataset the caller may not use. Identities are minted with real
// entropy and never reused, so that is not an enumeration route, and the
// listing is still what conceals which datasets exist.
func (s *Store) load(ctx context.Context, tx *sql.Tx, a store.Access, dataset string, objects bool) (held, error) {
	h := held{image: recordstate.Image{Types: map[string]p.Meta{}, Objects: map[string]recordstate.Row{}}}
	image := &h.image
	var secret, bounds []byte
	var next string
	var schemaKey, managedKey sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT "dataset_id","secret","next_id","watermark","limits","schema_cache_key","managed_key" FROM "tessera_dataset" WHERE "dataset_id"=$1`, dataset).Scan(&image.ID, &secret, &next, &image.Watermark, &bounds, &schemaKey, &managedKey)
	if err == sql.ErrNoRows {
		// not_found means proven retirement and nothing else. Without evidence
		// in the retirement ledger this identity may name a dataset that never
		// existed, or one this caller simply may not see, and answering
		// not_found would tell somebody their data is gone on no evidence.
		var retired int
		if e := tx.QueryRowContext(ctx, `SELECT count(*) FROM "tessera_retirements" WHERE "dataset_id"=$1`, dataset).Scan(&retired); e != nil {
			return h, dbError(ctx, e)
		}
		if retired > 0 {
			return h, store.Refuse("not_found")
		}
		return h, store.Refuse("unavailable")
	}
	if err != nil {
		return h, dbError(ctx, err)
	}
	g, err := authorization(ctx, tx, dataset, a)
	h.auth = g
	if err != nil {
		// A caller holding no grant at all cannot see this dataset, and telling
		// it the dataset exists would undo what the listing conceals. A caller
		// that holds one and wants more than it covers is refused inside the
		// operation instead, where permission_denied is the honest answer.
		if p.ErrorCode(err) == "permission_denied" {
			return h, store.Refuse("unavailable")
		}
		return h, err
	}
	if json.Unmarshal(bounds, &h.limits) != nil || h.limits.MaxMaterializedBytes <= 0 {
		return h, store.Refuse("internal_error")
	}
	image.Next, err = strconv.ParseUint(next, 10, 64)
	if err != nil || len(secret) != 32 {
		return h, store.Refuse("internal_error")
	}
	copy(image.Secret[:], secret)
	cached, hit := s.schemas.get(schemaKey.String)
	size := cached.bytes
	if !hit {
		if err = tx.QueryRowContext(ctx, `SELECT coalesce(sum(length("body")),0) FROM "tessera_types" WHERE "dataset_id"=$1`, dataset).Scan(&size); err != nil {
			return h, dbError(ctx, err)
		}
	}
	schemaBytes := size
	if objects {
		var count, objectBytes int64
		if err = tx.QueryRowContext(ctx, `SELECT "object_count","object_bytes" FROM "tessera_dataset" WHERE "dataset_id"=$1`, dataset).Scan(&count, &objectBytes); err != nil {
			return h, dbError(ctx, err)
		}
		if count > int64(h.limits.MaxScannedRows) {
			return h, store.Refuse("query_result_limit_exceeded")
		}
		size += objectBytes
	}
	if size > int64(h.limits.MaxMaterializedBytes) {
		// A reason, because three lesser bounds are required to carry one and
		// a client that cannot tell this from "your query asked for too much"
		// retries the wrong thing forever.
		return h, store.RefuseWith("query_result_limit_exceeded", "materialization_limit",
			map[string]any{"maximum": h.limits.MaxMaterializedBytes})
	}
	if hit {
		image.Types = cached.types
	} else {
		rows, err := tx.QueryContext(ctx, `SELECT "type_id","body" FROM "tessera_types" WHERE "dataset_id"=$1`, dataset)
		if err != nil {
			return h, dbError(ctx, err)
		}
		for rows.Next() {
			var id string
			var raw []byte
			if err = rows.Scan(&id, &raw); err != nil {
				break
			}
			var m p.Meta
			if err = json.Unmarshal(raw, &m); err != nil {
				break
			}
			image.Types[id] = m
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return h, dbError(ctx, err)
		}
		s.schemas.put(schemaKey.String, cachedSchema{types: image.Types, bytes: schemaBytes})
	}
	cacheKey := schemaKey.String
	if cacheKey == "" {
		cacheKey = dataset + ":" + image.Watermark
	}
	if h.managed, err = s.loadManaged(ctx, tx, dataset, managedKey.String, cacheKey, image.Types); err != nil {
		return h, err
	}
	if !objects {
		return h, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT "object_id","type_id","fields","wire_bytes" FROM `+h.managed.source+` AS o WHERE "dataset_id"=$1`, dataset)
	if err != nil {
		return h, dbError(ctx, err)
	}
	for rows.Next() {
		var id string
		var row recordstate.Row
		var raw []byte
		if err = rows.Scan(&id, &row.TypeID, &raw, &row.Bytes); err != nil {
			break
		}
		if err = decodeStoredFields(raw, &row); err != nil {
			break
		}
		image.Objects[id] = row
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return h, dbError(ctx, err)
	}
	return h, nil
}

// outside is what a dataset holds beyond the records two images carry: zero
// when the images are the whole dataset, and the rest of it when they are
// only what one write touched. The bounds a write is held to are over the
// whole dataset either way.
type outside struct {
	rows  int
	bytes int
}

func (s *Store) persist(ctx context.Context, tx *sql.Tx, limits p.Limits, before, after recordstate.Image, rest outside, managed managedMapping) error {
	if before.Watermark == after.Watermark {
		return nil
	}
	// The image carries the identity the evaluator worked from, which is the
	// dataset these rows belong to even if the store is erased concurrently.
	dataset := after.ID
	if rest.rows+len(after.Objects) > limits.MaxScannedRows {
		return store.Refuse("query_result_limit_exceeded")
	}
	size := rest.bytes
	schemaChanged := len(before.Types) != len(after.Types)
	for id, m := range after.Types {
		raw, err := p.MarshalWire(m)
		if err != nil {
			return err
		}
		size += len(raw)
		if limits.MaxStoredBytes > 0 && size > limits.MaxStoredBytes {
			return datasetFull(limits)
		}
		old, ok := before.Types[id]
		if ok {
			encoded, e := p.MarshalWire(old)
			if e != nil {
				return e
			}
			if bytes.Equal(encoded, raw) {
				continue
			}
		}
		schemaChanged = true
		if _, err = tx.ExecContext(ctx, `INSERT INTO "tessera_types" ("dataset_id","type_id","body") VALUES ($1,$2,$3) ON CONFLICT ("dataset_id","type_id") DO UPDATE SET "body"=excluded."body"`, dataset, id, raw); err != nil {
			return dbError(ctx, err)
		}
	}
	hadManaged := len(managed) != 0
	if schemaChanged && hadManaged {
		var err error
		if managed, err = managed.withTypes(after.Types); err != nil {
			return err
		}
	}
	objectBytes := rest.bytes
	for id, row := range after.Objects {
		raw, err := p.MarshalWire(row.Fields)
		if err != nil {
			return err
		}
		size += len(raw)
		objectBytes += len(raw)
		if limits.MaxStoredBytes > 0 && size > limits.MaxStoredBytes {
			return datasetFull(limits)
		}
		old, ok := before.Objects[id]
		if ok && old.TypeID == row.TypeID {
			encoded, e := p.MarshalWire(old.Fields)
			if e != nil {
				return e
			}
			if bytes.Equal(encoded, raw) {
				continue
			}
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO "tessera_objects" ("dataset_id","object_id","type_id","wire_bytes") VALUES ($1,$2,$3,$4)
            ON CONFLICT ("dataset_id","object_id") DO UPDATE
            SET "type_id"=excluded."type_id","wire_bytes"=excluded."wire_bytes"`, dataset, id, row.TypeID, len(raw)); err != nil {
			return dbError(ctx, err)
		}
		if err = s.persistManagedRecord(ctx, tx, dataset, id, managed[row.TypeID], old.Fields, row.Fields, ok); err != nil {
			return err
		}
	}
	for id := range before.Objects {
		if _, ok := after.Objects[id]; !ok {
			if _, err := tx.ExecContext(ctx, `DELETE FROM "tessera_objects" WHERE "dataset_id"=$1 AND "object_id"=$2`, dataset, id); err != nil {
				return dbError(ctx, err)
			}
		}
	}
	for id := range before.Types {
		if _, ok := after.Types[id]; !ok {
			if _, err := tx.ExecContext(ctx, `DELETE FROM "tessera_types" WHERE "dataset_id"=$1 AND "type_id"=$2`, dataset, id); err != nil {
				return dbError(ctx, err)
			}
		}
	}
	var schemaKey any
	if schemaChanged {
		if hadManaged {
			if err := updateManagedKey(ctx, tx, dataset); err != nil {
				return err
			}
		}
		key, err := schemaCacheKey(ctx, tx, dataset)
		if err != nil {
			return err
		}
		schemaKey = key
	}
	// The count and size are the dataset's after this write: what the images
	// hold now, and what lay outside them untouched.
	if _, err := tx.ExecContext(ctx, `UPDATE "tessera_dataset" SET "next_id"=$1,"watermark"=$2,"object_count"=$3,"object_bytes"=$4,"schema_cache_key"=coalesce($5,"schema_cache_key") WHERE "dataset_id"=$6`, strconv.FormatUint(after.Next, 10), after.Watermark, rest.rows+len(after.Objects), objectBytes, schemaKey, dataset); err != nil {
		return dbError(ctx, err)
	}
	return nil
}

// storageLimits is what this storage declares, as distinct from what any one
// dataset does. It bounds a request body before a dataset is known and seeds a
// dataset created without bounds of its own.
//
// It is the widest MaxRequestBytes any dataset here declares, because a body a
// dataset would accept must not be cut off before that dataset is known. What
// is actually enforced is always the bounds on the dataset's own row.
func storageLimits(ctx context.Context, tx *sql.Tx) (p.Limits, error) {
	rows, err := tx.QueryContext(ctx, `SELECT "limits" FROM "tessera_dataset" ORDER BY "dataset_id"`)
	if err != nil {
		return p.Limits{}, dbError(ctx, err)
	}
	var widest p.Limits
	found := false
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			break
		}
		var limits p.Limits
		if json.Unmarshal(raw, &limits) != nil || limits.MaxMaterializedBytes <= 0 {
			err = store.Refuse("internal_error")
			break
		}
		if !found || limits.MaxRequestBytes > widest.MaxRequestBytes {
			widest, found = limits, true
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		if p.ErrorCode(err) == "internal_error" {
			return p.Limits{}, err
		}
		return p.Limits{}, dbError(ctx, err)
	}
	if !found {
		return p.Limits{}, store.Refuse("internal_error")
	}
	return widest, nil
}

// datasetLockKey derives an advisory-lock key from a dataset identity. Two
// identities colliding would serialize two datasets against each other, which
// costs concurrency and never correctness.
func datasetLockKey(dataset string) int64 {
	sum := sha256.Sum256([]byte("tessera-dataset-write\x00" + dataset))
	return int64(binary.BigEndian.Uint64(sum[:8]))
}

// serializeWrites holds one dataset's write lock until this transaction ends.
//
// A record write reads the whole image and writes it back, so two against one
// dataset must not interleave. Two against different datasets may, which is the
// point: the lock used to be a constant taken by the driver, which serialized
// every write in the database because a driver cannot know which dataset a
// transaction is for.
//
// SQLite needs nothing here — one writer at a time is a property of the file,
// not something this engine arranges — so writes to different datasets in one
// SQLite database still wait for each other. That is a bound of that engine
// rather than of the protocol.
//
// A transaction takes at most one of these, so they cannot deadlock against
// each other.
func (s *Store) serializeWrites(ctx context.Context, tx *sql.Tx, dataset string) error {
	if !s.postgres {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, datasetLockKey(dataset)); err != nil {
		return dbError(ctx, err)
	}
	return nil
}

// datasetFull is what a write gets when the dataset would pass its storage
// quota. Only a dataset with a quota has one: without it, size alone never
// stops a write.
//
// It used to be query_result_limit_exceeded, which was wrong in the plainest
// way available: there is no query, no result and no scan — the caller
// inserted one record. Nothing it can send is smaller enough, because the
// bound is on the dataset rather than on the request.
func datasetFull(limits p.Limits) error {
	return &p.Error{
		Code:    "dataset_full",
		Message: "Dataset has reached its storage quota",
		Details: map[string]any{"reason": "storage_limit", "maximum": limits.MaxStoredBytes},
	}
}

// tooLargeToWrite is what a write gets when performing it needs more of the
// dataset in memory than the materialization bound allows: a schema change
// that rewrites every record of a large dataset, say. Same code as a full
// dataset, because only the operator can change either, with its own reason.
func tooLargeToWrite(limits p.Limits) error {
	return &p.Error{
		Code:    "dataset_full",
		Message: "Dataset has outgrown this server's materialization bound",
		Details: map[string]any{"reason": "materialization_limit", "maximum": limits.MaxMaterializedBytes},
	}
}
