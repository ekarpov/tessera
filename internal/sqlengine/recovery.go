package sqlengine

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/ekarpov/tessera/internal/recordstate"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

type retained struct {
	state            string
	watermark, scope string
	dataset          string
	owner            store.Access
	digest           []byte
	status           sql.NullInt64
	body, objects    []byte
}

// RecoveryLimits bounds a request body before any dataset is known, so it is
// the storage's declared bound rather than any one dataset's. What a dataset
// actually enforces is on its own row and in its own description.
func (s *Store) RecoveryLimits() p.Limits { return s.bounds() }

// datasetLimits reads the bounds one dataset declares. A mutation is decoded
// against them before it is claimed, so a dataset whose bounds are tighter than
// the storage's refuses the request rather than accepting it and failing later.
//
// An identity naming no dataset is not refused here. Nothing has been
// authorized yet, and answering would tell a caller with no grant which
// identities exist; the storage's own bounds parse the request and the refusal
// comes from loading the dataset, after a grant has been required.
func (s *Store) datasetLimits(ctx context.Context, dataset string) (p.Limits, error) {
	var raw []byte
	var limits p.Limits
	err := s.reader.QueryRowContext(ctx, `SELECT "limits" FROM "tessera_dataset" WHERE "dataset_id"=$1`, dataset).Scan(&raw)
	if err == sql.ErrNoRows {
		return s.bounds(), nil
	}
	if err != nil {
		return limits, dbError(ctx, err)
	}
	if json.Unmarshal(raw, &limits) != nil || limits.MaxMaterializedBytes <= 0 {
		return limits, store.Refuse("internal_error")
	}
	return limits, nil
}
func readReceipt(ctx context.Context, tx *sql.Tx, dataset, key string) (retained, bool, error) {
	var r retained
	r.dataset = dataset
	err := tx.QueryRowContext(ctx, `SELECT "principal","authorization","digest","state","status","body","objects","watermark","replay_scope" FROM "tessera_receipts" WHERE "dataset_id"=$1 AND "key"=$2`, dataset, key).Scan(&r.owner.Principal, &r.owner.Authorization, &r.digest, &r.state, &r.status, &r.body, &r.objects, &r.watermark, &r.scope)
	if err == sql.ErrNoRows {
		return r, false, nil
	}
	if err != nil {
		return r, false, dbError(ctx, err)
	}
	// Older receipts did not retain the request category. Schema response shapes
	// identify schema-only work unambiguously; affected counts do not distinguish
	// updates from deletes and therefore retain the conservative read requirement.
	if r.scope == "read" && r.state == "committed" {
		var body struct {
			Results      []p.OperationResult `json:"results"`
			Type         *p.Meta             `json:"type"`
			DeletedTypes []string            `json:"deleted_types"`
		}
		if json.Unmarshal(r.body, &body) == nil {
			results := body.Results
			if results == nil && (body.Type != nil || body.DeletedTypes != nil) {
				results = []p.OperationResult{{Type: body.Type, DeletedTypes: body.DeletedTypes}}
			}
			schema, deleted := len(results) > 0, len(results) > 0
			for _, op := range results {
				if op.Type == nil && op.DeletedTypes == nil {
					schema = false
				}
				if op.Type != nil || op.DeletedTypes == nil {
					deleted = false
				}
			}
			if schema {
				r.scope = "schema"
			}
			if deleted {
				r.scope = "identities"
			}
		}
	}
	return r, true, nil
}
func (s *Store) Status(ctx context.Context, a store.Access, dataset, key string) (store.Receipt, error) {
	if !a.Valid() {
		return store.Receipt{}, store.Refuse("authentication_failed")
	}
	if err := p.ValidateOperationKey(key); err != nil {
		return store.Receipt{}, err
	}
	if err := s.enter(); err != nil {
		return store.Receipt{}, err
	}
	defer s.life.RUnlock()
	tx, err := s.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return store.Receipt{}, dbError(ctx, err)
	}
	defer tx.Rollback()
	r, found, err := readReceipt(ctx, tx, dataset, key)
	if err != nil {
		return store.Receipt{}, err
	}
	if !found {
		return store.Receipt{State: "unknown"}, nil
	}
	return receiptView(ctx, tx, a, r)
}
func receiptView(ctx context.Context, tx *sql.Tx, a store.Access, r retained) (store.Receipt, error) {
	if a != r.owner {
		return store.Receipt{}, store.Refuse("permission_denied")
	}
	// Legacy successful receipts still carry their original watermark in the body.
	if r.watermark == "" {
		var saved struct {
			Watermark string `json:"watermark"`
		}
		_ = json.Unmarshal(r.body, &saved)
		r.watermark = saved.Watermark
	}
	result := store.Receipt{State: r.state, Watermark: r.watermark}
	switch r.state {
	case "in_progress":
		return result, nil
	case "failed":
		result.Error = append([]byte(nil), r.body...)
		return result, nil
	case "committed":
		if len(r.objects) > 0 && json.Unmarshal(r.objects, &result.Objects) != nil {
			return result, store.Refuse("internal_error")
		}
		// A receipt names the dataset it was claimed against, which erasure
		// retires: its authorizations go with it, so the grant lookup resolves
		// nothing and the result is withheld without a separate comparison.
		g, err := authorization(ctx, tx, r.dataset, a)
		if err != nil && p.ErrorCode(err) != "permission_denied" {
			return result, err
		}
		var live int
		if e := tx.QueryRowContext(ctx, `SELECT count(*) FROM "tessera_dataset" WHERE "dataset_id"=$1`, r.dataset).Scan(&live); e != nil {
			return result, dbError(ctx, e)
		}
		visible := r.scope == "identities" || (err == nil && live > 0 && g.View.Full() && (g.Grants.Read || r.scope == "schema" && g.Grants.Schema))
		if visible {
			for _, ref := range result.Objects {
				var exists int
				err = tx.QueryRowContext(ctx, `SELECT 1 FROM "tessera_objects" WHERE "dataset_id"=$1 AND "object_id"=$2 AND "type_id"=$3`, r.dataset, ref.ObjectID, ref.MetaID).Scan(&exists)
				if err == sql.ErrNoRows {
					visible = false
					break
				}
				if err != nil {
					return result, dbError(ctx, err)
				}
			}
		}
		if visible {
			result.Result = append([]byte(nil), r.body...)
			result.Objects = nil
		} else {
			result.ResultWithheld = true
		}
		return result, nil
	default:
		return result, store.Refuse("internal_error")
	}
}
func replayView(ctx context.Context, tx *sql.Tx, a store.Access, r retained, digest [32]byte) (store.Reply, error) {
	if a != r.owner {
		return store.Reply{}, store.Refuse("permission_denied")
	}
	if subtle.ConstantTimeCompare(digest[:], r.digest) != 1 {
		return store.Reply{}, &p.Error{Code: "conflict", Message: "Key belongs to a different request", Details: map[string]any{"reason": "idempotency_key_reused"}}
	}
	if r.state == "in_progress" {
		return store.Reply{}, &p.Error{Code: "conflict", Message: "Operation is unresolved", Details: map[string]any{"state": "in_progress"}}
	}
	view, err := receiptView(ctx, tx, a, r)
	if err != nil {
		return store.Reply{}, err
	}
	if view.ResultWithheld {
		body, e := p.MarshalWire(view)
		return store.Reply{Status: 200, Body: body, Replayed: true}, e
	}
	return store.Reply{Status: int(r.status.Int64), Body: append([]byte(nil), r.body...), Replayed: true}, nil
}
func (s *Store) Replay(ctx context.Context, a store.Access, dataset, key string, digest [32]byte) (store.Reply, bool, error) {
	if !a.Valid() {
		return store.Reply{}, false, store.Refuse("authentication_failed")
	}
	if err := p.ValidateOperationKey(key); err != nil {
		return store.Reply{}, false, err
	}
	if err := s.enter(); err != nil {
		return store.Reply{}, false, err
	}
	defer s.life.RUnlock()
	tx, err := s.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return store.Reply{}, false, dbError(ctx, err)
	}
	defer tx.Rollback()
	r, found, err := readReceipt(ctx, tx, dataset, key)
	if err != nil || !found {
		return store.Reply{}, found, err
	}
	reply, err := replayView(ctx, tx, a, r, digest)
	return reply, true, err
}
func (s *Store) Mutate(ctx context.Context, a store.Access, m store.Mutation) (store.Reply, error) {
	return s.MutateWith(ctx, a, m, nil)
}

