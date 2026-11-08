package sqlengine

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ekarpov/tessera/internal/recordstate"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

func (s *Store) PreviewErase(ctx context.Context, a store.Access, dataset string) (store.ErasePreview, error) {
	var result store.ErasePreview
	if err := s.enter(); err != nil {
		return result, err
	}
	defer s.life.RUnlock()
	tx, err := s.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, dbError(ctx, err)
	}
	defer tx.Rollback()
	if err = eraseAuthority(ctx, tx, a, dataset); err != nil {
		return result, err
	}
	result.DatasetID = dataset
	if err = tx.QueryRowContext(ctx, `SELECT "watermark",(SELECT count(*) FROM "tessera_objects" WHERE "dataset_id"=$1),(SELECT count(*) FROM "tessera_types" WHERE "dataset_id"=$1),(SELECT count(DISTINCT "principal") FROM "tessera_access" WHERE "dataset_id"=$1 AND "principal"<>$2) FROM "tessera_dataset" WHERE "dataset_id"=$1`, dataset, a.Principal).Scan(&result.Watermark, &result.ObjectCount, &result.TypeCount, &result.AffectedPrincipals); err != nil {
		return result, dbError(ctx, err)
	}
	return result, nil
}
func eraseAuthority(ctx context.Context, tx *sql.Tx, a store.Access, dataset string) error {
	// An identity that names no live dataset is absence, not a refusal: Erase
	// mints a replacement, so the erased identity stops existing entirely.
	var present int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM "tessera_dataset" WHERE "dataset_id"=$1`, dataset).Scan(&present)
	if err != nil {
		return dbError(ctx, err)
	}
	if present == 0 {
		return store.Refuse("not_found")
	}
	auth, err := authorization(ctx, tx, dataset, a)
	if err != nil {
		return err
	}
	if !auth.Grants.Erase {
		return store.Refuse("permission_denied")
	}
	return nil
}
func (s *Store) Erase(ctx context.Context, a store.Access, dataset, confirm string) (store.EraseResult, error) {
	var result store.EraseResult
	if confirm != dataset {
		return result, store.Refuse("invalid_field")
	}
	if err := s.enter(); err != nil {
		return result, err
	}
	defer s.life.RUnlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, dbError(ctx, err)
	}
	defer tx.Rollback()
	// Erasure rewrites everything this dataset owns, so nothing else may be
	// writing it while that happens.
	if err = s.serializeWrites(ctx, tx, dataset); err != nil {
		return result, err
	}
	if err = eraseAuthority(ctx, tx, a, dataset); err != nil {
		return result, err
	}
	limits, err := s.datasetLimits(ctx, dataset)
	if err != nil {
		return result, err
	}
	if result, err = s.eraseWithin(ctx, tx, a, dataset, limits); err != nil {
		return result, err
	}
	if err = tx.Commit(); err != nil {
		return result, dbError(ctx, err)
	}
	// Publish retirement to the independent restore fence before acknowledging.
	// If interrupted, startup drains the committed queue before serving requests.
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err = s.journalRetirements(cleanup); err != nil {
		return result, err
	}
	return result, nil
}

// eraseWithin is an erasure's work inside a transaction the caller owns and
// commits: every row the dataset owns is removed, the dataset is renamed to a
// fresh identity provisioned only for a, and the retirement is queued. It
// checks no authority; Erase does that first, and a restore re-applying an
// erasure that already happened has none to check.
func (s *Store) eraseWithin(ctx context.Context, tx *sql.Tx, a store.Access, dataset string, limits p.Limits) (store.EraseResult, error) {
	var result store.EraseResult
	// What the erasing authorization holds, read before its access row goes
	// with the rest. A restore re-applying an erasure holds nothing here.
	held, heldErr := authorization(ctx, tx, dataset, a)
	fresh, err := recordstate.New(limits)
	if err != nil {
		return result, err
	}
	image, err := fresh.Image(ctx)
	if err != nil {
		return result, err
	}
	for _, statement := range []string{`DELETE FROM "tessera_sequences" WHERE "dataset_id"=$1`, `DELETE FROM "tessera_objects" WHERE "dataset_id"=$1`, `DELETE FROM "tessera_types" WHERE "dataset_id"=$1`, `DELETE FROM "tessera_access" WHERE "dataset_id"=$1`} {
		if _, err = tx.ExecContext(ctx, statement, dataset); err != nil {
			return result, dbError(ctx, err)
		}
	}
	// Only the erasing authorization is provisioned on the replacement, and
	// with exactly the grants it held on what it erased. It used to be given
	// every right, so a person who could never define types — a study's
	// participant, holding read, write and erase — gained schema on the
	// replacement by erasing.
	if heldErr == nil {
		generation, e := cursorToken()
		if e != nil {
			return result, e
		}
		grants, _ := json.Marshal(held.Grants)
		// Keyed by the replacement, which the dataset row does not yet name.
		// Nothing references that row, so the provisioning may precede the
		// rename.
		if _, e = tx.ExecContext(ctx, `INSERT INTO "tessera_access" ("dataset_id","principal","authorization","grants","generation") VALUES ($1,$2,$3,$4,$5)`, image.ID, a.Principal, a.Authorization, grants, generation); e != nil {
			return result, dbError(ctx, e)
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE "tessera_receipts" SET "body"='{}' WHERE "dataset_id"=$1 AND "state"='committed' AND "replay_scope"<>'identities'`, dataset); err != nil {
		return result, dbError(ctx, err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO "tessera_retirements" ("dataset_id","replacement_id","retired_at") VALUES ($1,$2,$3)`, dataset, image.ID, time.Now().UnixMilli()); err != nil {
		return result, dbError(ctx, err)
	}

	var contentCount int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM "tessera_content_stores" WHERE "dataset_id"=$1 AND "active"=1`, dataset).Scan(&contentCount); err != nil {
		return result, dbError(ctx, err)
	}
	if contentCount > 0 {
		var oldContent string
		if err = tx.QueryRowContext(ctx, `SELECT "id" FROM "tessera_content_stores" WHERE "dataset_id"=$1 AND "active"=1`, dataset).Scan(&oldContent); err != nil {
			return result, dbError(ctx, err)
		}
		// eraseContent resolves the store against the dataset that still owns it,
		// so the byte store is carried to the replacement only afterwards.
		if _, err = s.eraseContent(ctx, tx, a, oldContent); err != nil {
			return result, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE "tessera_content_stores" SET "dataset_id"=$1 WHERE "dataset_id"=$2 AND "active"=1`, image.ID, dataset); err != nil {
			return result, dbError(ctx, err)
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM "tessera_attachments" WHERE "dataset_id"=$1`, dataset); err != nil {
			return result, dbError(ctx, err)
		}
	}
	// The rename is last: everything above resolves against the erased identity.
	if _, err = tx.ExecContext(ctx, `UPDATE "tessera_dataset" SET "dataset_id"=$1,"secret"=$2,"next_id"=$3,"watermark"=$4,"replaced_dataset_id"=$5,"object_count"=0,"object_bytes"=0 WHERE "dataset_id"=$6`, image.ID, image.Secret[:], strconv.FormatUint(image.Next, 10), image.Watermark, dataset, dataset); err != nil {
		return result, dbError(ctx, err)
	}
	return store.EraseResult{DatasetID: image.ID, Watermark: image.Watermark}, nil
}

