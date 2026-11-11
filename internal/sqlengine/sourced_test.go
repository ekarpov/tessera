package sqlengine

import (
	"database/sql"
	"fmt"
	"math/rand/v2"
	"strconv"
	"testing"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// snapshot is everything a write can change in one dataset, as stored.
func snapshot(t *testing.T, s *Store, dataset string) (rows string, next, watermark string) {
	t.Helper()
	result, err := s.db.QueryContext(t.Context(), `SELECT "object_id","type_id","fields" FROM "tessera_read_objects" WHERE "dataset_id"=$1 ORDER BY "object_id"`, dataset)
	if err != nil {
		t.Fatal(err)
	}
	for result.Next() {
		var id, typeID string
		var fields []byte
		if err = result.Scan(&id, &typeID, &fields); err != nil {
			t.Fatal(err)
		}
		rows += id + "|" + typeID + "|" + string(fields) + "\n"
	}
	result.Close()
	if err = s.db.QueryRowContext(t.Context(), `SELECT "next_id","watermark" FROM "tessera_dataset" WHERE "dataset_id"=$1`, dataset).Scan(&next, &watermark); err != nil {
		t.Fatal(err)
	}
	return rows, next, watermark
}

// A write evaluated against only the records it touches must do exactly what
// the same write evaluated against the whole dataset does: the same answer,
// the same rows stored, the same identities, and the watermark moving exactly
// when something changed. Random transactions are run against two stores,
// one forced onto the whole-image path, and compared after every one —
// including transactions that fail part way and ones that scan, which the
// sourced path hands back to the whole-image path.
func TestSourcedWritesMatchWholeImageWrites(t *testing.T) {
	for seed := uint64(1); seed <= 6; seed++ {
		t.Run(fmt.Sprint("seed ", seed), func(t *testing.T) {
			sourced, _, sourcedSet := fixture(t)
			whole, _, wholeSet := fixture(t)
			whole.wholeImageWrites = true
			random := rand.New(rand.NewPCG(seed, 99))
			value := func(n int64) p.Value {
				v, err := p.NewValue(p.IntegerType, n)
				if err != nil {
					t.Fatal(err)
				}
				return v
			}
			for step := 0; step < 120; step++ {
				ops := make([]int, 1+random.IntN(4))
				targets := make([]string, len(ops))
				values := make([]int64, len(ops))
				for i := range ops {
					ops[i] = random.IntN(9)
					targets[i] = strconv.Itoa(1 + random.IntN(40))
					values[i] = int64(random.IntN(5))
				}
				run := func(s *Store, dataset string) (string, error) {
					// What the transaction was told, in order: every answer a
					// read or a scan gave must be the same on both paths.
					var told string
					describe := func(set p.RecordSet) {
						for _, r := range set.Records {
							told += r.ID.ID + "="
							for _, v := range r.Values {
								told += fmt.Sprint(v.Payload()) + ","
							}
							told += ";"
						}
						told += "|"
					}
					err := s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
						// A callback may run twice; what it was told is only
						// what the run that counted told it.
						told = ""
						for i, op := range ops {
							var err error
							switch op {
							case 0, 1:
								_, err = tx.Insert(typeID, map[string]p.Value{"n": value(values[i])})
							case 2:
								// Often the value it already has: a write that
								// changes nothing must not move the watermark.
								err = tx.Update(typeID, targets[i], map[string]p.Value{"n": value(values[i])})
							case 3:
								err = tx.Delete(typeID, targets[i])
							case 4:
								var set p.RecordSet
								set, err = tx.Object(typeID, targets[i], nil)
								if p.ErrorCode(err) == "not_found" {
									err = nil
								}
								describe(set)
							case 5:
								// A scan: the sourced path cannot answer it.
								var set p.RecordSet
								set, err = tx.Query(typeID, p.Query{}, 5)
								describe(set)
							case 6:
								err = tx.Update(typeID, targets[i], map[string]p.Value{})
							case 7:
								// A scan that reads to the end.
								var groups []p.Aggregate
								groups, err = tx.Aggregate(typeID, "n", "", nil)
								told += fmt.Sprint(groups) + "|"
							case 8:
								// A scan that stops after one row.
								var set p.RecordSet
								set, err = tx.Query(typeID, p.Query{}, 1)
								describe(set)
							}
							if err != nil {
								return err
							}
						}
						return nil
					})
					return told, err
				}
				_, _, sourcedBefore := snapshot(t, sourced, sourcedSet)
				_, _, wholeBefore := snapshot(t, whole, wholeSet)
				toldSourced, a := run(sourced, sourcedSet)
				toldWhole, b := run(whole, wholeSet)
				if p.ErrorCode(a) != p.ErrorCode(b) || (a == nil) != (b == nil) {
					t.Fatalf("step %d %v: sourced answered %v, whole image %v", step, ops, a, b)
				}
				if a == nil && toldSourced != toldWhole {
					t.Fatalf("step %d %v: the transaction was told different things\nsourced: %s\nwhole image: %s", step, ops, toldSourced, toldWhole)
				}
				sourcedRows, sourcedNext, sourcedAfter := snapshot(t, sourced, sourcedSet)
				wholeRows, wholeNext, wholeAfter := snapshot(t, whole, wholeSet)
				if sourcedRows != wholeRows || sourcedNext != wholeNext {
					t.Fatalf("step %d %v: the stored datasets differ\\nsourced:\\n%s\\nwhole image:\\n%s", step, ops, sourcedRows, wholeRows)
				}
				if (sourcedBefore != sourcedAfter) != (wholeBefore != wholeAfter) {
					t.Fatalf("step %d %v: the watermark moved on one path only", step, ops)
				}
				counted(t, sourced, sourcedSet)
				counted(t, whole, wholeSet)
			}
		})
	}
}