// MutateWith composes protocol writes, host rows/events and receipt outcome in
// one SQL transaction. A durable claim is committed before this callback starts.
// The callback has the same ownership restrictions as WithTx and is never called
// for a replay. The request digest must also identify the host's requested work.
func (s *Store) MutateWith(ctx context.Context, a store.Access, m store.Mutation, host func(store.Tx, *sql.Tx) error) (store.Reply, error) {
	if err := s.enter(); err != nil {
		return store.Reply{}, err
	}
	defer s.life.RUnlock()
	// The transport already bounded the original body. Revalidate grammar for
	// embedded callers too; serialization is not the receipt identity.
	raw, e := p.MarshalWire(m.Transaction)
	if e != nil {
		return store.Reply{}, e
	}
	limits, err := s.datasetLimits(ctx, m.Dataset)
	if err != nil {
		return store.Reply{}, err
	}
	if len(raw) > limits.MaxRequestBytes {
		limits.MaxRequestBytes = len(raw)
	}
	m.Transaction, err = p.DecodeTransaction(raw, limits)
	if err != nil {
		return store.Reply{}, err
	}
	if m.Single && len(m.Transaction.Operations) != 1 {
		return store.Reply{}, store.Refuse("invalid_field")
	}
	if m.SuccessStatus != 200 && m.SuccessStatus != 201 {
		return store.Reply{}, store.Refuse("invalid_field")
	}
	if m.Key != "" {
		if err = p.ValidateOperationKey(m.Key); err != nil {
			return store.Reply{}, err
		}
		if reply, found, e := s.claim(ctx, a, m); found || e != nil {
			return reply, e
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.Reply{}, dbError(ctx, err)
	}
	defer tx.Rollback()
	var reply store.Reply
	var objects []p.Reference
	err = s.evaluate(ctx, tx, a, m.Dataset, func(core store.Tx, sqltx *sql.Tx) error {
		// evaluate may run this twice — once against the records a write
		// touches, and again from the whole dataset when that could not
		// answer — so nothing it gathers outside itself may carry over.
		objects = nil
		result, e := store.ExecuteWith(core, m.Transaction, m.Dataset, limits, store.Represent(m.Represent))
		if e != nil {
			return e
		}
		if host != nil {
			if e = host(core, sqltx); e != nil {
				return e
			}
		}
		result.Watermark = core.Watermark()
		for _, op := range result.Results {
			for _, identity := range op.Objects {
				objects = append(objects, p.Reference{DatasetID: m.Dataset, ObjectID: identity.ID, MetaID: identity.MetaID})
			}
		}
		reply, e = store.SuccessReply(result, m.Single, m.SuccessStatus)
		return e
	})
	if err == nil && m.Key != "" {
		encoded, e := p.MarshalWire(objects)
		if e != nil {
			err = e
		} else {
			// Only this executor's own claim, and only if it is still in
			// progress. Another process resolving it as abandoned means this
			// one lost its session lock, and its work must not commit with an
			// outcome that says it failed: outcomes and data commit together or
			// this transaction does not commit at all.
			var resolved sql.Result
			resolved, err = tx.ExecContext(ctx, `UPDATE "tessera_receipts" SET "state"='committed',"status"=$1,"body"=$2,"objects"=$3,"watermark"=$4 WHERE "key"=$5 AND "dataset_id"=$6 AND "state"='in_progress' AND "executor_id"=$7`, reply.Status, []byte(reply.Body), encoded, replyWatermark(reply.Body), m.Key, m.Dataset, s.executor)
			if err == nil {
				var claimed int64
				if claimed, err = resolved.RowsAffected(); err == nil && claimed != 1 {
					err = store.Refuse("conflict")
				}
			}
		}
	}
	if err == nil {
		err = tx.Commit()
	}
	if err == nil {
		return reply, nil
	}
	// Roll back host rows before finalizing a definitive failure. A failed COMMIT
	// is reconciled by the same conditional update: a committed outcome wins.
	_ = tx.Rollback()
	if m.Key == "" {
		return store.Reply{}, err
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return s.failClaim(cleanup, a, m, err)
}
func (s *Store) claim(ctx context.Context, a store.Access, m store.Mutation) (store.Reply, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.Reply{}, false, dbError(ctx, err)
	}
	defer tx.Rollback()
	// A claim reads the image to authorize the work it is claiming, so it takes
	// the same turn a write does.
	if err = s.serializeWrites(ctx, tx, m.Dataset); err != nil {
		return store.Reply{}, false, err
	}
	r, found, err := readReceipt(ctx, tx, m.Dataset, m.Key)
	if err != nil {
		return store.Reply{}, false, err
	}
	if found {
		reply, e := replayView(ctx, tx, a, r, m.Digest)
		return reply, true, e
	}
	h, err := s.load(ctx, tx, a, m.Dataset, false)
	if err != nil {
		return store.Reply{}, false, err
	}
	image := h.image
	engine, err := recordstate.Restore(image, h.limits, a, h.auth, s.reads)
	if err != nil {
		return store.Reply{}, false, err
	}
	if err = engine.Transact(ctx, a, m.Dataset, func(core store.Tx) error { return store.AuthorizeTransaction(core, m.Transaction) }); err != nil {
		return store.Reply{}, false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO "tessera_receipts" ("key","dataset_id","principal","authorization","digest","claimed_at","state","watermark","replay_scope","executor_id") VALUES ($1,$2,$3,$4,$5,$6,'in_progress',$7,$8,$9)`, m.Key, m.Dataset, a.Principal, a.Authorization, m.Digest[:], time.Now().UnixMilli(), image.Watermark, replayScope(m.Transaction), s.executor)
	if err != nil {
		return store.Reply{}, false, dbError(ctx, err)
	}
	if err = tx.Commit(); err != nil {
		return store.Reply{}, false, dbError(ctx, err)
	}
	return store.Reply{}, false, nil
}
func (s *Store) failClaim(ctx context.Context, a store.Access, m store.Mutation, cause error) (store.Reply, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.Reply{}, dbError(ctx, err)
	}
	defer tx.Rollback()
	if err = s.serializeWrites(ctx, tx, m.Dataset); err != nil {
		return store.Reply{}, err
	}
	r, found, err := readReceipt(ctx, tx, m.Dataset, m.Key)
	if err != nil {
		return store.Reply{}, err
	}
	if !found {
		return store.Reply{}, store.Refuse("internal_error")
	}
	if r.state != "in_progress" {
		return replayView(ctx, tx, a, r, m.Digest)
	}
	reply := store.FailureReply(cause)
	if _, err = tx.ExecContext(ctx, `UPDATE "tessera_receipts" SET "state"='failed',"status"=$1,"body"=$2 WHERE "key"=$3 AND "dataset_id"=$4 AND "state"='in_progress'`, reply.Status, []byte(reply.Body), m.Key, m.Dataset); err != nil {
		return store.Reply{}, dbError(ctx, err)
	}
	if err = tx.Commit(); err != nil {
		return store.Reply{}, dbError(ctx, err)
	}
	return reply, nil
}

// PruneReceipts discards only terminal outcomes older than the advertised window
// measured from claim time. An unresolved claim is never discarded.
//
// The window is each dataset's own, because that is the one its description
// advertises. Taking the storage's would let a dataset promising a long window
// lose an outcome early, which a caller asking for its status would read as
// never having happened.
func (s *Store) PruneReceipts(ctx context.Context) error {
	if err := s.enter(); err != nil {
		return err
	}
	defer s.life.RUnlock()
	held, err := s.provisioned(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	for _, dataset := range held {
		limits, e := s.datasetLimits(ctx, dataset.ID)
		if e != nil {
			return e
		}
		cutoff := now.Add(-time.Duration(limits.IdempotencyRetentionSeconds) * time.Second).UnixMilli()
		if _, e = s.db.ExecContext(ctx, `DELETE FROM "tessera_receipts" WHERE "dataset_id"=$1 AND "state" <> 'in_progress' AND "claimed_at" < $2`, dataset.ID, cutoff); e != nil {
			return dbError(ctx, e)
		}
	}
	// A receipt whose dataset is gone is retained by nothing, so it goes by the
	// storage's window rather than being kept forever. A receipt carrying its
	// own deadline is excluded: it never had a dataset, so "the dataset is
	// gone" was never true of it, and the storage's *dataset* window has
	// nothing to say about how long the server promised to keep it.
	cutoff := now.Add(-time.Duration(s.bounds().IdempotencyRetentionSeconds) * time.Second).UnixMilli()
	if _, err = s.db.ExecContext(ctx, `DELETE FROM "tessera_receipts" WHERE "state" <> 'in_progress' AND "claimed_at" < $1 AND "retain_until" IS NULL AND NOT EXISTS (SELECT 1 FROM "tessera_dataset" d WHERE d."dataset_id"="tessera_receipts"."dataset_id")`, cutoff); err != nil {
		return dbError(ctx, err)
	}
	// And a receipt that carries one is swept by the promise it was given.
	if _, err = s.db.ExecContext(ctx, `DELETE FROM "tessera_receipts" WHERE "state" <> 'in_progress' AND "retain_until" IS NOT NULL AND "retain_until" < $1`, now.UnixMilli()); err != nil {
		return dbError(ctx, err)
	}
	return nil
}

func replyWatermark(raw []byte) string {
	var body struct {
		Watermark string `json:"watermark"`
	}
	_ = json.Unmarshal(raw, &body)
	return body.Watermark
}

func replayScope(t p.Transaction) string {
	schema, deleted := true, true
	for _, op := range t.Operations {
		switch op.Op {
		case "define_type", "update_type":
			deleted = false
		case "delete_type":
		case "delete", "delete_where":
			schema = false
		default:
			return "read"
		}
	}
	if deleted {
		return "identities"
	}
	if schema {
		return "schema"
	}
	return "read"
}

// legacyProcessKey is the session advisory lock a process of the previous kind
// holds for as long as it runs. Such a process resolves every in-progress claim
// as abandoned, including claims this one is still working on, so this one does
// not start beside it. Taking the key and letting it go proves none is running.
const legacyProcessKey int64 = 74657373657260

// executorKey derives an advisory-lock key from an executor identity. Two
// identities colliding would make one executor look alive while it is gone,
// which is the failure this cannot afford, so the identity is minted with real
// entropy and the key is taken from a digest of it rather than from a counter.
func executorKey(executor string) int64 {
	sum := sha256.Sum256([]byte("tessera-executor\x00" + executor))
	return int64(binary.BigEndian.Uint64(sum[:8]))
}

func newExecutor() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", store.Refuse("internal_error")
	}
	return "x-" + hex.EncodeToString(id[:]), nil
}

// ResolveAbandonedClaims fails the claims of executors that are no longer
// running, and leaves alone the claims of executors that are.
//
// An executor holds its own session lock for as long as it lives, taken before
// it can have made any claim. Another process asks whether an executor is gone
// by trying to take that lock: if it succeeds the executor's session ended, and
// a session ends only when the process does. Nothing here consults a clock,
// because a slow executor and a dead one look the same to one, and guessing
// wrong resolves live work as failed.
//
// An empty executor is a claim made before executors were recorded. Startup
// proves no process of that kind is running before it calls this.
//
// Two processes may reach the same executor at once; only one takes the lock,
// and the other tries again on its next sweep.
func (s *Store) ResolveAbandonedClaims(ctx context.Context) error {
	if !s.postgres {
		return nil
	}
	if err := s.enter(); err != nil {
		return err
	}
	defer s.life.RUnlock()
	return s.resolveAbandonedClaims(ctx)
}

func (s *Store) resolveAbandonedClaims(ctx context.Context) error {
	rows, err := s.reader.QueryContext(ctx, `SELECT DISTINCT "executor_id" FROM "tessera_receipts" WHERE "state"='in_progress'`)
	if err != nil {
		return dbError(ctx, err)
	}
	var executors []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		executors = append(executors, id)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return dbError(ctx, err)
	}
	failure, err := p.MarshalWire(map[string]any{"error": &p.Error{Code: "internal_error", Message: "Execution was interrupted before commit"}})
	if err != nil {
		return err
	}
	for _, executor := range executors {
		if executor == s.executor {
			continue
		}
		if executor != "" {
			var gone bool
			if err = s.pgOwner.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, executorKey(executor)).Scan(&gone); err != nil {
				return dbError(ctx, err)
			}
			if !gone {
				continue
			}
		}
		_, err = s.db.ExecContext(ctx, `UPDATE "tessera_receipts" SET "state"='failed',"status"=500,"body"=$1 WHERE "state"='in_progress' AND "executor_id"=$2`, failure, executor)
		if executor != "" {
			if _, e := s.pgOwner.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, executorKey(executor)); e != nil && err == nil {
				err = e
			}
		}
		if err != nil {
			return dbError(ctx, err)
		}
	}
	return nil
}

// claimCreation reserves a creation's outcome before the dataset exists.
//
// Every other receipt is keyed by the dataset it acts on. A creation has none
// yet, so it is keyed under store.ServerScope, which the dataset grammar can
// never produce. Without this a caller whose response was lost has no way to
// learn whether a dataset was made — it could only list and try to recognise a
// display name, which is not an identity and may collide.
//
// Found reports that the key already carries an outcome; the returned Info is
// then the one the original attempt produced.
// resolveRetainedCreation answers a repeat from what was retained, and reports
// false when nothing was. The three outcomes differ: a committed claim replays
// its identity, a failed one replays its failure, and an unresolved one is a
// conflict rather than either.
func resolveRetainedCreation(ctx context.Context, tx *sql.Tx, a store.Access, key string, digest [32]byte) (store.Created, bool, error) {
	r, found, err := readReceipt(ctx, tx, store.ServerScope, key)
	if err != nil || !found {
		return store.Created{}, false, err
	}
	if a != r.owner {
		return store.Created{}, true, store.Refuse("permission_denied")
	}
	if subtle.ConstantTimeCompare(digest[:], r.digest) != 1 {
		return store.Created{}, true, &p.Error{Code: "conflict", Message: "Key belongs to a different request", Details: map[string]any{"reason": "idempotency_key_reused"}}
	}
	switch r.state {
	case "in_progress":
		return store.Created{}, true, &p.Error{Code: "conflict", Message: "Operation is unresolved", Details: map[string]any{"state": "in_progress"}}
	case "failed":
		var problem struct {
			Error *p.Error `json:"error"`
		}
		if json.Unmarshal(r.body, &problem) == nil && problem.Error != nil {
			return store.Created{}, true, problem.Error
		}
		return store.Created{}, true, store.Refuse("internal_error")
	}
	var made struct {
		DatasetID string `json:"dataset_id"`
	}
	if json.Unmarshal(r.body, &made) != nil || made.DatasetID == "" {
		return store.Created{}, true, store.Refuse("internal_error")
	}
	// The retained identity, never a read of the dataset. An owner may be
	// recovering after its grant was withdrawn or after the dataset was erased,
	// and the answer to "what did I make" does not depend on whether it may
	// still read it.
	return store.Created{DatasetID: made.DatasetID, Replayed: true}, true, nil
}

// RecoverCreation answers from a retained claim and never makes one, so a
// caller may be answered before any gate on new work is applied.
func (s *Store) RecoverCreation(ctx context.Context, a store.Access, key string, digest [32]byte) (store.Created, bool, error) {
	if !a.Valid() {
		return store.Created{}, false, store.Refuse("authentication_failed")
	}
	if err := p.ValidateOperationKey(key); err != nil {
		return store.Created{}, false, err
	}
	if err := s.enter(); err != nil {
		return store.Created{}, false, err
	}
	defer s.life.RUnlock()
	tx, err := s.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return store.Created{}, false, dbError(ctx, err)
	}
	defer tx.Rollback()
	return resolveRetainedCreation(ctx, tx, a, key, digest)
}

func (s *Store) claimCreation(ctx context.Context, a store.Access, key string, digest [32]byte) (store.Created, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.Created{}, false, dbError(ctx, err)
	}
	defer tx.Rollback()
	retained, found, err := resolveRetainedCreation(ctx, tx, a, key, digest)
	if found || err != nil {
		return retained, found, err
	}
	// An identity outcome: what a lost caller needs back is which dataset now
	// exists, and that category replays without a grant over the dataset named,
	// which no caller could have held when the key was claimed.
	claimed := time.Now()
	// The promise is written with the claim, not read at sweep time. A window a
	// deployment later lowers applies to new claims; it cannot shorten one this
	// caller has already been told it has.
	deadline := claimed.Add(time.Duration(s.ServerRetentionSeconds()) * time.Second).UnixMilli()
	if _, err = tx.ExecContext(ctx, `INSERT INTO "tessera_receipts" ("key","dataset_id","principal","authorization","digest","claimed_at","state","watermark","replay_scope","executor_id","retain_until") VALUES ($1,$2,$3,$4,$5,$6,'in_progress','',$7,$8,$9)`,
		key, store.ServerScope, a.Principal, a.Authorization, digest[:], claimed.UnixMilli(), "identities", s.executor, deadline); err != nil {
		return store.Created{}, false, dbError(ctx, err)
	}
	if err = tx.Commit(); err != nil {
		return store.Created{}, false, dbError(ctx, err)
	}
	return store.Created{}, false, nil
}

// resolveCreation commits the outcome in the same transaction as the dataset,
// guarded on this executor's own in-progress claim for the same reason an
// ordinary write is: another process resolving it as abandoned means this one
// lost its session lock, and this work must not commit saying otherwise.
func (s *Store) resolveCreation(ctx context.Context, tx *sql.Tx, key, datasetID string) error {
	body, err := p.MarshalWire(map[string]any{"dataset_id": datasetID})
	if err != nil {
		return err
	}
	resolved, err := tx.ExecContext(ctx, `UPDATE "tessera_receipts" SET "state"='committed',"status"=201,"body"=$1 WHERE "key"=$2 AND "dataset_id"=$3 AND "state"='in_progress' AND "executor_id"=$4`,
		[]byte(body), key, store.ServerScope, s.executor)
	if err != nil {
		return dbError(ctx, err)
	}
	claimed, err := resolved.RowsAffected()
	if err != nil {
		return dbError(ctx, err)
	}
	if claimed != 1 {
		return store.Refuse("conflict")
	}
	return nil
}

// failCreation records a definitive failure against the claim, so a lost
// response to a creation that could not happen is still answerable. A claim
// left in progress would make the caller wait for an outcome that never comes.
func (s *Store) failCreation(ctx context.Context, key string, cause error) {
	reply := store.FailureReply(cause)
	resolve, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, _ = s.db.ExecContext(resolve, `UPDATE "tessera_receipts" SET "state"='failed',"status"=$1,"body"=$2 WHERE "key"=$3 AND "dataset_id"=$4 AND "state"='in_progress' AND "executor_id"=$5`,
		reply.Status, []byte(reply.Body), key, store.ServerScope, s.executor)
}

// definitelyNothingCommitted reports whether a creation's failure is one this
// store can state as an outcome. The contract forbids resolving an uncertain
// outcome into a failure, and the uncertainty that matters is not exotic: a
// cancelled request and a dropped connection both arrive as errors while
// saying nothing about whether the transaction committed.
//
// A refusal this store produced itself — a validation error, a conflict, a
// permission answer — is definite, because it happened before or instead of
// the commit. Anything else is treated as unresolved, which costs a caller one
// status call and costs it nothing to be wrong about.
func definitelyNothingCommitted(ctx context.Context, failure error) bool {
	if ctx.Err() != nil {
		return false
	}
	var refusal *p.Error
	return errors.As(failure, &refusal) && refusal.Code != "internal_error"
}
