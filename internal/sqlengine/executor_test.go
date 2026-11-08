package sqlengine

import (
	"context"
	"database/sql"
	"github.com/ekarpov/tessera/internal/dbtest"
	"path/filepath"
	"testing"
	"time"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// postgresFixture opens a store on a disposable database, emptied of this
// engine's tables first so each test starts from nothing. Two of these are two
// executors: each has its own connections, and so its own PostgreSQL session
// and its own advisory locks, which is what a second process is from the
// database's side.
func postgresFixture(t *testing.T) *Store {
	t.Helper()
	s, err := openPostgres(t.Context(), dbtest.Postgres(t), filepath.Join(t.TempDir(), "fence"), p.DevelopmentLimits(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// postgresTestKey excludes the other package's PostgreSQL tests from this one's
// database. Both empty it of this engine's tables to start from nothing, and
// `go test ./...` runs packages at the same time, so without this they take
// each other's tables away mid-test. An advisory lock rather than a file lock,
// because what is being shared is the database.
const postgresTestKey int64 = 0x7e57000000000001

func postgresAdmin(t *testing.T) *sql.DB {
	t.Helper()
	dsn := dbtest.Postgres(t)
	admin, err := sql.Open("tessera-postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := admin.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = guard.ExecContext(context.Background(), `SELECT pg_advisory_lock($1)`, postgresTestKey); err != nil {
		t.Fatal(err)
	}
	// Registered before the reset, so they run after it.
	t.Cleanup(func() { admin.Close() })
	t.Cleanup(func() {
		guard.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, postgresTestKey)
		guard.Close()
	})
	reset := func() {
		if _, e := admin.ExecContext(context.Background(), `DO $$ DECLARE t record; BEGIN FOR t IN SELECT tablename FROM pg_tables WHERE schemaname='public' AND tablename LIKE 'tessera\_%' LOOP EXECUTE format('DROP TABLE IF EXISTS %I CASCADE', t.tablename); END LOOP; END $$`); e != nil {
			t.Fatal(e)
		}
	}
	t.Cleanup(reset)
	reset()
	return admin
}

// claimOf writes an in-progress claim attributed to one executor, which is what
// a process mid-transaction has left behind and what another process has to
// decide about without being able to see that process.
func claimOf(t *testing.T, admin *sql.DB, dataset, key, executor string) {
	t.Helper()
	_, err := admin.ExecContext(t.Context(), `INSERT INTO "tessera_receipts" ("key","dataset_id","principal","authorization","digest","claimed_at","state","watermark","replay_scope","executor_id") VALUES ($1,$2,'synthetic','owner',$3,$4,'in_progress','w:seeded','read',$5)`, key, dataset, make([]byte, 32), time.Now().UnixMilli(), executor)
	if err != nil {
		t.Fatal(err)
	}
}

func claimState(t *testing.T, admin *sql.DB, dataset, key string) string {
	t.Helper()
	var state string
	if err := admin.QueryRowContext(t.Context(), `SELECT "state" FROM "tessera_receipts" WHERE "dataset_id"=$1 AND "key"=$2`, dataset, key).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

// Startup used to resolve every in-progress claim as abandoned, which is only
// safe while one process can write. Two executors now share a database, and
// neither may decide that the other's work in flight has failed — a claim is
// abandoned when its executor's session is gone, which is answered by taking
// that executor's lock rather than by waiting out a clock.
func TestExecutorsDoNotResolveEachOthersLiveClaims(t *testing.T) {
	admin := postgresAdmin(t)
	first := postgresFixture(t)
	dataset, err := first.SoleDataset(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	claimOf(t, admin, dataset.ID, key(800), first.executor)

	// A second executor opens the same database, which a process of the
	// previous kind could not do at all.
	second := postgresFixture(t)
	if second.executor == first.executor {
		t.Fatal("two executors minted one identity")
	}
	if state := claimState(t, admin, dataset.ID, key(800)); state != "in_progress" {
		t.Fatal("a starting executor resolved a live executor's claim", state)
	}
	claimOf(t, admin, dataset.ID, key(801), second.executor)

	// Neither resolves the other while both are running, and neither resolves
	// its own: its own claims are the ones it is working on.
	for _, s := range []*Store{first, second} {
		if err = s.ResolveAbandonedClaims(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	for _, k := range []string{key(800), key(801)} {
		if state := claimState(t, admin, dataset.ID, k); state != "in_progress" {
			t.Fatal("a live claim was resolved", k, state)
		}
	}

	// The first executor stops. Its session ends, and with it the lock that
	// said it was still there.
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	// The server ends a closed session's lock a moment after the client goes,
	// and until it has, the executor still looks alive — so resolution is
	// asked for again, as the maintenance worker would, until it lands.
	state := ""
	for deadline := time.Now().Add(10 * time.Second); state != "failed" && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if err = second.ResolveAbandonedClaims(t.Context()); err != nil {
			t.Fatal(err)
		}
		state = claimState(t, admin, dataset.ID, key(800))
	}
	if state != "failed" {
		t.Fatal("a departed executor's claim was left unresolved", state)
	}
	if state := claimState(t, admin, dataset.ID, key(801)); state != "in_progress" {
		t.Fatal("the surviving executor resolved its own live claim", state)
	}
}

// A claim recording no executor was made before executors were recorded. A
// starting executor proves no process of that kind is running, by taking the
// key such a process holds, and only then resolves those claims.
func TestClaimsFromBeforeExecutorsAreResolvedAtStartup(t *testing.T) {
	admin := postgresAdmin(t)
	first := postgresFixture(t)
	dataset, err := first.SoleDataset(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	claimOf(t, admin, dataset.ID, key(810), "")
	claimOf(t, admin, dataset.ID, key(811), first.executor)

	postgresFixture(t)
	if state := claimState(t, admin, dataset.ID, key(810)); state != "failed" {
		t.Fatal("a claim from before executors was left unresolved", state)
	}
	if state := claimState(t, admin, dataset.ID, key(811)); state != "in_progress" {
		t.Fatal("a live executor's claim was resolved alongside it", state)
	}
}

// A process of the previous kind resolves every in-progress claim as abandoned,
// including ones this process would be working on, so this one does not start
// beside it. The key it holds is what says it is there.
func TestAProcessOfThePreviousKindKeepsThisOneOut(t *testing.T) {
	postgresAdmin(t)
	held := postgresFixture(t)
	previous, err := held.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer previous.Close()
	var taken bool
	if err = previous.QueryRowContext(t.Context(), `SELECT pg_try_advisory_lock($1)`, legacyProcessKey).Scan(&taken); err != nil {
		t.Fatal(err)
	}
	if !taken {
		t.Fatal("the key a previous process would hold was already held")
	}
	blocked, err := openPostgres(t.Context(), dbtest.Postgres(t), filepath.Join(t.TempDir(), "fence"), p.DevelopmentLimits(), false)
	if err == nil {
		blocked.Close()
		t.Fatal("started beside a process that would resolve this one's claims")
	}
	if p.ErrorCode(err) != "dataset_unavailable" {
		t.Fatal(err)
	}
	// Once it lets go, the next executor starts, and holds no such key itself:
	// two executors of this kind are meant to run together.
	if _, err = previous.ExecContext(t.Context(), `SELECT pg_advisory_unlock($1)`, legacyProcessKey); err != nil {
		t.Fatal(err)
	}
	next := postgresFixture(t)
	if next.executor == held.executor {
		t.Fatal("two executors minted one identity")
	}
	third := postgresFixture(t)
	if third.executor == next.executor {
		t.Fatal("two executors minted one identity")
	}
}

// Two executors write the same dataset. The work commits once, under whichever
// executor claimed the key, and the other is told the key is taken.
func TestTwoExecutorsShareOneDataset(t *testing.T) {
	admin := postgresAdmin(t)
	first := postgresFixture(t)
	second := postgresFixture(t)
	dataset, err := first.SoleDataset(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	full := store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true}, View: store.FullView()}
	if err = first.SetAuthorization(t.Context(), dataset.ID, owner, full); err != nil {
		t.Fatal(err)
	}
	if err = first.Transact(t.Context(), owner, dataset.ID, func(tx store.Tx) error {
		_, e := tx.CreateMetadata(p.Meta{ID: typeID, Fields: []p.Field{{ID: "n", MetaID: typeID, Type: p.IntegerType}}})
		return e
	}); err != nil {
		t.Fatal(err)
	}
	// Both executors send the same keyed mutation. One executes it and the
	// others replay, whichever order they arrive in.
	for _, s := range []*Store{first, second, first, second} {
		if _, err = s.Mutate(t.Context(), owner, mutation(t, dataset.ID, key(820), insertBody)); err != nil {
			t.Fatal(err)
		}
	}
	var rows int
	if err = admin.QueryRowContext(t.Context(), `SELECT count(*) FROM "tessera_objects" WHERE "dataset_id"=$1`, dataset.ID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatal("one idempotency key produced more than one record", rows)
	}
	// A key claimed by one executor is not re-executed by the other: it replays.
	reply, err := second.Mutate(t.Context(), owner, mutation(t, dataset.ID, key(820), insertBody))
	if err != nil {
		t.Fatal(err)
	}
	if !reply.Replayed {
		t.Fatal("the other executor executed a key that was already resolved")
	}
}