// CheckpointCleanup retries physical page reclamation. Logical access was
// already revoked by Erase. Backups must use the restore fence, not raw copying.
func (s *Store) CheckpointCleanup(ctx context.Context) error {
	if err := s.enter(); err != nil {
		return err
	}
	defer s.life.RUnlock()
	if err := s.journalRetirements(ctx); err != nil {
		return err
	}
	var pending int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM "tessera_retirements" WHERE "cleaned"=0`).Scan(&pending); err != nil {
		return dbError(ctx, err)
	}
	if pending == 0 {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `VACUUM`); err != nil {
		return dbError(ctx, err)
	}
	if s.postgres {
		_, err := s.db.ExecContext(ctx, `UPDATE "tessera_retirements" SET "cleaned"=1 WHERE "journaled"=1`)
		if err != nil {
			return dbError(ctx, err)
		}
		return nil
	}
	var busy, log, checkpointed int
	if err := s.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &log, &checkpointed); err != nil {
		return dbError(ctx, err)
	}
	if busy != 0 {
		return store.Refuse("dataset_unavailable")
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE "tessera_retirements" SET "cleaned"=1 WHERE "journaled"=1`); err != nil {
		return dbError(ctx, err)
	}
	return nil
}

// journalRetirements brings this process's retirement ledger up to date with
// every retirement the storage records, and returns once it is durable.
//
// Each process keeps its own ledger file, and several may share one
// database. The work used to be the retirements nobody had journaled yet,
// judged by a flag in the shared database, so the first process to journal a
// retirement set the flag and every other process's ledger missed it for
// good — and a restore given that ledger brought an erased dataset back. Now
// each process compares what the storage records with what its own ledger
// holds and appends the difference, whoever made the retirement. The flag
// keeps its one other meaning: some ledger holds it, which cleanup waits on.
func (s *Store) journalRetirements(ctx context.Context) error {
	s.journalMu.Lock()
	defer s.journalMu.Unlock()
	rows, err := s.db.QueryContext(ctx, `SELECT "dataset_id","journaled" FROM "tessera_retirements"`)
	if err != nil {
		return dbError(ctx, err)
	}
	var recorded []string
	unflagged := map[string]bool{}
	for rows.Next() {
		var id string
		var journaled int
		if err = rows.Scan(&id, &journaled); err != nil {
			break
		}
		recorded = append(recorded, id)
		if journaled == 0 {
			unflagged[id] = true
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return dbError(ctx, err)
	}
	if len(recorded) == 0 {
		return nil
	}
	held := map[string]bool{}
	existing, err := os.ReadFile(s.path + ".retired")
	if err != nil && !os.IsNotExist(err) {
		return store.Refuse("dataset_unavailable")
	}
	for _, line := range bytes.Split(existing, []byte{'\n'}) {
		var id string
		if len(line) > 0 && json.Unmarshal(line, &id) == nil {
			held[id] = true
		}
	}
	var missing []string
	for _, id := range recorded {
		if !held[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		f, e := os.OpenFile(s.path+".retired", os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0600)
		if e != nil {
			return store.Refuse("dataset_unavailable")
		}
		for _, id := range missing {
			raw, _ := json.Marshal(id)
			if _, e = f.Write(append(raw, '\n')); e != nil {
				break
			}
		}
		if e == nil {
			e = f.Sync()
		}
		closeErr := f.Close()
		if e != nil || closeErr != nil {
			return store.Refuse("dataset_unavailable")
		}
		directory, e := os.Open(filepath.Dir(s.path))
		if e != nil {
			return store.Refuse("dataset_unavailable")
		}
		e = directory.Sync()
		directory.Close()
		if e != nil {
			return store.Refuse("dataset_unavailable")
		}
	}
	for id := range unflagged {
		if _, err = s.db.ExecContext(ctx, `UPDATE "tessera_retirements" SET "journaled"=1 WHERE "dataset_id"=$1`, id); err != nil {
			return dbError(ctx, err)
		}
	}
	return nil
}
func (s *Store) checkRestoreFence(ctx context.Context) error {
	f, err := os.Open(s.path + ".retired")
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return store.Refuse("dataset_unavailable")
	}
	defer f.Close()
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		var retired string
		if json.Unmarshal(scan.Bytes(), &retired) != nil {
			return store.Refuse("dataset_unavailable")
		}
		// A retired identity that still names a live dataset or an active content
		// store means the database predates the retirement it was restored past.
		var live, activeContent int
		if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM "tessera_dataset" WHERE "dataset_id"=$1`, retired).Scan(&live); err != nil {
			return dbError(ctx, err)
		}
		if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM "tessera_content_stores" WHERE "id"=$1 AND "active"=1`, retired).Scan(&activeContent); err != nil {
			return dbError(ctx, err)
		}
		if live > 0 || activeContent > 0 {
			return store.Refuse("dataset_unavailable")
		}
	}
	if scan.Err() != nil {
		return store.Refuse("dataset_unavailable")
	}
	return nil
}

