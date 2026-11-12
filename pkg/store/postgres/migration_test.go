package postgres_test

import (
	"context"
	"database/sql"
	"github.com/ekarpov/tessera/internal/dbtest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
	"github.com/ekarpov/tessera/pkg/store/postgres"
)

// Model the preceding PostgreSQL schema: no dataset column on any record table,
// the original keys, the object-to-type foreign key over the type alone, and a
// dataset row held to one by its singleton. Every column the migration adds has
// to go, or reopening re-adds one that already exists. The names are the ones
// the forward migration drops by name, so they have to be restored exactly.
var singletonSchema = []string{

	`DROP TABLE "tessera_contributions"`, `DROP TABLE "tessera_contribution_erasure"`,
	`DROP VIEW "tessera_read_objects"`,
	`DROP VIEW "tessera_all_field_values"`,
	`DROP TABLE "tessera_managed_installations"`,
	`DROP TABLE "tessera_managed_layouts"`,
	`DROP TABLE "tessera_field_values"`,
	`ALTER TABLE "tessera_objects" DROP COLUMN "wire_bytes"`,
	`ALTER TABLE "tessera_dataset" DROP COLUMN "managed_key"`,
	`DROP TRIGGER "tessera_schema_cache_changed" ON "tessera_types"`,
	`DROP FUNCTION "tessera_invalidate_schema_cache"()`,
	`ALTER TABLE "tessera_objects" DROP CONSTRAINT "tessera_objects_pkey"`,
	`ALTER TABLE "tessera_objects" ADD PRIMARY KEY ("object_id")`,
	`DROP INDEX "tessera_objects_type"`,
	// Dropping the column takes the composite foreign key with it, which is what
	// frees the type table's key to be rebuilt.
	`ALTER TABLE "tessera_objects" DROP COLUMN "dataset_id"`,
	`CREATE INDEX "tessera_objects_type" ON "tessera_objects" ("type_id")`,
	`ALTER TABLE "tessera_types" DROP CONSTRAINT "tessera_types_pkey"`,
	`ALTER TABLE "tessera_types" ADD PRIMARY KEY ("type_id")`,
	`ALTER TABLE "tessera_types" DROP COLUMN "dataset_id"`,
	`ALTER TABLE "tessera_objects" ADD CONSTRAINT "tessera_objects_type_id_fkey" FOREIGN KEY ("type_id") REFERENCES "tessera_types"("type_id") DEFERRABLE INITIALLY DEFERRED`,
	`ALTER TABLE "tessera_access" DROP CONSTRAINT "tessera_access_pkey"`,
	`ALTER TABLE "tessera_access" ADD PRIMARY KEY ("principal","authorization")`,
	`ALTER TABLE "tessera_access" DROP COLUMN "dataset_id"`,
	`ALTER TABLE "tessera_dataset" DROP CONSTRAINT "tessera_dataset_pkey"`,
	`ALTER TABLE "tessera_dataset" ADD COLUMN "singleton" INTEGER`,
	`UPDATE "tessera_dataset" SET "singleton"=1`,
	`ALTER TABLE "tessera_dataset" ALTER COLUMN "singleton" SET NOT NULL`,
	`ALTER TABLE "tessera_dataset" ADD PRIMARY KEY ("singleton")`,
	`ALTER TABLE "tessera_dataset" ADD CONSTRAINT "tessera_dataset_singleton_check" CHECK ("singleton" = 1)`,
	`ALTER TABLE "tessera_dataset" DROP COLUMN "display_name"`,
	`DROP INDEX "tessera_receipts_executor"`,
	`DROP INDEX "tessera_receipts_retention_deadline"`,
	`ALTER TABLE "tessera_receipts" DROP COLUMN "retain_until"`,
	`ALTER TABLE "tessera_receipts" DROP COLUMN "executor_id"`,
	`ALTER TABLE "tessera_dataset" DROP COLUMN "object_count"`,
	`ALTER TABLE "tessera_dataset" DROP COLUMN "object_bytes"`,
	`ALTER TABLE "tessera_dataset" DROP COLUMN "schema_cache_key"`,
	`ALTER TABLE "tessera_sequences" DROP COLUMN "meta"`,
	`DROP INDEX "tessera_imports_uncleaned"`,
	`ALTER TABLE "tessera_imports" DROP COLUMN "cleaned"`,
	`DROP TABLE "tessera_page_rows"`,
	`ALTER TABLE "tessera_pages" DROP COLUMN "position"`,
	`ALTER TABLE "tessera_dataset" DROP COLUMN "creator_secret"`,
	`UPDATE "tessera_migration" SET "version"=4`,
}