// A write that changes nothing leaves the watermark where it was, on the
// sourced path as on the whole image: rewriting a value with itself, and an
// empty update, are not changes.
func TestASourcedWriteThatChangesNothingKeepsTheWatermark(t *testing.T) {
	s, _, dataset := fixture(t)
	three, err := p.NewValue(p.IntegerType, int64(3))
	if err != nil {
		t.Fatal(err)
	}
	var id string
	if err = s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
		set, e := tx.Insert(typeID, map[string]p.Value{"n": three})
		if e == nil {
			id = set.Records[0].ID.ID
		}
		return e
	}); err != nil {
		t.Fatal(err)
	}
	_, _, before := snapshot(t, s, dataset)
	if err = s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
		if e := tx.Update(typeID, id, map[string]p.Value{"n": three}); e != nil {
			return e
		}
		return tx.Update(typeID, id, map[string]p.Value{})
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, after := snapshot(t, s, dataset); after != before {
		t.Fatal("a write that changed nothing moved the watermark")
	}
	four, _ := p.NewValue(p.IntegerType, int64(4))
	if err = s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
		return tx.Update(typeID, id, map[string]p.Value{"n": four})
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, after := snapshot(t, s, dataset); after == before {
		t.Fatal("a write that changed a value did not move the watermark")
	}
}

