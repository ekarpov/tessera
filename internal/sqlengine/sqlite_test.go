package sqlengine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

var owner = store.Access{Principal: "synthetic", Authorization: "owner"}

const typeID = "test.record"
const insertBody = `{"operations":[{"op":"insert","type":"test.record","fields":{"n":{"type":"integer","value":9007199254740993}},"as":"entry"}]}`

func fixture(t *testing.T) (*Store, string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dataset.db")
	s, err := Open(t.Context(), path, p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err = s.SetAuthorization(t.Context(), sole(t, s), owner, store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true}, View: store.FullView()}); err != nil {
		t.Fatal(err)
	}
	err = s.Transact(t.Context(), owner, sole(t, s), func(tx store.Tx) error {
		_, e := tx.CreateMetadata(p.Meta{ID: typeID, Annotations: map[string]json.RawMessage{"opaque": json.RawMessage(`{ "z": 1e2, "a": [ true, false ] }`)}, Fields: []p.Field{{ID: "n", MetaID: typeID, Type: p.IntegerType}}})
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.Info(t.Context(), owner, sole(t, s))
	if err != nil {
		t.Fatal(err)
	}
	return s, path, info.ID
}
func mutation(t *testing.T, dataset, key, body string) store.Mutation {
	t.Helper()
	request, err := p.DecodeTransaction([]byte(body), p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	return store.Mutation{Dataset: dataset, Key: key, Digest: p.RequestDigest("POST", "/v1/datasets/"+dataset+":transact", []byte(body)), Transaction: request, SuccessStatus: 200}
}
func key(n int) string { return fmt.Sprintf("00000000-0000-0000-0000-%012d", n) }

// sole names the dataset a test store holds. Tests that are not about routing
// name it this way; those that are name an identity outright.
func sole(t *testing.T, s *Store) string {
	t.Helper()
	held, err := s.SoleDataset(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return held.ID
}

// tighten rewrites the bounds one dataset declares. They are stored on its row
// and read from there on every operation, which is what makes them the
// dataset's rather than the storage's.
func tighten(t *testing.T, s *Store, dataset string, change func(*p.Limits)) {
	t.Helper()
	var raw []byte
	if err := s.db.QueryRow(`SELECT "limits" FROM "tessera_dataset" WHERE "dataset_id"=$1`, dataset).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var limits p.Limits
	if err := json.Unmarshal(raw, &limits); err != nil {
		t.Fatal(err)
	}
	change(&limits)
	raw, err := json.Marshal(limits)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE "tessera_dataset" SET "limits"=$1 WHERE "dataset_id"=$2`, raw, dataset); err != nil {
		t.Fatal(err)
	}
}
func record(t *testing.T, s *Store, dataset, id string) p.RecordSet {
	t.Helper()
	var result p.RecordSet
	err := s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error { var e error; result, e = tx.Object(typeID, id, nil); return e })
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func identity(t *testing.T, reply store.Reply) string {
	t.Helper()
	var r p.TransactionResult
	if err := json.Unmarshal(reply.Body, &r); err != nil || len(r.Results) != 1 || len(r.Results[0].Objects) != 1 {
		t.Fatal("invalid insert response", err, string(reply.Body))
	}
	return r.Results[0].Objects[0].ID
}
func count(t *testing.T, s *Store, dataset string) int {
	t.Helper()
	var n int
	err := s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error { rows, e := tx.Matching(typeID, nil); n = len(rows); return e })
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRestartAndByteExactRecovery(t *testing.T) {
	s, path, dataset := fixture(t)
	m := mutation(t, dataset, key(1), insertBody)
	reply, err := s.Mutate(t.Context(), owner, m)
	if err != nil || reply.Status != 200 {
		t.Fatal(reply, err)
	}
	id := identity(t, reply)
	before := record(t, s, dataset, id)
	info, _ := s.Info(t.Context(), owner, sole(t, s))
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(t.Context(), path, p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	after := record(t, s, dataset, id)
	afterInfo, _ := s.Info(t.Context(), owner, sole(t, s))
	if before.Records[0].Version != after.Records[0].Version || info.ID != afterInfo.ID || info.Watermark != afterInfo.Watermark || info.SchemaVersion != afterInfo.SchemaVersion {
		t.Fatal("restart changed persistent identity or versions")
	}
	if got := after.Records[0].Values[0].Payload().(int64); got != 9007199254740993 {
		t.Fatal("integer rounded")
	}
	if string(after.Meta.Annotations["opaque"]) != `{ "z": 1e2, "a": [ true, false ] }` {
		t.Fatal("annotation rewritten")
	}
	replay, err := s.Mutate(t.Context(), owner, m)
	if err != nil || !replay.Replayed || string(replay.Body) != string(reply.Body) || count(t, s, dataset) != 1 {
		t.Fatal("write reexecuted", err)
	}
	m.Digest = p.RequestDigest("POST", "/v1/datasets/"+dataset+":transact", []byte(insertBody+" "))
	if _, err = s.Mutate(t.Context(), owner, m); p.ErrorCode(err) != "conflict" {
		t.Fatal("changed bytes reused key", err)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0600 {
		t.Fatal("database not private")
	}
}
func TestReceiptOwnershipSurvivesRevocationAndDeletion(t *testing.T) {
	for _, remove := range []string{"grant", "record"} {
		t.Run(remove, func(t *testing.T) {
			s, _, dataset := fixture(t)
			m := mutation(t, dataset, key(2), insertBody)
			reply, err := s.Mutate(t.Context(), owner, m)
			if err != nil {
				t.Fatal(err)
			}
			id := identity(t, reply)
			other := store.Access{Principal: owner.Principal, Authorization: "other-app"}
			if err = s.SetAuthorization(t.Context(), sole(t, s), other, store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true}, View: store.FullView()}); err != nil {
				t.Fatal(err)
			}
			if _, err = s.Status(t.Context(), other, dataset, m.Key); p.ErrorCode(err) != "permission_denied" {
				t.Fatal("non-owner recovered receipt", err)
			}
			if remove == "grant" {
				err = s.Revoke(t.Context(), sole(t, s), owner)
			} else {
				err = s.Transact(t.Context(), owner, sole(t, s), func(tx store.Tx) error { return tx.Delete(typeID, id) })
			}
			if err != nil {
				t.Fatal(err)
			}
			status, err := s.Status(t.Context(), owner, dataset, m.Key)
			if err != nil || status.State != "committed" || !status.ResultWithheld || len(status.Result) != 0 || len(status.Objects) != 1 || status.Objects[0].ObjectID != id {
				t.Fatal("identity-only recovery failed", err, status)
			}
			replay, found, err := s.Replay(t.Context(), owner, dataset, m.Key, m.Digest)
			if err != nil || !found || !replay.Replayed || !strings.Contains(string(replay.Body), `"result_withheld":true`) {
				t.Fatal("revoked replay failed", err)
			}
			unknown, err := s.Status(t.Context(), store.Access{Principal: "no-grants", Authorization: "app"}, dataset, key(999))
			if err != nil || unknown.State != "unknown" {
				t.Fatal("unknown required grant", err)
			}
		})
	}
}
func TestInProgressAndConcurrentDeduplication(t *testing.T) {
	s, _, dataset := fixture(t)
	m := mutation(t, dataset, key(3), insertBody)
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		_, err := s.MutateWith(context.Background(), owner, m, func(store.Tx, *sql.Tx) error { close(started); <-release; return nil })
		done <- err
	}()
	<-started
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	status, err := s.Status(ctx, owner, dataset, m.Key)
	if err != nil || status.State != "in_progress" {
		close(release)
		t.Fatal("claim not visible during execution", err)
	}
	_, found, err := s.Replay(ctx, owner, dataset, m.Key, m.Digest)
	if !found || p.ErrorCode(err) != "conflict" {
		close(release)
		t.Fatal("concurrent request not rejected", err)
	}
	close(release)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := s.Mutate(context.Background(), owner, m)
			if e == nil && !r.Replayed {
				e = errors.New("not a replay")
			}
			errs <- e
		}()
	}
	wg.Wait()
	close(errs)
	for err = range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if count(t, s, dataset) != 1 {
		t.Fatal("duplicate records")
	}
}
func TestHostCompositionRollbackAndFailedReplay(t *testing.T) {
	s, _, dataset := fixture(t)
	err := s.WithTx(t.Context(), owner, sole(t, s), func(_ store.Tx, tx *sql.Tx) error {
		_, e := tx.ExecContext(t.Context(), `CREATE TABLE "host_events" ("kind" TEXT NOT NULL)`)
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, fail := range []bool{false, true} {
		m := mutation(t, dataset, key(10+i), insertBody)
		reply, err := s.MutateWith(t.Context(), owner, m, func(_ store.Tx, tx *sql.Tx) error {
			_, e := tx.ExecContext(t.Context(), `INSERT INTO "host_events" ("kind") VALUES ('synthetic')`)
			if e != nil {
				return e
			}
			if fail {
				return store.Refuse("conflict")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if fail && reply.Status != 409 || !fail && reply.Status != 200 {
			t.Fatal("wrong outcome", reply.Status)
		}
		replay, err := s.Mutate(t.Context(), owner, m)
		if err != nil || !replay.Replayed || string(replay.Body) != string(reply.Body) {
			t.Fatal("outcome not retained", err)
		}
	}
	var events int
	if err = s.db.QueryRow(`SELECT count(*) FROM "host_events"`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 || count(t, s, dataset) != 1 {
		t.Fatal("host failure partially committed")
	}
	info, _ := s.Info(t.Context(), owner, sole(t, s))
	// A late check rolls back earlier mutation and preserves the watermark.
	body := `{"operations":[{"op":"delete_where","type":"test.record"},{"op":"expect","target":{"type":"test.record","id":"missing"},"version":"stale"}]}`
	reply, err := s.Mutate(t.Context(), owner, mutation(t, dataset, key(15), body))
	if err != nil || reply.Status != 404 {
		t.Fatal(err, reply)
	}
	after, _ := s.Info(t.Context(), owner, sole(t, s))
	if count(t, s, dataset) != 1 || info.Watermark != after.Watermark {
		t.Fatal("failed transaction changed state")
	}
}
func TestClaimsAuthorizeBeforeExecutionAndRetention(t *testing.T) {
	s, _, dataset := fixture(t)
	schema := store.Access{Principal: "schema", Authorization: "only"}
	if err := s.SetAuthorization(t.Context(), sole(t, s), schema, store.Authorization{Grants: store.Grants{Schema: true}, View: store.FullView()}); err != nil {
		t.Fatal(err)
	}
	m := mutation(t, dataset, key(20), insertBody)
	if _, err := s.Mutate(t.Context(), schema, m); p.ErrorCode(err) != "permission_denied" {
		t.Fatal(err)
	}
	status, err := s.Status(t.Context(), schema, dataset, m.Key)
	if err != nil || status.State != "unknown" {
		t.Fatal("denied request claimed key", err)
	}
	m = mutation(t, dataset, key(21), `{"operations":[{"op":"define_type","meta":{"id":"test.schema_only","fields":[]}}]}`)
	if _, err = s.Mutate(t.Context(), schema, m); err != nil {
		t.Fatal(err)
	}
	status, err = s.Status(t.Context(), schema, dataset, m.Key)
	if err != nil || status.State != "committed" || status.ResultWithheld || len(status.Result) == 0 {
		t.Fatal("schema-only writer cannot reconcile", err)
	}
	inflight := mutation(t, dataset, key(22), insertBody)
	if _, _, err = s.claim(t.Context(), owner, inflight); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE "tessera_receipts" SET "claimed_at"=0`); err != nil {
		t.Fatal(err)
	}
	if err = s.PruneReceipts(t.Context()); err != nil {
		t.Fatal(err)
	}
	status, _ = s.Status(t.Context(), schema, dataset, m.Key)
	if status.State != "unknown" {
		t.Fatal("expired receipt retained by sweep")
	}
	status, _ = s.Status(t.Context(), owner, dataset, inflight.Key)
	if status.State != "in_progress" {
		t.Fatal("unresolved claim swept")
	}
}
func TestCrashChild(t *testing.T) {
	path := os.Getenv("TESSERA_TEST_CRASH_PATH")
	if path == "" {
		t.Skip("subprocess helper")
	}
	s, err := Open(context.Background(), path, p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.Info(context.Background(), owner, sole(t, s))
	if err != nil {
		t.Fatal(err)
	}
	m := mutation(t, info.ID, key(30), insertBody)
	_, err = s.MutateWith(context.Background(), owner, m, func(_ store.Tx, tx *sql.Tx) error {
		if _, e := tx.Exec(`CREATE TABLE "uncommitted_host" ("id" INTEGER)`); e != nil {
			return e
		}
		if os.Getenv("TESSERA_TEST_CRASH_PHASE") == "before_commit" {
			os.Exit(77)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	os.Exit(77)
}
func TestCrashRecoveryBeforeAndAfterCommit(t *testing.T) {
	for _, phase := range []string{"before_commit", "after_commit"} {
		t.Run(phase, func(t *testing.T) {
			s, path, dataset := fixture(t)
			s.Close()
			child := exec.Command(os.Args[0], "-test.run=^TestCrashChild$")
			child.Env = append(os.Environ(), "TESSERA_TEST_CRASH_PATH="+path, "TESSERA_TEST_CRASH_PHASE="+phase)
			err := child.Run()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 77 {
				t.Fatal("child did not reach crash point", err)
			}
			s, err = Open(t.Context(), path, p.DevelopmentLimits())
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			status, err := s.Status(t.Context(), owner, dataset, key(30))
			if err != nil {
				t.Fatal(err)
			}
			wantState := "committed"
			wantCount := 1
			if phase == "before_commit" {
				wantState = "failed"
				wantCount = 0
			}
			if status.State != wantState || count(t, s, dataset) != wantCount {
				t.Fatal("crash recovered wrong outcome", status.State)
			}
			m := mutation(t, dataset, key(30), insertBody)
			reply, err := s.Mutate(t.Context(), owner, m)
			if err != nil || !reply.Replayed || count(t, s, dataset) != wantCount {
				t.Fatal("crash retry reexecuted", err)
			}
		})
	}
}
func TestExclusiveProcessOwnership(t *testing.T) {
	s, path, _ := fixture(t)
	if other, err := Open(t.Context(), path, p.DevelopmentLimits()); err == nil {
		other.Close()
		t.Fatal("second process owner admitted")
	}
	if _, err := s.Info(t.Context(), owner, sole(t, s)); err != nil {
		t.Fatal("failed second open harmed owner", err)
	}
}

func TestCancelledHostTransactionDoesNotCommit(t *testing.T) {
	s, _, dataset := fixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	m := mutation(t, dataset, key(40), insertBody)
	reply, err := s.MutateWith(ctx, owner, m, func(store.Tx, *sql.Tx) error { cancel(); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if reply.Status != 500 || count(t, s, dataset) != 0 {
		t.Fatal("cancelled write committed")
	}
	status, err := s.Status(t.Context(), owner, dataset, m.Key)
	if err != nil || status.State != "failed" {
		t.Fatal("cancellation not reconciled", err)
	}
}
func TestConcurrentFreshClaim(t *testing.T) {
	s, _, dataset := fixture(t)
	m := mutation(t, dataset, key(41), insertBody)
	start := make(chan struct{})
	results := make(chan error, 20)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := s.Mutate(context.Background(), owner, m)
			if err != nil && p.ErrorCode(err) != "conflict" {
				results <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		t.Fatal(err)
	}
	if count(t, s, dataset) != 1 {
		t.Fatal("fresh key executed more than once")
	}
}
func TestMaterializationBoundRollsBack(t *testing.T) {
	s, _, dataset := fixture(t)
	// Bounds belong to the dataset and are read from its row, so tightening one
	// means writing it there rather than reaching into the store.
	tighten(t, s, dataset, func(limits *p.Limits) { limits.MaxScannedRows = 1 })
	if _, err := s.Mutate(t.Context(), owner, mutation(t, dataset, key(42), insertBody)); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Info(t.Context(), owner, sole(t, s))
	reply, err := s.Mutate(t.Context(), owner, mutation(t, dataset, key(43), insertBody))
	if err != nil || reply.Status != 422 {
		t.Fatal("row bound not enforced", err, reply.Status)
	}
	after, _ := s.Info(t.Context(), owner, sole(t, s))
	if count(t, s, dataset) != 1 || before.Watermark != after.Watermark {
		t.Fatal("oversized transaction partially committed")
	}
}

// An unknown status is not a verdict. A request delayed in the network can
// still arrive and commit afterwards, so a caller that reads unknown and
// concludes the write failed will duplicate it. Resolved as review finding 6.
func TestUnknownStatusDoesNotPreventALaterCommit(t *testing.T) {
	s, _, dataset := fixture(t)
	m := mutation(t, dataset, key(700), insertBody)
	status, err := s.Status(t.Context(), owner, dataset, m.Key)
	if err != nil || status.State != "unknown" {
		t.Fatal("an unclaimed key was not unknown", status.State, err)
	}
	if status.Watermark != "" {
		t.Fatal("unknown carried a dataset observation")
	}
	// The delayed request now arrives. Observing unknown must not have claimed
	// or poisoned the key.
	reply, err := s.Mutate(t.Context(), owner, m)
	if err != nil {
		t.Fatal("a request delayed past an unknown status could not commit", err)
	}
	created := identity(t, reply)
	after, err := s.Status(t.Context(), owner, dataset, m.Key)
	if err != nil || after.State != "committed" {
		t.Fatal("the committed outcome was not retained", after.State, err)
	}
	// And the ordinary replay rule still holds afterwards.
	replay, err := s.Mutate(t.Context(), owner, m)
	if err != nil || identity(t, replay) != created {
		t.Fatal("replay after a late commit created a second record", err)
	}
}

// Host rows and core rows commit together or not at all. A host failure must
// leave no record, and no receipt claiming one.
func TestHostRollbackLeavesNoCommittedOutcome(t *testing.T) {
	s, _, dataset := fixture(t)
	if err := s.WithTx(t.Context(), owner, sole(t, s), func(_ store.Tx, host *sql.Tx) error {
		_, e := host.ExecContext(t.Context(), `CREATE TABLE IF NOT EXISTS "host_rows" ("id" TEXT PRIMARY KEY)`)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := s.Transact(t.Context(), owner, sole(t, s), func(tx store.Tx) error {
		rows, e := tx.Matching(typeID, nil)
		before = len(rows)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	m := mutation(t, dataset, key(701), insertBody)
	refusal := errors.New("the host declined this unit of work")
	// A definitive failure is a reply and a retained receipt rather than a Go
	// error: that is what makes the outcome recoverable by a caller whose
	// response was lost.
	reply, err := s.MutateWith(t.Context(), owner, m, func(_ store.Tx, host *sql.Tx) error {
		if _, e := host.ExecContext(t.Context(), `INSERT INTO "host_rows" ("id") VALUES ('doomed')`); e != nil {
			return e
		}
		return refusal
	})
	if err != nil {
		t.Fatal(err)
	}
	if reply.Status < 400 {
		t.Fatal("a host failure reported success", reply.Status)
	}
	// Neither the record nor the host row survived.
	var after, hostRows int
	if err = s.Transact(t.Context(), owner, sole(t, s), func(tx store.Tx) error {
		rows, e := tx.Matching(typeID, nil)
		after = len(rows)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatal("a rolled-back host callback left a record behind")
	}
	if err = s.WithTx(t.Context(), owner, sole(t, s), func(_ store.Tx, host *sql.Tx) error {
		return host.QueryRowContext(t.Context(), `SELECT count(*) FROM "host_rows"`).Scan(&hostRows)
	}); err != nil {
		t.Fatal(err)
	}
	if hostRows != 0 {
		t.Fatal("a rolled-back host callback left its own row behind")
	}
	// The key is answerable, and never as a committed outcome that did not
	// happen. A definitive failure may be retained; success may not.
	status, err := s.Status(t.Context(), owner, dataset, m.Key)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "failed" {
		t.Fatal("a rolled-back transaction did not retain a definitive failure", status.State)
	}
	// Replaying the same key returns that failure rather than trying again,
	// so a lost response cannot become a second attempt at the same work.
	again, err := s.Mutate(t.Context(), owner, m)
	if err != nil || again.Status < 400 {
		t.Fatal("replaying a failed key did not return the retained failure", err)
	}
	if err = s.Transact(t.Context(), owner, sole(t, s), func(tx store.Tx) error {
		rows, e := tx.Matching(typeID, nil)
		after = len(rows)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatal("replaying a failed key created the record after all")
	}
}