// disposable opens an administrative connection and empties the database of the
// engine's own tables, before the test and again after it. These tests model
// schemas the engine does not otherwise produce, so each one starts from
// nothing and leaves nothing: whatever runs next sees the disposable database
// the environment variable promises. Tables that are not the engine's are left
// alone.
// postgresTestKey excludes the engine package's PostgreSQL tests from this
// one's database. Both empty it of this engine's tables to start from nothing,
// and `go test ./...` runs packages at the same time, so without this they take
// each other's tables away mid-test. An advisory lock rather than a file lock,
// because what is being shared is the database.
const postgresTestKey int64 = 0x7e57000000000001

func disposable(t *testing.T, dsn string) *sql.DB {
	t.Helper()
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
	t.Cleanup(func() {
		guard.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, postgresTestKey)
		guard.Close()
	})
	// Not t.Context(): the cleanup runs after the test's context is cancelled.
	reset := func() { dropTessera(t, admin) }
	// Registered before the reset, so it runs after it.
	t.Cleanup(func() { admin.Close() })
	t.Cleanup(reset)
	reset()
	return admin
}

// PostgreSQL alters the record tables in place where SQLite rebuilds them, so
// the statements that carry existing rows onto the new keys are this engine's
// alone. An empty database migrates either way; this is the only check that the
// rows of a database in use arrive on the other side.
func TestPostgresSingletonSchemaMigratesWithItsRecords(t *testing.T) {
	dsn := dbtest.Postgres(t)
	admin := disposable(t, dsn)
	owner := store.Access{Principal: "synthetic", Authorization: "owner"}
	const typeID = "test.migrated"
	fence := filepath.Join(t.TempDir(), "retirement-fence")

	s, err := postgres.Open(t.Context(), dsn, fence, p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetAuthorization(t.Context(), sole(t, s), owner, store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true}, View: store.FullView()}); err != nil {
		t.Fatal(err)
	}
	amount, err := p.NewValue(p.IntegerType, int64(9007199254740993))
	if err != nil {
		t.Fatal(err)
	}
	listed, err := s.Datasets(t.Context(), owner)
	if err != nil || len(listed) != 1 {
		t.Fatal("expected exactly one dataset", err)
	}
	dataset := listed[0].ID
	var objectID string
	if err = s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
		if _, e := tx.CreateMetadata(p.Meta{ID: typeID, Fields: []p.Field{{ID: "n", MetaID: typeID, Type: p.IntegerType}}}); e != nil {
			return e
		}
		set, e := tx.Insert(typeID, map[string]p.Value{"n": amount})
		if e != nil {
			return e
		}
		objectID = set.Records[0].ID.ID
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, err := s.Info(t.Context(), owner, dataset)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := p.MarshalWire(map[string]p.Value{"n": amount})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = admin.ExecContext(t.Context(), `ALTER TABLE "tessera_objects" ADD COLUMN "fields" BYTEA NOT NULL DEFAULT convert_to('{}','UTF8')`); err != nil {
		t.Fatal(err)
	}
	if _, err = admin.ExecContext(t.Context(), `UPDATE "tessera_objects" SET "fields"=$1`, payload); err != nil {
		t.Fatal(err)
	}
	for _, statement := range singletonSchema {
		if _, err = admin.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(statement, err)
		}
	}
	// The session advisory lock is held until this store lets go of it.
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = postgres.Open(t.Context(), dsn, fence, p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if e := s.Close(); e != nil {
			t.Error(e)
		}
	}()
	after, err := s.Info(t.Context(), owner, dataset)
	if err != nil || after.ID != before.ID || after.Watermark != before.Watermark {
		t.Fatal("migration changed identity or watermark", after, err)
	}
	// A database written before datasets carried a name answers what it always
	// answered, rather than arriving nameless.
	if after.DisplayName != "Tessera dataset" {
		t.Fatal("migration left the dataset nameless", after.DisplayName)
	}
	var carried p.RecordSet
	if err = s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
		var e error
		carried, e = tx.Object(typeID, objectID, nil)
		return e
	}); err != nil {
		t.Fatal("migration lost the record", err)
	}
	if len(carried.Records) != 1 || carried.Records[0].Values[0].Payload().(int64) != 9007199254740993 {
		t.Fatal("migration altered the record")
	}
	// The carried rows belong to the dataset that held them, not to no dataset.
	var types, objects, access int
	if err = admin.QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM "tessera_types" WHERE "dataset_id"=$1),(SELECT count(*) FROM "tessera_objects" WHERE "dataset_id"=$1),(SELECT count(*) FROM "tessera_access" WHERE "dataset_id"=$1)`, before.ID).Scan(&types, &objects, &access); err != nil {
		t.Fatal(err)
	}
	if types != 1 || objects != 1 || access != 1 {
		t.Fatal("migration did not key the carried rows", types, objects, access)
	}
	// And the dataset counts what it carried, from the migration that began
	// keeping the count.
	var counted int
	if err = admin.QueryRowContext(t.Context(), `SELECT "object_count" FROM "tessera_dataset" WHERE "dataset_id"=$1`, before.ID).Scan(&counted); err != nil || counted != 1 {
		t.Fatal("migration did not count the carried records", counted, err)
	}
	// The rebuilt keys and the rebuilt object-to-type foreign key take ordinary
	// work: a second row on the composite key, and a type the key must refuse.
	if err = s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
		_, e := tx.Insert(typeID, map[string]p.Value{"n": amount})
		return e
	}); err != nil {
		t.Fatal("post-migration write refused", err)
	}
	if err = s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
		_, e := tx.Insert("test.undeclared", map[string]p.Value{"n": amount})
		return e
	}); p.ErrorCode(err) != "meta_type_not_found" {
		t.Fatal("an undeclared type was accepted", err)
	}
	var rows int
	if err = admin.QueryRowContext(t.Context(), `SELECT count(*) FROM "tessera_objects" WHERE "dataset_id"=$1`, before.ID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatal("post-migration write lost", rows)
	}
}

// A grant belongs to one dataset. Seeded directly, because nothing provisions a
// second dataset yet, and checked on this engine because the predicate that
// scopes it is SQL rather than Go.
func TestPostgresNeighbouringGrantIsRefused(t *testing.T) {
	dsn := dbtest.Postgres(t)
	admin := disposable(t, dsn)
	s, err := postgres.Open(t.Context(), dsn, filepath.Join(t.TempDir(), "retirement-fence"), p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	intruder := store.Access{Principal: "intruder", Authorization: "intruder"}
	if _, err = admin.ExecContext(t.Context(), `INSERT INTO "tessera_access" ("dataset_id","principal","authorization","grants","generation","view") VALUES ('d-neighbour','intruder','intruder',$1,'g-neighbour',$2) `, []byte(`{"read":true,"write":true,"schema":true,"erase":true}`), []byte(`{"full":true}`)); err != nil {
		t.Fatal(err)
	}
	// Nothing is authorized on this store, so its dataset is named from the
	// database rather than from a listing the intruder must not be able to read.
	var dataset string
	if err = admin.QueryRowContext(t.Context(), `SELECT "dataset_id" FROM "tessera_dataset"`).Scan(&dataset); err != nil {
		t.Fatal(err)
	}
	if listed, e := s.Datasets(t.Context(), intruder); e != nil || len(listed) != 0 {
		t.Fatal("a neighbouring grant put a dataset in the listing", listed, e)
	}
	// A grant on the neighbour is no grant here, so this dataset is concealed
	// rather than refused: permission_denied would confirm it exists, which is
	// what the empty listing above already declined to say.
	if _, err = s.Info(t.Context(), intruder, dataset); p.ErrorCode(err) != "unavailable" {
		t.Fatal("neighbouring grant admitted", err)
	}
	if err = s.Transact(t.Context(), intruder, dataset, func(store.Tx) error { return nil }); p.ErrorCode(err) != "unavailable" {
		t.Fatal("neighbouring grant admitted", err)
	}
}

// Two datasets in one PostgreSQL database, written independently. The engine
// code is shared, but the composite keys, the deferred composite foreign key
// and the on-conflict targets are this dialect's, and only a real server runs
// them.
func TestPostgresHoldsTwoDatasets(t *testing.T) {
	dsn := dbtest.Postgres(t)
	disposable(t, dsn)
	s, err := postgres.Open(t.Context(), dsn, filepath.Join(t.TempDir(), "retirement-fence"), p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const typeID = "test.shared"
	first, err := s.SoleDataset(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	one := store.Access{Principal: "synthetic", Authorization: "one"}
	two := store.Access{Principal: "synthetic", Authorization: "two"}
	full := store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true, Erase: true}, View: store.FullView()}
	if err = s.SetAuthorization(t.Context(), first.ID, one, full); err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateDataset(t.Context(), two, "Second quarter", p.Limits{}, nil, "", [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if second.DatasetID == first.ID {
		t.Fatal("the second dataset reused the first's identity")
	}
	// Both declare the same type name and both mint their own identities, which
	// the composite keys are what make safe.
	write := func(a store.Access, dataset string) string {
		t.Helper()
		var id string
		if e := s.Transact(t.Context(), a, dataset, func(tx store.Tx) error {
			if _, err := tx.CreateMetadata(p.Meta{ID: typeID, Fields: []p.Field{{ID: "n", MetaID: typeID, Type: p.IntegerType}}}); err != nil {
				return err
			}
			set, err := tx.Insert(typeID, nil)
			if err == nil {
				id = set.Records[0].ID.ID
			}
			return err
		}); e != nil {
			t.Fatal(e)
		}
		return id
	}
	write(one, first.ID)
	write(two, second.DatasetID)
	rows := func(a store.Access, dataset string) int {
		t.Helper()
		var n int
		if e := s.Transact(t.Context(), a, dataset, func(tx store.Tx) error {
			found, err := tx.Matching(typeID, nil)
			n = len(found)
			return err
		}); e != nil {
			t.Fatal(e)
		}
		return n
	}
	if rows(one, first.ID) != 1 || rows(two, second.DatasetID) != 1 {
		t.Fatal("a record crossed between datasets")
	}
	if err = s.Transact(t.Context(), one, second.DatasetID, func(store.Tx) error { return nil }); p.ErrorCode(err) != "unavailable" {
		t.Fatal("one dataset's caller reached the other", err)
	}
	for _, c := range []struct {
		access  store.Access
		dataset string
	}{{one, first.ID}, {two, second.DatasetID}} {
		held, e := s.Datasets(t.Context(), c.access)
		if e != nil || len(held) != 1 || held[0].ID != c.dataset {
			t.Fatal("a listing did not hold exactly that caller's dataset", held, e)
		}
	}
	// Erasing one is not a fact about the other.
	if _, err = s.Erase(t.Context(), two, second.DatasetID, second.DatasetID); err != nil {
		t.Fatal(err)
	}
	if rows(one, first.ID) != 1 {
		t.Fatal("erasing one dataset reached the other")
	}
	if got, e := s.Info(t.Context(), one, first.ID); e != nil || got.ID != first.ID {
		t.Fatal("the surviving dataset stopped being served", e)
	}
}

// The write lock used to be one constant taken by the driver for every write
// transaction in the database, because a driver cannot tell which dataset a
// transaction is for. Scoped to the dataset, a write to one no longer waits for
// a write to another — and two writes to the same one still do, which is the
// half that would be lost by simply deleting the lock.
func TestPostgresWritesContendOnlyWithinADataset(t *testing.T) {
	dsn := dbtest.Postgres(t)
	disposable(t, dsn)
	s, err := postgres.Open(t.Context(), dsn, filepath.Join(t.TempDir(), "retirement-fence"), p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const typeID = "test.contended"
	full := store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true}, View: store.FullView()}
	one := store.Access{Principal: "synthetic", Authorization: "one"}
	two := store.Access{Principal: "synthetic", Authorization: "two"}
	first, err := s.SoleDataset(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetAuthorization(t.Context(), first.ID, one, full); err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateDataset(t.Context(), two, "Second quarter", p.Limits{}, nil, "", [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	declare := func(a store.Access, dataset string) {
		t.Helper()
		if e := s.Transact(t.Context(), a, dataset, func(tx store.Tx) error {
			_, err := tx.CreateMetadata(p.Meta{ID: typeID})
			return err
		}); e != nil {
			t.Fatal(e)
		}
	}
	declare(one, first.ID)
	declare(two, second.DatasetID)
	insert := func(a store.Access, dataset string) <-chan error {
		done := make(chan error, 1)
		go func() {
			done <- s.Transact(t.Context(), a, dataset, func(tx store.Tx) error {
				_, err := tx.Insert(typeID, nil)
				return err
			})
		}()
		return done
	}

	// Hold the first dataset's write open. Releasing it is deferred as well as
	// done: a failure below would otherwise leave the transaction open, and the
	// cleanup that drops these tables would wait on it until the whole binary
	// timed out — which reports a deadline rather than the thing that broke.
	holding, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(release) }) }
	defer stop()
	held := make(chan error, 1)
	go func() {
		held <- s.WithTx(t.Context(), one, first.ID, func(tx store.Tx, _ *sql.Tx) error {
			if _, e := tx.Insert(typeID, nil); e != nil {
				return e
			}
			close(holding)
			<-release
			return nil
		})
	}()
	<-holding

	// The other dataset is written while that is held. Waiting here is the
	// failure: it would mean the lock is still one lock.
	select {
	case e := <-insert(two, second.DatasetID):
		if e != nil {
			t.Fatal("a write to another dataset failed", e)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("a write to another dataset waited for one it has nothing to do with")
	}

	// The same dataset still serializes. Completing here is the failure.
	same := insert(one, first.ID)
	select {
	case e := <-same:
		t.Fatal("two writes to one dataset overlapped", e)
	case <-time.After(500 * time.Millisecond):
	}
	stop()
	if e := <-held; e != nil {
		t.Fatal(e)
	}
	if e := <-same; e != nil {
		t.Fatal(e)
	}
	rows := func(a store.Access, dataset string) int {
		t.Helper()
		var n int
		if e := s.Transact(t.Context(), a, dataset, func(tx store.Tx) error {
			found, err := tx.Matching(typeID, nil)
			n = len(found)
			return err
		}); e != nil {
			t.Fatal(e)
		}
		return n
	}
	if rows(one, first.ID) != 2 || rows(two, second.DatasetID) != 1 {
		t.Fatal("a write was lost or crossed datasets", rows(one, first.ID), rows(two, second.DatasetID))
	}
}

// dropTessera empties a database disposable already holds. Calling disposable
// a second time instead would wait for the advisory lock the first call holds
// until the test ends.
func dropTessera(t *testing.T, admin *sql.DB) {
	t.Helper()
	if _, e := admin.ExecContext(context.Background(), `DO $$ DECLARE t record; BEGIN FOR t IN SELECT tablename FROM pg_tables WHERE schemaname='public' AND tablename LIKE 'tessera\_%' LOOP EXECUTE format('DROP TABLE IF EXISTS %I CASCADE', t.tablename); END LOOP; END $$`); e != nil {
		t.Fatal(e)
	}
}