// maxDisplayNameBytes bounds a name a person will be shown. It is generous for
// a label and far short of anything that could be used to carry a payload.
const maxDisplayNameBytes = 200

// ValidateDisplayName accepts a name for a dataset. The name is host-supplied
// and shown to people, so it must be text: control characters would let a name
// rewrite a terminal or a log line it is printed into.
func ValidateDisplayName(name string) error {
	if name == "" || len(name) > maxDisplayNameBytes || !utf8.ValidString(name) {
		return store.Refuse("invalid_field")
	}
	if strings.TrimSpace(name) == "" {
		return store.Refuse("invalid_field")
	}
	for _, r := range name {
		if r == utf8.RuneError || unicode.IsControl(r) {
			return store.Refuse("invalid_field")
		}
	}
	return nil
}

// ServerRetentionSeconds is the floor for receipts belonging to no dataset. It
// is separate from the storage's dataset window on purpose: raising that one to
// reach this would also lengthen every created dataset's own write receipts,
// because a creation takes the storage's limits when the request carries none.
func (s *Store) ServerRetentionSeconds() int { return store.DefaultServerRetentionSeconds }

// CreateDataset provisions a dataset in this storage and makes the caller its
// administrator, which is the same policy Erase applies to a replacement: the
// authorization that asked for it is the only one provisioned, and any further
// grant is installed by the host.
//
// Creating a dataset is composition, like installing a grant. It is deliberately
// not a protocol operation: the contract describes datasets a server already
// holds and says nothing about how one comes to exist.
//
// Bounds are the dataset's own from here on. Passing the zero value takes the
// storage's, which is what a host with nothing particular to say should do.
//
// A non-empty key retains the outcome under store.ServerScope so a caller whose
// response was lost can ask what happened instead of guessing. The receipt is
// recorded as an identity outcome, which is what it is: the answer a caller
// needs back is which dataset now exists, and an identity outcome is the one
// category a replay returns without a grant over the dataset it names — which
// matters here because the dataset did not exist when the key was claimed.
func (s *Store) CreateDataset(ctx context.Context, a store.Access, displayName string, limits p.Limits, types []p.Meta, key string, digest [32]byte) (made store.Created, failure error) {
	if !a.Valid() {
		return store.Created{}, store.Refuse("permission_denied")
	}
	if key != "" {
		if err := p.ValidateOperationKey(key); err != nil {
			return store.Created{}, err
		}
	}
	if err := ValidateDisplayName(displayName); err != nil {
		return store.Created{}, err
	}
	if limits.MaxMaterializedBytes <= 0 {
		limits = s.defaults
	}
	// Stored as the number rather than as an absent member, so that what this
	// dataset declares can be read back without knowing which default was in
	// force when it was made.
	if limits.MaxAggregateGroups <= 0 {
		limits.MaxAggregateGroups = p.DefaultAggregateGroups
	}
	// The retention floor is enforced where a dataset is made, because that is
	// where a deployment's own limits first become a promise to a caller.
	if limits.MaxScannedRows <= 0 || limits.IdempotencyRetentionSeconds < p.MinimumIdempotencyRetentionSeconds || limits.MaxRequestBytes <= 0 {
		return store.Created{}, store.Refuse("invalid_field")
	}
	if err := s.enter(); err != nil {
		return store.Created{}, err
	}
	defer s.life.RUnlock()
	fresh, err := recordstate.New(limits)
	if err != nil {
		return store.Created{}, err
	}
	image, err := fresh.Image(ctx)
	if err != nil {
		return store.Created{}, err
	}
	// Types given with the creation are defined on the fresh dataset through
	// the ordinary define_type path, so every rule a later schema write meets
	// applies here unchanged — identifiers, parents, roles, annotations, scale.
	// And before the key is claimed: an invalid schema is a malformed request,
	// and a malformed request must not spend a key, which is the rule for every
	// other operation's grammar.
	typed := image
	if len(types) > 0 {
		installer := store.Access{Principal: "creation", Authorization: "creation"}
		if err = fresh.SetAuthorization(ctx, image.ID, installer, store.Authorization{Grants: store.Grants{Schema: true}, View: store.FullView()}); err != nil {
			return store.Created{}, err
		}
		if err = fresh.Transact(ctx, installer, image.ID, func(tx store.Tx) error {
			for _, meta := range types {
				if _, e := tx.CreateMetadata(meta); e != nil {
					return e
				}
			}
			return nil
		}); err != nil {
			return store.Created{}, err
		}
		if typed, err = fresh.Image(ctx); err != nil {
			return store.Created{}, err
		}
	}
	bounds, err := json.Marshal(limits)
	if err != nil {
		return store.Created{}, err
	}
	generation, err := cursorToken()
	if err != nil {
		return store.Created{}, err
	}
	grants, err := json.Marshal(store.Grants{Read: true, Write: true, Schema: true, Erase: true})
	if err != nil {
		return store.Created{}, err
	}
	view, err := json.Marshal(store.FullView())
	if err != nil {
		return store.Created{}, err
	}
	if key != "" {
		retained, found, e := s.claimCreation(ctx, a, key, digest)
		if found || e != nil {
			return retained, e
		}
		// From here the claim exists, so a failure this store can vouch for has
		// to become an outcome: a claim left in progress makes the caller wait
		// for an answer that never arrives.
		//
		// But only a failure it can vouch for. A cancelled context or a lost
		// connection says this process stopped watching, not that nothing
		// committed, and recording `failed` for a creation that did commit
		// would hand the caller a definitive wrong answer. Those stay
		// in_progress, which is the honest state and the one a later status
		// call or a restart resolves.
		defer func() {
			if failure != nil && definitelyNothingCommitted(ctx, failure) {
				s.failCreation(ctx, key, failure)
			}
		}()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.Created{}, dbError(ctx, err)
	}
	defer tx.Rollback()
	// A minted identity cannot collide with a live one, which the primary key
	// settles, nor with a retired one, which sixteen bytes of entropy settles.
	// No guard here, for the reason the feature matrix gives for the ones it
	// does not carry: a check no composition can reach is a check no test can
	// prove, and an unprovable check is worse than none.
	if _, err = tx.ExecContext(ctx, `INSERT INTO "tessera_dataset" ("dataset_id","display_name","secret","next_id","watermark","limits") VALUES ($1,$2,$3,$4,$5,$6)`, image.ID, displayName, image.Secret[:], strconv.FormatUint(image.Next, 10), image.Watermark, bounds); err != nil {
		return store.Created{}, dbError(ctx, err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO "tessera_access" ("dataset_id","principal","authorization","grants","generation","view") VALUES ($1,$2,$3,$4,$5,$6)`, image.ID, a.Principal, a.Authorization, grants, generation, view); err != nil {
		return store.Created{}, dbError(ctx, err)
	}
	// Written as the schema change it is, from the untyped image to the typed
	// one, in this transaction: the dataset and its types commit together or
	// not at all, so nobody can reach a dataset that exists without them.
	if len(types) > 0 {
		if err = s.persist(ctx, tx, limits, image, typed, outside{}); err != nil {
			return store.Created{}, err
		}
	}
	// A dataset created into a storage that already serves content gets a store
	// of its own, never a share of another dataset's.
	if s.contentDir != "" {
		if err = provisionContent(ctx, tx); err != nil {
			return store.Created{}, err
		}
	}
	// The outcome commits with the dataset it describes. A creation that
	// committed while its receipt said otherwise is the failure the whole
	// idempotency discipline exists to prevent.
	if key != "" {
		if err = s.resolveCreation(ctx, tx, key, image.ID); err != nil {
			return store.Created{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return store.Created{}, dbError(ctx, err)
	}
	// A dataset created with a wider max_request_bytes than any before it
	// widens what is read before a dataset is known. The dataset exists
	// whatever happens here, so a failure to refresh is not a failure to
	// create: the bound catches up at the next change or restart.
	_ = s.refreshBounds(ctx)
	return store.Created{DatasetID: image.ID}, nil
}

// Provisioned names every dataset in this storage, for composition only.
//
// It takes no access, which is the point: this is the call that names the
// dataset a first grant is installed on, and no grant could name it. The
// composition owns the storage, so enumerating it is not a disclosure. Nothing
// routes a request here — what a caller may see is Datasets.
func (s *Store) Provisioned(ctx context.Context) ([]store.Dataset, error) {
	if err := s.enter(); err != nil {
		return nil, err
	}
	defer s.life.RUnlock()
	return s.provisioned(ctx)
}

func (s *Store) provisioned(ctx context.Context) ([]store.Dataset, error) {
	rows, err := s.reader.QueryContext(ctx, `SELECT "dataset_id","display_name" FROM "tessera_dataset" ORDER BY "dataset_id"`)
	if err != nil {
		return nil, dbError(ctx, err)
	}
	var held []store.Dataset
	for rows.Next() {
		var d store.Dataset
		if err = rows.Scan(&d.ID, &d.DisplayName); err != nil {
			break
		}
		held = append(held, d)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, dbError(ctx, err)
	}
	return held, nil
}

// SoleDataset names the one dataset a storage holds, for a composition built
// around exactly one. It refuses rather than choosing when there are several,
// because picking one arbitrarily is how a deployment ends up administering a
// dataset nobody meant.
func (s *Store) SoleDataset(ctx context.Context) (store.Dataset, error) {
	held, err := s.Provisioned(ctx)
	if err != nil {
		return store.Dataset{}, err
	}
	if len(held) != 1 {
		return store.Dataset{}, store.Refuse("conflict")
	}
	return held[0], nil
}