// A write the sourced path hands back is run again from the whole dataset,
// and what the host wrote in the first run is rolled back first: its rows are
// written once, not twice.
func TestAHostsWritesAreNotDoubledWhenAWriteIsRunAgain(t *testing.T) {
	s, _, dataset := fixture(t)
	if _, err := s.db.ExecContext(t.Context(), `CREATE TABLE "host_events" ("n" INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	runs := 0
	err := s.WithTx(t.Context(), owner, dataset, func(tx store.Tx, sqltx *sql.Tx) error {
		runs++
		if _, e := sqltx.ExecContext(t.Context(), `INSERT INTO "host_events" ("n") VALUES (1)`); e != nil {
			return e
		}
		// A schema change, which only the whole dataset can answer: it may
		// rewrite every record of the type.
		_, e := tx.UpdateMetadata(p.Meta{ID: typeID, Fields: []p.Field{{ID: "n", MetaID: typeID, Type: p.IntegerType}, {ID: "added", MetaID: typeID, Type: p.IntegerType}}}, "")
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	var events int
	if err = s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM "host_events"`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if runs != 2 || events != 1 {
		t.Fatal("a write run again kept the first run's host rows:", runs, "runs,", events, "rows")
	}
}

// At exactly its bound, a dataset still takes a write that changes a record
// it holds and refuses one that adds a record. The sourced path measures the
// rest of the dataset from the table, and a record it touched must be counted
// once — as it is now — not also as it was.
func TestASourcedWriteAtTheBoundCountsWhatItTouchesOnce(t *testing.T) {
	s, _, dataset := fixture(t)
	limits, err := s.DatasetLimits(t.Context(), dataset)
	if err != nil {
		t.Fatal(err)
	}
	limits.MaxScannedRows = 3
	if err = s.SetDatasetLimits(t.Context(), dataset, limits); err != nil {
		t.Fatal(err)
	}
	value := func(n int64) p.Value {
		v, _ := p.NewValue(p.IntegerType, n)
		return v
	}
	for i := int64(0); i < 3; i++ {
		if err = s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
			_, e := tx.Insert(typeID, map[string]p.Value{"n": value(i)})
			return e
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
		return tx.Update(typeID, "1", map[string]p.Value{"n": value(9)})
	}); err != nil {
		t.Fatal("a dataset at its bound refused a change to a record it holds:", err)
	}
	if err = s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
		_, e := tx.Insert(typeID, map[string]p.Value{"n": value(4)})
		return e
	}); p.ErrorCode(err) != "query_result_limit_exceeded" {
		t.Fatal("a dataset at its bound took another record:", err)
	}
}

// counted checks that the record count and size a dataset keeps on its row
// are what its records actually come to.
func counted(t *testing.T, s *Store, dataset string) {
	t.Helper()
	var keptRows, keptBytes, rows, size int64
	if err := s.db.QueryRowContext(t.Context(), `SELECT "object_count","object_bytes" FROM "tessera_dataset" WHERE "dataset_id"=$1`, dataset).Scan(&keptRows, &keptBytes); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(t.Context(), `SELECT count(*),coalesce(sum("wire_bytes"),0) FROM "tessera_read_objects" WHERE "dataset_id"=$1`, dataset).Scan(&rows, &size); err != nil {
		t.Fatal(err)
	}
	if keptRows != rows || keptBytes != size {
		t.Fatalf("the dataset keeps %d records and %d bytes on its row, and holds %d and %d", keptRows, keptBytes, rows, size)
	}
}

// A bulk change reads each record once: a record a write matched with a scan
// is not fetched again to change it. And a record the write inserted, saw in
// a scan and then deleted stays deleted when the write names it again.
func TestASourcedWriteReusesWhatItScannedAndOnlyThat(t *testing.T) {
	s, _, dataset := fixture(t)
	value := func(n int64) p.Value {
		v, _ := p.NewValue(p.IntegerType, n)
		return v
	}
	if err := s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
		for i := int64(0); i < 50; i++ {
			if _, e := tx.Insert(typeID, map[string]p.Value{"n": value(i)}); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	fetched := 0
	counting := &countingFetch{count: &fetched}
	s.fetchHook = counting.observe
	defer func() { s.fetchHook = nil }()
	if err := s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
		keys, e := tx.Matching(typeID, nil)
		if e != nil {
			return e
		}
		for _, key := range keys {
			if e = tx.Update(typeID, key.ObjectID, map[string]p.Value{"n": value(7)}); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if fetched != 0 {
		t.Fatal("a bulk update fetched", fetched, "records it had already scanned")
	}
	err := s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
		set, e := tx.Insert(typeID, map[string]p.Value{"n": value(1)})
		if e != nil {
			return e
		}
		id := set.Records[0].ID.ID
		if _, e = tx.Query(typeID, p.Query{}, 1000); e != nil {
			return e
		}
		if e = tx.Delete(typeID, id); e != nil {
			return e
		}
		return tx.Update(typeID, id, map[string]p.Value{"n": value(2)})
	})
	if p.ErrorCode(err) != "not_found" {
		t.Fatal("a record the write inserted and deleted came back:", err)
	}
}

type countingFetch struct{ count *int }

func (c *countingFetch) observe() { *c.count++ }
