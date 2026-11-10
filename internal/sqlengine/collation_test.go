package sqlengine

import (
	"encoding/json"
	"testing"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// A read that stops early is correct only if storage yields records in the
// evaluator's order, which is byte order over the object identifier.
// PostgreSQL orders text by the database collation instead, and a locale can
// fold case and ignore punctuation — both of which an identifier may contain.
//
// This is the test that establishes the guarantee, and it has to be this test
// rather than the order check in the evaluator: that check compares each row
// with the one before, so a read that stops after one row gives it nothing to
// compare. Seeding a misordered source proved it — at limit 1 the wrong row
// came back and the check never fired. So the single-row read below is the
// case that matters most, not an edge case.
func TestStreamingReadsFollowByteOrderWhateverTheDatabaseCollation(t *testing.T) {
	admin := postgresAdmin(t)
	var collation string
	if err := admin.QueryRowContext(t.Context(), `SELECT datcollate FROM pg_database WHERE datname=current_database()`).Scan(&collation); err != nil {
		t.Fatal(err)
	}
	// On a byte-ordered database the query without COLLATE "C" would also
	// pass, so this test would prove nothing and must say so rather than
	// pass quietly.
	if collation == "C" || collation == "POSIX" {
		t.Fatalf("the database collation is %q, which is byte order already; this test needs a locale collation to prove anything", collation)
	}

	s := postgresFixture(t)
	dataset, err := s.SoleDataset(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetAuthorization(t.Context(), dataset.ID, owner, store.Authorization{
		Grants: store.Grants{Read: true, Write: true, Schema: true}, View: store.FullView(),
	}); err != nil {
		t.Fatal(err)
	}
	if err = s.Transact(t.Context(), owner, dataset.ID, func(tx store.Tx) error {
		_, e := tx.CreateMetadata(p.Meta{ID: typeID, Fields: []p.Field{{ID: "n", MetaID: typeID, Type: p.IntegerType}}})
		return e
	}); err != nil {
		t.Fatal(err)
	}
	// Minted identifiers are decimal strings, which a locale orders the same
	// way as bytes, so they cannot expose this. Identifiers can arrive from
	// elsewhere, and these are ones a locale orders differently: byte order
	// puts every capital before every lower-case letter and puts `_` between
	// them, where a locale typically interleaves case and ignores the `_`.
	for _, id := range []string{"a", "B", "_z", "Z", "b", "A"} {
		if _, err = admin.ExecContext(t.Context(), `INSERT INTO "tessera_objects" ("dataset_id","object_id","type_id","wire_bytes") VALUES ($1,$2,$3,$4)`,
			dataset.ID, id, typeID, 2); err != nil {
			t.Fatal(err)
		}
	}
	byteOrder := []string{"A", "B", "Z", "_z", "a", "b"}

	// Show that the database really does disagree with byte order here, so
	// the assertions below are testing something.
	var localeFirst string
	if err = admin.QueryRowContext(t.Context(), `SELECT "object_id" FROM "tessera_objects" WHERE "dataset_id"=$1 ORDER BY "object_id" LIMIT 1`, dataset.ID).Scan(&localeFirst); err != nil {
		t.Fatal(err)
	}
	if localeFirst == byteOrder[0] {
		t.Fatalf("collation %q orders %q first, which is byte order; these identifiers do not exercise it", collation, localeFirst)
	}

	for limit := 1; limit <= len(byteOrder); limit++ {
		var set p.RecordSet
		if err = s.Read(t.Context(), owner, dataset.ID, func(tx store.Tx) error {
			var e error
			set, e = tx.Query(typeID, p.Query{}, limit)
			return e
		}); err != nil {
			t.Fatalf("limit %d: %v", limit, err)
		}
		got := make([]string, len(set.Records))
		for i, r := range set.Records {
			got[i] = r.ID.ID
		}
		want, _ := json.Marshal(byteOrder[:limit])
		have, _ := json.Marshal(got)
		if string(want) != string(have) {
			t.Fatalf("limit %d under collation %q returned %s, byte order is %s", limit, collation, have, want)
		}
	}
}
