package sqlengine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// The record tables are keyed by dataset but nothing provisions a second one
// yet, so a neighbour is seeded directly. It carries its own type, objects and
// authorization, all of which this store must behave as though it cannot see.
// padding inflates its schema so that a budget taken over the whole database
// rather than over one dataset is exceeded; objects are named by the caller,
// because identities are minted per dataset and so do collide across them.
func seedNeighbour(t *testing.T, s *Store, id string, padding int, objects ...string) {
	t.Helper()
	limits, err := json.Marshal(p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"id":"` + typeID + `","pad":"` + string(bytes.Repeat([]byte{'x'}, padding)) + `","fields":[]}`)
	statements := []struct {
		sql       string
		arguments []any
	}{
		// Placed ahead of this store's own row. A statement that names no dataset
		// then reads the neighbour, rather than reading the right row by accident
		// because the store's dataset happened to be inserted first.
		{`INSERT INTO "tessera_dataset" ("rowid","dataset_id","secret","next_id","watermark","limits") VALUES (-1,$1,$2,'1','w:neighbour',$3)`, []any{id, bytes.Repeat([]byte{7}, 32), limits}},
		{`INSERT INTO "tessera_types" ("dataset_id","type_id","body") VALUES ($1,$2,$3)`, []any{id, typeID, body}},
		{`INSERT INTO "tessera_access" ("dataset_id","principal","authorization","grants","generation","view") VALUES ($1,'intruder','intruder',$2,'g-neighbour',$3)`, []any{id, []byte(`{"read":true,"write":true,"schema":true,"erase":true}`), []byte(`{"full":true}`)}},
	}
	for _, object := range objects {
		statements = append(statements, struct {
			sql       string
			arguments []any
		}{`INSERT INTO "tessera_objects" ("dataset_id","object_id","type_id","fields") VALUES ($1,$2,$3,$4)`, []any{id, object, typeID, []byte(`{}`)}})
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement.sql, statement.arguments...); err != nil {
			t.Fatal(err)
		}
	}
}

func datasetWatermark(t *testing.T, s *Store, id string) string {
	t.Helper()
	var watermark string
	if err := s.db.QueryRow(`SELECT "watermark" FROM "tessera_dataset" WHERE "dataset_id"=$1`, id).Scan(&watermark); err != nil {
		t.Fatal(err)
	}
	return watermark
}

// Every statement over a record table names its dataset. Drop any one of those
// predicates and this fails: a neighbour's rows are counted, its object is
// served, its grant admits a principal this dataset never authorized, or a
// write here rewrites its watermark.
func TestNeighbouringDatasetIsNeitherReadNorWritten(t *testing.T) {
	s, _, dataset := fixture(t)
	auth := store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true, Erase: true}, View: store.FullView()}
	if err := s.SetAuthorization(t.Context(), dataset, owner, auth); err != nil {
		t.Fatal(err)
	}
	reply, err := s.Mutate(t.Context(), owner, mutation(t, dataset, key(700), insertBody))
	if err != nil {
		t.Fatal(err)
	}
	id := identity(t, reply)
	const neighbour = "d-neighbour"
	// One of the neighbour's objects carries this dataset's identity, which two
	// datasets minting identities independently will eventually do on their own.
	seedNeighbour(t, s, neighbour, 0, "o-neighbour", id)
	watermark := datasetWatermark(t, s, neighbour)

	if n := count(t, s, dataset); n != 1 {
		t.Fatal("neighbouring records counted", n)
	}
	if got := record(t, s, dataset, id); len(got.Records) != 1 {
		t.Fatal("own record lost")
	}
	err = s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
		_, e := tx.Object(typeID, "o-neighbour", nil)
		return e
	})
	if p.ErrorCode(err) != "not_found" {
		t.Fatal("neighbouring record served", err)
	}
	preview, err := s.PreviewErase(t.Context(), owner, dataset)
	if err != nil || preview.ObjectCount != 1 || preview.TypeCount != 1 || preview.AffectedPrincipals != 0 {
		t.Fatal(preview, err)
	}

	// A grant is a grant within one dataset. The neighbour's administrator is
	// an unknown principal here, refused rather than told the dataset exists —
	// which is why the answer is unavailable and not permission_denied, since
	// the latter would confirm what the listing conceals.
	intruder := store.Access{Principal: "intruder", Authorization: "intruder"}
	if _, err = s.Info(t.Context(), intruder, dataset); p.ErrorCode(err) != "unavailable" {
		t.Fatal("neighbouring grant admitted", err)
	}
	if err = s.Transact(t.Context(), intruder, dataset, func(store.Tx) error { return nil }); p.ErrorCode(err) != "unavailable" {
		t.Fatal("neighbouring grant admitted", err)
	}

	// Writing here must reach only this dataset's rows: the neighbour keeps its
	// object and its watermark, which a predicate-free UPDATE would rewrite.
	if err = s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error { return tx.Delete(typeID, id) }); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, dataset); n != 0 {
		t.Fatal("own record survived deletion", n)
	}
	var surviving int
	if err = s.db.QueryRow(`SELECT count(*) FROM "tessera_objects" WHERE "dataset_id"=$1`, neighbour).Scan(&surviving); err != nil {
		t.Fatal(err)
	}
	if surviving != 2 {
		t.Fatal("neighbouring record deleted", surviving)
	}
	if after := datasetWatermark(t, s, neighbour); after != watermark {
		t.Fatal("neighbouring watermark advanced", after)
	}
	// Both datasets declare the same type. Dropping it here leaves the
	// neighbour's definition, which names the same type of a different dataset.
	if err = s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
		_, _, e := tx.DeleteMetadata(typeID, false)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	var definitions int
	if err = s.db.QueryRow(`SELECT count(*) FROM "tessera_types" WHERE "dataset_id"=$1`, neighbour).Scan(&definitions); err != nil {
		t.Fatal(err)
	}
	if definitions != 1 {
		t.Fatal("neighbouring type definition deleted", definitions)
	}
}

// Erasure retires an identity and mints a replacement. With the record tables
// keyed by dataset, the rows the replacement owns must be the ones it can see,
// and a neighbour must be untouched by an erasure it has nothing to do with.
func TestEraseRekeysOnlyItsOwnDataset(t *testing.T) {
	s, _, dataset := fixture(t)
	auth := store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true, Erase: true}, View: store.FullView()}
	if err := s.SetAuthorization(t.Context(), dataset, owner, auth); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Mutate(t.Context(), owner, mutation(t, dataset, key(701), insertBody)); err != nil {
		t.Fatal(err)
	}
	const neighbour = "d-neighbour"
	seedNeighbour(t, s, neighbour, 0, "o-neighbour")

	result, err := s.Erase(t.Context(), owner, dataset, dataset)
	if err != nil {
		t.Fatal(err)
	}
	if result.DatasetID == dataset {
		t.Fatal("identity reused")
	}
	info, err := s.Info(t.Context(), owner, result.DatasetID)
	if err != nil || info.ID != result.DatasetID {
		t.Fatal("the replacement is not served", info.ID, err)
	}
	// The replacement names what it replaced, read from its own row rather than
	// from whichever dataset row the database happens to return first.
	if info.ReplacedDatasetID != dataset {
		t.Fatal("replacement does not name the identity it retired", info.ReplacedDatasetID)
	}
	// The replacement is empty: its schema is gone, so there is no type left to
	// query by, which is what an erased dataset looks like from the protocol.
	var remainingTypes int
	if err = s.Transact(t.Context(), owner, result.DatasetID, func(tx store.Tx) error {
		metas, e := tx.ListMetadata()
		remainingTypes = len(metas)
		return e
	}); err != nil || remainingTypes != 0 {
		t.Fatal("erased schema served", remainingTypes, err)
	}
	// The retired identity names no dataset at all, which is absence.
	if _, err = s.PreviewErase(t.Context(), owner, dataset); p.ErrorCode(err) != "not_found" {
		t.Fatal("retired identity still resolves", err)
	}
	for _, table := range []string{"tessera_types", "tessera_objects", "tessera_access"} {
		var remaining int
		if err = s.db.QueryRow(`SELECT count(*) FROM "`+table+`" WHERE "dataset_id"=$1`, dataset).Scan(&remaining); err != nil {
			t.Fatal(err)
		}
		if remaining != 0 {
			t.Fatal("retired rows retained in "+table, remaining)
		}
	}
	var types, objects, access int
	if err = s.db.QueryRow(`SELECT (SELECT count(*) FROM "tessera_types" WHERE "dataset_id"=$1),(SELECT count(*) FROM "tessera_objects" WHERE "dataset_id"=$1),(SELECT count(*) FROM "tessera_access" WHERE "dataset_id"=$1)`, neighbour).Scan(&types, &objects, &access); err != nil {
		t.Fatal(err)
	}
	if types != 1 || objects != 1 || access != 1 {
		t.Fatal("neighbour erased alongside", types, objects, access)
	}
	if watermark := datasetWatermark(t, s, neighbour); watermark != "w:neighbour" {
		t.Fatal("neighbouring watermark replaced", watermark)
	}
}

// Model the preceding SQLite schema: no dataset column on any record table and
// a dataset row keyed by the singleton that held it to one. Every column the
// migration adds has to go, or reopening re-adds one that already exists.
func demoteToSingletonSchema(t *testing.T, s *Store) {
	t.Helper()
	// Objects are carried out and dropped before types is rebuilt, for the same
	// reason the forward migration does it: SQLite refuses to drop a table
	// another still references, and the carry table declares no foreign key.
	for _, statement := range []string{
		// Later migrations appended a column to receipts, and an indexed column
		// cannot be dropped while the index still names it.
		`DROP INDEX "tessera_receipts_executor"`,
		`DROP INDEX "tessera_receipts_retention_deadline"`,
		`ALTER TABLE "tessera_receipts" DROP COLUMN "retain_until"`,
		`ALTER TABLE "tessera_receipts" DROP COLUMN "executor_id"`,
		`ALTER TABLE "tessera_sequences" DROP COLUMN "meta"`,
		`DROP INDEX "tessera_imports_uncleaned"`,
		`ALTER TABLE "tessera_imports" DROP COLUMN "cleaned"`,
		`DROP TABLE "tessera_page_rows"`,
		`ALTER TABLE "tessera_pages" DROP COLUMN "position"`,
		`CREATE TABLE "carry" ("object_id" TEXT NOT NULL,"type_id" TEXT NOT NULL,"fields" BLOB NOT NULL)`,
		`INSERT INTO "carry" SELECT "object_id","type_id","fields" FROM "tessera_objects"`,
		`DROP TABLE "tessera_objects"`,
		`CREATE TABLE "old_types" ("type_id" TEXT PRIMARY KEY,"body" BLOB NOT NULL)`,
		`INSERT INTO "old_types" SELECT "type_id","body" FROM "tessera_types"`,
		`DROP TABLE "tessera_types"`,
		`ALTER TABLE "old_types" RENAME TO "tessera_types"`,
		`CREATE TABLE "tessera_objects" ("object_id" TEXT PRIMARY KEY,"type_id" TEXT NOT NULL REFERENCES "tessera_types"("type_id") DEFERRABLE INITIALLY DEFERRED,"fields" BLOB NOT NULL)`,
		`INSERT INTO "tessera_objects" SELECT "object_id","type_id","fields" FROM "carry"`,
		`DROP TABLE "carry"`,
		`CREATE INDEX "tessera_objects_type" ON "tessera_objects" ("type_id")`,
		`CREATE TABLE "old_access" ("principal" TEXT NOT NULL,"authorization" TEXT NOT NULL,"grants" BLOB NOT NULL,"generation" TEXT NOT NULL DEFAULT '',"view" BLOB NOT NULL DEFAULT '{"full":true}',PRIMARY KEY ("principal","authorization"))`,
		`INSERT INTO "old_access" SELECT "principal","authorization","grants","generation","view" FROM "tessera_access"`,
		`DROP TABLE "tessera_access"`,
		`ALTER TABLE "old_access" RENAME TO "tessera_access"`,
		`CREATE TABLE "old_dataset" ("singleton" INTEGER PRIMARY KEY CHECK ("singleton" = 1),"dataset_id" TEXT NOT NULL UNIQUE,"secret" BLOB NOT NULL CHECK (length("secret") = 32),"next_id" TEXT NOT NULL,"watermark" TEXT NOT NULL,"limits" BLOB NOT NULL,"replaced_dataset_id" TEXT)`,
		`INSERT INTO "old_dataset" SELECT 1,"dataset_id","secret","next_id","watermark","limits","replaced_dataset_id" FROM "tessera_dataset"`,
		`DROP TABLE "tessera_dataset"`,
		`ALTER TABLE "old_dataset" RENAME TO "tessera_dataset"`,
		`PRAGMA user_version=7`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(statement, err)
		}
	}
}

func TestSingletonSchemaMigratesWithItsRecords(t *testing.T) {
	s, path, dataset := fixture(t)
	reply, err := s.Mutate(t.Context(), owner, mutation(t, dataset, key(702), insertBody))
	if err != nil {
		t.Fatal(err)
	}
	id := identity(t, reply)
	before := record(t, s, dataset, id)
	info, err := s.Info(t.Context(), owner, dataset)
	if err != nil {
		t.Fatal(err)
	}
	demoteToSingletonSchema(t, s)
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(t.Context(), path, p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	after, err := s.Info(t.Context(), owner, dataset)
	if err != nil || after.ID != info.ID || after.Watermark != info.Watermark {
		t.Fatal("migration changed identity or watermark", after, err)
	}
	restored := record(t, s, dataset, id)
	if len(restored.Records) != 1 || restored.Records[0].Version != before.Records[0].Version {
		t.Fatal("migration lost the record")
	}
	// The carried rows belong to the dataset that held them, not to no dataset.
	var types, objects, access int
	if err = s.db.QueryRow(`SELECT (SELECT count(*) FROM "tessera_types" WHERE "dataset_id"=$1),(SELECT count(*) FROM "tessera_objects" WHERE "dataset_id"=$1),(SELECT count(*) FROM "tessera_access" WHERE "dataset_id"=$1)`, dataset).Scan(&types, &objects, &access); err != nil {
		t.Fatal(err)
	}
	if types != 1 || objects != 1 || access != 1 {
		t.Fatal("migration did not key the carried rows", types, objects, access)
	}
	// And the dataset counts what it carried, from the migration that began
	// keeping the count.
	counted(t, s, dataset)
	// Writing after the migration proves the rebuilt keys and the rebuilt
	// object-to-type foreign key accept ordinary work.
	if _, err = s.Mutate(t.Context(), owner, mutation(t, dataset, key(703), insertBody)); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, dataset); n != 2 {
		t.Fatal("post-migration write lost", n)
	}
}

// A database written by a later version is refused rather than misread. The
// version is taken from the database and advanced, so a migration added later
// moves this case with it instead of quietly making it test nothing.
func TestUnknownSchemaVersionIsRefused(t *testing.T) {
	s, path, _ := fixture(t)
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(fmt.Sprintf(`PRAGMA user_version=%d`, version+1)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(t.Context(), path, p.DevelopmentLimits())
	if err == nil {
		reopened.Close()
		t.Fatal("a future schema was opened")
	}
	if p.ErrorCode(err) != "not_supported" {
		t.Fatal(err)
	}
}

// The scan and materialization budgets are this dataset's, not the database's.
// A neighbour large enough to exhaust either on its own must not make ordinary
// work here refusable, which is what a budget taken over every row would do.
func TestNeighbourDoesNotConsumeThisDatasetsBudget(t *testing.T) {
	limits := p.DevelopmentLimits()
	limits.MaxScannedRows = 2
	limits.MaxMaterializedBytes = 32 << 10
	path := filepath.Join(t.TempDir(), "bounded.db")
	s, err := Open(t.Context(), path, limits)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	dataset := sole(t, s)
	if err = s.SetAuthorization(t.Context(), dataset, owner, store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true}, View: store.FullView()}); err != nil {
		t.Fatal(err)
	}
	if err = s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
		_, e := tx.CreateMetadata(p.Meta{ID: typeID, Fields: []p.Field{{ID: "n", MetaID: typeID, Type: p.IntegerType}}})
		return e
	}); err != nil {
		t.Fatal(err)
	}
	info, err := s.Info(t.Context(), owner, dataset)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Mutate(t.Context(), owner, mutation(t, info.ID, key(704), insertBody)); err != nil {
		t.Fatal(err)
	}
	// Three rows exceeds the scan bound and the padded schema exceeds the
	// materialization bound, each on the neighbour's own.
	seedNeighbour(t, s, "d-large", 64<<10, "o-1", "o-2", "o-3")

	if n := count(t, s, dataset); n != 1 {
		t.Fatal("neighbouring rows counted against this dataset", n)
	}
	if _, err = s.Mutate(t.Context(), owner, mutation(t, info.ID, key(705), insertBody)); err != nil {
		t.Fatal("a neighbour's size refused a write here", err)
	}
	// The bound still binds: this dataset's own third row crosses it. A keyed
	// mutation records its outcome, so the refusal arrives as the reply.
	reply, err := s.Mutate(t.Context(), owner, mutation(t, info.ID, key(706), insertBody))
	if err != nil {
		t.Fatal(err)
	}
	var failure struct {
		Error p.Error `json:"error"`
	}
	if json.Unmarshal(reply.Body, &failure) != nil || failure.Error.Code != "query_result_limit_exceeded" {
		t.Fatal("the scan bound stopped binding", reply.Status, string(reply.Body))
	}
	if n := count(t, s, dataset); n != 2 {
		t.Fatal("a refused write was retained", n)
	}
}

// Every operation names a dataset. One that does not exist is absence, whoever
// asks: erasure retires an identity along with its authorizations, so a caller
// that cannot learn absence cannot learn that its dataset is gone. A dataset
// that does exist but is not the caller's is a refusal, which says nothing
// about what it holds.
func TestOperationsRefuseADatasetThisStoreDoesNotServe(t *testing.T) {
	s, _, dataset := fixture(t)
	const absent = "d-not-served"
	seedNeighbour(t, s, "d-neighbour", 0, "o-neighbour")
	stranger := store.Access{Principal: "stranger", Authorization: "stranger"}
	for _, a := range []store.Access{owner, stranger} {
		// not_found is reserved for proven retirement. An identity that names
		// nothing has no such evidence behind it, so it answers unavailable.
		if _, err := s.Info(t.Context(), a, absent); p.ErrorCode(err) != "unavailable" {
			t.Fatal("an identity naming no dataset claimed a retirement", err)
		}
		if err := s.Transact(t.Context(), a, absent, func(store.Tx) error {
			t.Fatal("the callback ran for a dataset that does not exist")
			return nil
		}); p.ErrorCode(err) != "unavailable" {
			t.Fatal("an identity naming no dataset claimed a retirement", err)
		}
	}
	// The neighbour exists. Neither caller holds it, and neither is told
	// anything about it beyond being refused.
	for _, a := range []store.Access{owner, stranger} {
		if _, err := s.Info(t.Context(), a, "d-neighbour"); p.ErrorCode(err) != "unavailable" {
			t.Fatal("a dataset that is not the caller's was not concealed", err)
		}
		if err := s.Transact(t.Context(), a, "d-neighbour", func(store.Tx) error {
			t.Fatal("the callback ran for a dataset that is not the caller's")
			return nil
		}); p.ErrorCode(err) != "unavailable" {
			t.Fatal("a dataset that is not the caller's was not concealed", err)
		}
	}
	// A caller holding nothing anywhere is told unavailable on this store's own
	// dataset: it learns neither that the dataset exists nor that it does not.
	if _, err := s.Info(t.Context(), stranger, dataset); p.ErrorCode(err) != "unavailable" {
		t.Fatal("an ungranted caller was told the dataset is absent", err)
	}
}

// The listing answers what the caller may use, and nothing else. Two callers
// with grants on different datasets in one storage each see their own; a caller
// with no grant anywhere gets an empty listing rather than a refusal, and
// learns nothing from it.
func TestListingHoldsOnlyWhatTheCallerMayUse(t *testing.T) {
	s, _, dataset := fixture(t)
	const neighbour = "d-neighbour"
	seedNeighbour(t, s, neighbour, 0, "o-neighbour")
	names := func(a store.Access) []string {
		t.Helper()
		held, err := s.Datasets(t.Context(), a)
		if err != nil {
			t.Fatal("a listing was refused rather than answered", err)
		}
		var ids []string
		for _, info := range held {
			ids = append(ids, info.ID)
		}
		return ids
	}
	if held := names(owner); len(held) != 1 || held[0] != dataset {
		t.Fatal("the owner was not offered exactly its own dataset", held)
	}
	// The neighbour's administrator holds a real grant, on the other dataset.
	// It sees that one and is not told this one is here.
	intruder := store.Access{Principal: "intruder", Authorization: "intruder"}
	if held := names(intruder); len(held) != 1 || held[0] != neighbour {
		t.Fatal("a grant elsewhere did not list its own dataset alone", held)
	}
	for _, a := range []store.Access{
		{Principal: "stranger", Authorization: "stranger"},
		{Principal: "intruder", Authorization: "elsewhere"},
		{},
	} {
		if held := names(a); len(held) != 0 {
			t.Fatal("listed a dataset the caller may not use", held)
		}
	}
}

// A dataset carries a name of its own rather than the listing inventing one.
// An upgraded database answers exactly what it answered before, so a caller
// that reopens after the migration sees no change.
func TestDatasetCarriesItsOwnName(t *testing.T) {
	s, path, dataset := fixture(t)
	info, err := s.Info(t.Context(), owner, dataset)
	if err != nil || info.DisplayName != "Tessera dataset" {
		t.Fatal("dataset was not named", info.DisplayName, err)
	}
	// The name is read from the row, not returned as a constant.
	if _, err = s.db.Exec(`UPDATE "tessera_dataset" SET "display_name"=$1 WHERE "dataset_id"=$2`, "Second quarter", dataset); err != nil {
		t.Fatal(err)
	}
	if info, err = s.Info(t.Context(), owner, dataset); err != nil || info.DisplayName != "Second quarter" {
		t.Fatal("the stored name was not read", info.DisplayName, err)
	}
	held, err := s.Datasets(t.Context(), owner)
	if err != nil || len(held) != 1 || held[0].DisplayName != "Second quarter" {
		t.Fatal("the listing did not carry the name", held, err)
	}
	// A database written before the column existed is named on the way forward.
	demoteToSingletonSchema(t, s)
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(t.Context(), path, p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if info, err = reopened.Info(t.Context(), owner, dataset); err != nil || info.DisplayName != "Tessera dataset" {
		t.Fatal("migration left the dataset nameless", info.DisplayName, err)
	}
}

// Provisioning is composition, like installing a grant: the contract describes
// datasets a server holds and says nothing about how one comes to exist. A
// second dataset is a real dataset — its own identity, secret, watermark,
// bounds, schema and records — and neither dataset can see the other's.
func TestASecondDatasetIsAWholeDataset(t *testing.T) {
	s, _, first := fixture(t)
	other := store.Access{Principal: "other", Authorization: "other"}
	created, err := s.CreateDataset(t.Context(), other, "Second quarter", p.Limits{}, nil, "", [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if created.DatasetID == first {
		t.Fatal("the second dataset reused the first's identity", created)
	}
	// A creation reports an identity; everything else about the dataset is
	// discovery state and is asked for separately.
	info, err := s.Info(t.Context(), other, created.DatasetID)
	if err != nil {
		t.Fatal(err)
	}
	if info.DisplayName != "Second quarter" || info.Watermark == "" {
		t.Fatal("the created dataset is not described as asked for", info)
	}
	// Identities are minted per dataset, so both number their records from the
	// same start. That is only safe because the records are keyed separately.
	var firstID, secondID string
	if err = s.Transact(t.Context(), owner, first, func(tx store.Tx) error {
		set, e := tx.Insert(typeID, nil)
		if e == nil {
			firstID = set.Records[0].ID.ID
		}
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if err = s.Transact(t.Context(), other, created.DatasetID, func(tx store.Tx) error {
		if _, e := tx.CreateMetadata(p.Meta{ID: typeID, Fields: []p.Field{{ID: "n", MetaID: typeID, Type: p.IntegerType}}}); e != nil {
			return e
		}
		set, e := tx.Insert(typeID, nil)
		if e == nil {
			secondID = set.Records[0].ID.ID
		}
		return e
	}); err != nil {
		t.Fatal(err)
	}
	// count reads as the fixture's owner, who holds only the first dataset.
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
	if rows(owner, first) != 1 || rows(other, created.DatasetID) != 1 {
		t.Fatal("a record crossed between datasets")
	}
	// Each holds its own record under whatever identity it minted, and neither
	// serves the other's caller.
	if got := record(t, s, first, firstID); len(got.Records) != 1 {
		t.Fatal("the first dataset lost its record")
	}
	if err = s.Transact(t.Context(), owner, created.DatasetID, func(store.Tx) error { return nil }); p.ErrorCode(err) != "unavailable" {
		t.Fatal("the first dataset's caller reached the second", err)
	}
	if err = s.Transact(t.Context(), other, first, func(store.Tx) error { return nil }); p.ErrorCode(err) != "unavailable" {
		t.Fatal("the second dataset's caller reached the first", err)
	}
	// Each listing holds one entry, and it is that caller's own.
	for _, c := range []struct {
		access  store.Access
		dataset string
	}{{owner, first}, {other, created.DatasetID}} {
		held, e := s.Datasets(t.Context(), c.access)
		if e != nil || len(held) != 1 || held[0].ID != c.dataset {
			t.Fatal("a listing did not hold exactly that caller's dataset", held, e)
		}
	}
	// Erasing one leaves the other untouched.
	if err = s.SetAuthorization(t.Context(), created.DatasetID, other, store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true, Erase: true}, View: store.FullView()}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Erase(t.Context(), other, created.DatasetID, created.DatasetID); err != nil {
		t.Fatal(err)
	}
	if rows(owner, first) != 1 {
		t.Fatal("erasing one dataset reached the other")
	}
	if got := record(t, s, first, firstID); len(got.Records) != 1 || got.Records[0].ID.ID != secondID && firstID != secondID {
		t.Fatal("the surviving dataset's record changed")
	}
}

// A name is shown to a person, so it has to be text. Provisioning refuses one
// that could rewrite the line it is printed into, or that names nothing.
func TestADatasetNameMustBeText(t *testing.T) {
	s, _, _ := fixture(t)
	for _, name := range []string{"", "   ", "first\nsecond", "bell\a", "\x1b[2Jcleared", strings.Repeat("n", 201), "bad\xff"} {
		if _, err := s.CreateDataset(t.Context(), owner, name, p.Limits{}, nil, "", [32]byte{}); p.ErrorCode(err) != "invalid_field" {
			t.Fatalf("accepted %q as a name: %v", name, err)
		}
	}
	if _, err := s.CreateDataset(t.Context(), store.Access{}, "Nameless caller", p.Limits{}, nil, "", [32]byte{}); p.ErrorCode(err) != "permission_denied" {
		t.Fatal("provisioned a dataset for no one", err)
	}
	// A name a person would actually use is accepted, including one that is not
	// ASCII: a name is text, not an identifier.
	if _, err := s.CreateDataset(t.Context(), owner, "Zweites Quartal — Löhne", p.Limits{}, nil, "", [32]byte{}); err != nil {
		t.Fatal(err)
	}
}

// A dataset leaves a caller's listing the moment the grant does, whether the
// grant expired, was revoked, or the dataset was erased out from under it.
func TestListingFollowsTheGrant(t *testing.T) {
	s, _, dataset := fixture(t)
	auth := store.Authorization{Grants: store.Grants{Read: true, Erase: true}, View: store.FullView()}
	if err := s.SetAuthorization(t.Context(), dataset, owner, auth); err != nil {
		t.Fatal(err)
	}
	listed := func() int {
		t.Helper()
		held, err := s.Datasets(t.Context(), owner)
		if err != nil {
			t.Fatal(err)
		}
		return len(held)
	}
	if listed() != 1 {
		t.Fatal("a current grant did not list its dataset")
	}
	// An expired grant leaves the access row in place, so the listing has to
	// decide on the grant rather than on the row.
	expiry := time.Now().Add(-time.Minute)
	expired := auth
	expired.Grants.ExpiresAt = &expiry
	if err := s.SetAuthorization(t.Context(), dataset, owner, expired); err != nil {
		t.Fatal(err)
	}
	if listed() != 0 {
		t.Fatal("an expired grant kept its dataset in the listing")
	}
	if err := s.SetAuthorization(t.Context(), dataset, owner, auth); err != nil {
		t.Fatal(err)
	}
	if listed() != 1 {
		t.Fatal("a reinstated grant did not list its dataset")
	}
	if err := s.Revoke(t.Context(), dataset, owner); err != nil {
		t.Fatal(err)
	}
	if listed() != 0 {
		t.Fatal("a revoked grant kept its dataset in the listing")
	}
}

// A grant has to be over something. Nothing in the schema says so, because
// access rows outlive nothing, so provisioning says it instead.
func TestAGrantCannotBeInstalledOnNothing(t *testing.T) {
	s, _, dataset := fixture(t)
	auth := store.Authorization{Grants: store.Grants{Read: true}, View: store.FullView()}
	if err := s.SetAuthorization(t.Context(), "d-not-served", owner, auth); p.ErrorCode(err) != "not_found" {
		t.Fatal("installed a grant over nothing", err)
	}
	// Revoking one that is not there is not an error: there is nothing to undo.
	if err := s.Revoke(t.Context(), "d-not-served", owner); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAuthorization(t.Context(), dataset, owner, auth); err != nil {
		t.Fatal(err)
	}
}

// A composition built around one dataset refuses to guess once there are
// several, rather than administering whichever came first.
func TestSoleDatasetRefusesToChoose(t *testing.T) {
	s, _, dataset := fixture(t)
	held, err := s.SoleDataset(t.Context())
	if err != nil || held.ID != dataset {
		t.Fatal("the one dataset was not named", held, err)
	}
	if _, err = s.CreateDataset(t.Context(), owner, "Second quarter", p.Limits{}, nil, "", [32]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SoleDataset(t.Context()); p.ErrorCode(err) != "conflict" {
		t.Fatal("a storage with two datasets named one of them", err)
	}
	provisioned, err := s.Provisioned(t.Context())
	if err != nil || len(provisioned) != 2 {
		t.Fatal("composition could not see both datasets", provisioned, err)
	}
}

// Receipt retention is advertised in a dataset's own description, so it is that
// dataset's window that decides when an outcome is discarded. Taking the
// storage's would let a dataset promising a long window lose an outcome early,
// which a caller asking for its status reads as never having happened.
func TestReceiptsAreKeptForTheirOwnDatasetsWindow(t *testing.T) {
	// The fixture's dataset promises the floor; this one promises a week. A
	// shorter window than the floor is no longer a legal thing to create, so
	// the two windows are the floor and a longer one rather than a minute and
	// a day, and the receipts age past one but not the other.
	s, _, hasty := fixture(t)
	generous := p.DevelopmentLimits()
	generous.IdempotencyRetentionSeconds = 7 * p.MinimumIdempotencyRetentionSeconds
	other := store.Access{Principal: "other", Authorization: "other"}
	made, err := s.CreateDataset(t.Context(), other, "Generous retention", generous, nil, "", [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	patient := made.DatasetID
	patientInfo, err := s.Info(t.Context(), other, patient)
	if err != nil {
		t.Fatal(err)
	}
	if patientInfo.Limits.IdempotencyRetentionSeconds != generous.IdempotencyRetentionSeconds {
		t.Fatal("the dataset did not take the bounds it was created with", patientInfo.Limits.IdempotencyRetentionSeconds)
	}
	// Both outcomes are two days old. One dataset promised a week and keeps
	// it; the other promised a day and does not.
	old := time.Now().Add(-48 * time.Hour).UnixMilli()
	for _, c := range []struct{ dataset, key string }{{patient, key(900)}, {hasty, key(901)}} {
		if _, err = s.db.Exec(`INSERT INTO "tessera_receipts" ("key","dataset_id","principal","authorization","digest","claimed_at","state","status","body","watermark","replay_scope") VALUES ($1,$2,'synthetic','owner',$3,$4,'committed',200,$5,'w:seeded','read')`,
			c.key, c.dataset, make([]byte, 32), old, []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.PruneReceipts(t.Context()); err != nil {
		t.Fatal(err)
	}
	retained := func(dataset, key string) bool {
		t.Helper()
		var n int
		if e := s.db.QueryRow(`SELECT count(*) FROM "tessera_receipts" WHERE "dataset_id"=$1 AND "key"=$2`, dataset, key).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n == 1
	}
	if !retained(patient, key(900)) {
		t.Fatal("an outcome was discarded inside the window its dataset advertises")
	}
	if retained(hasty, key(901)) {
		t.Fatal("an outcome outlived the window its dataset advertises")
	}
}

// Describing a type is a question about the schema, and the answer is the
// same whether the dataset holds ten records or ten million. It used to cost
// reading and decoding every one of them, because every evaluation began by
// materialising the whole dataset.
//
// Asserted in allocations rather than in time: the count is what the work is,
// and unlike a duration it does not depend on what else the machine is doing.
func TestDescribingATypeDoesNotDependOnHowManyRecordsThereAre(t *testing.T) {
	small := schemaReadCost(t, 10)
	large := schemaReadCost(t, 2000)
	if large > 2*small {
		t.Fatal("describing a type got more expensive as records were added", small, large)
	}
}

// And the same for reading one record, which is the operation the review
// named: a fixed-size question whose answer cost grew with everything else
// stored beside it.
func TestReadingOneRecordDoesNotDependOnHowManyThereAre(t *testing.T) {
	small := recordReadCost(t, 10)
	large := recordReadCost(t, 2000)
	if large > 2*small {
		t.Fatal("reading one record got more expensive as records were added", small, large)
	}
}

func recordReadCost(t *testing.T, records int) float64 {
	t.Helper()
	s, dataset := countedFixture(t, records)
	return testing.AllocsPerRun(10, func() {
		if err := s.ReadRecord(t.Context(), owner, dataset, func(tx store.Tx) error {
			_, e := tx.Object("test.counted", "1", nil)
			return e
		}); err != nil {
			t.Fatal(err)
		}
	})
}

func schemaReadCost(t *testing.T, records int) float64 {
	t.Helper()
	s, dataset := countedFixture(t, records)
	return testing.AllocsPerRun(10, func() {
		if err := s.ReadSchema(t.Context(), owner, dataset, func(tx store.Tx) error {
			_, e := tx.Metadata("test.counted")
			return e
		}); err != nil {
			t.Fatal(err)
		}
	})
}

func countedFixture(t *testing.T, records int) (*Store, string) {
	t.Helper()
	s, _, dataset := fixture(t)
	if err := s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
		if _, e := tx.CreateMetadata(p.Meta{ID: "test.counted", Fields: []p.Field{{ID: "n", MetaID: "test.counted", Type: p.IntegerType}}}); e != nil {
			return e
		}
		for i := 0; i < records; i++ {
			value, e := p.NewValue(p.IntegerType, int64(i))
			if e != nil {
				return e
			}
			if _, e = tx.Insert("test.counted", map[string]p.Value{"n": value}); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return s, dataset
}

// The records are not there to be read in a schema evaluation, so asking for
// one is an error rather than an answer of none — and a read evaluation saves
// nothing, so a write attempted in one must not quietly vanish.
func TestAnEvaluationRefusesWhatItCannotServe(t *testing.T) {
	s, _, dataset := fixture(t)
	if err := s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
		_, e := tx.CreateMetadata(p.Meta{ID: "test.bounded", Fields: []p.Field{{ID: "n", MetaID: "test.bounded", Type: p.IntegerType}}})
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReadSchema(t.Context(), owner, dataset, func(tx store.Tx) error {
		_, e := tx.Query("test.bounded", p.Query{}, 10)
		return e
	}); p.ErrorCode(err) != "internal_error" {
		t.Fatal("a schema evaluation answered a question about records", err)
	}
	if err := s.Read(t.Context(), owner, dataset, func(tx store.Tx) error {
		value, e := p.NewValue(p.IntegerType, int64(1))
		if e != nil {
			return e
		}
		_, e = tx.Insert("test.bounded", map[string]p.Value{"n": value})
		return e
	}); p.ErrorCode(err) != "internal_error" {
		t.Fatal("a read evaluation accepted a write it would never have saved", err)
	}
	// And a read still reads.
	if err := s.Read(t.Context(), owner, dataset, func(tx store.Tx) error {
		_, e := tx.Query("test.bounded", p.Query{}, 10)
		return e
	}); err != nil {
		t.Fatal(err)
	}
}

// Returning a row used to cost resolving the effective type and deriving the
// projected fields from scratch, for every row. The metadata is the same for
// every row in an answer — the projection is settled by the query, not by the
// record — so it is settled once.
//
// Measured as the difference between a query that returns ten rows and one
// that returns them all, over one dataset. What is left when the two are
// subtracted is the cost of a row and nothing else.
func TestReturningARowDoesNotCostRederivingTheProjection(t *testing.T) {
	const records = 2000
	s, dataset := countedFixture(t, records)
	cost := func(filter json.RawMessage, expected int) float64 {
		return testing.AllocsPerRun(5, func() {
			if err := s.Read(t.Context(), owner, dataset, func(tx store.Tx) error {
				set, e := tx.Query("test.counted", p.Query{Filter: filter}, records)
				if e == nil && len(set.Records) != expected {
					t.Fatal("query returned", len(set.Records), "of", expected)
				}
				return e
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
	// Both halves read every record and only one returns them, so what they
	// differ by is the cost of returning a row and nothing else.
	//
	// This used to compare a ten-row query with a full one. That isolated the
	// same quantity only because both used to decode the whole dataset, so
	// decoding cancelled out. Reads now stop once they hold what the answer
	// needs, a ten-row query decodes about ten rows, and the old difference
	// would have charged every row's decode to the projection — reporting a
	// regression that was really the optimisation working.
	none := json.RawMessage(`{"field":{"id":"n","exact":{"type":"integer","value":-1}}}`)
	baseline, all := cost(none, 0), cost(nil, records)
	perRow := (all - baseline) / float64(records)
	// Re-deriving it per row cost something like eighty allocations a row;
	// carrying it costs about one. Ten is far from both.
	if perRow > 10 {
		t.Fatal("returning a row costs more than carrying the projection to it", perRow)
	}
}

// A retention window is a promise a client plans reconciliation against, so a
// deployment cannot promise one too short to reconcile inside. The bound used
// to be "greater than zero", which let a dataset advertise a minute and stay
// conformant while making the guarantee the key exists for unusable.
func TestADatasetCannotPromiseAWindowTooShortToReconcileIn(t *testing.T) {
	s, _, _ := fixture(t)
	owner := store.Access{Principal: "other", Authorization: "other"}
	for _, seconds := range []int{0, 60, p.MinimumIdempotencyRetentionSeconds - 1} {
		limits := p.DevelopmentLimits()
		limits.IdempotencyRetentionSeconds = seconds
		if _, err := s.CreateDataset(t.Context(), owner, "Hasty", limits, nil, "", [32]byte{}); p.ErrorCode(err) != "invalid_field" {
			t.Fatal("a dataset promising", seconds, "seconds of retention was created", err)
		}
	}
	limits := p.DevelopmentLimits()
	limits.IdempotencyRetentionSeconds = p.MinimumIdempotencyRetentionSeconds
	if _, err := s.CreateDataset(t.Context(), owner, "At the floor", limits, nil, "", [32]byte{}); err != nil {
		t.Fatal("a dataset promising exactly the floor was refused", err)
	}
}
