package sqlengine

import (
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// TestCapacity measures what everyday operations cost as a dataset grows, on
// both engines, for the sizing guidance in the self-hosting guide. It is not
// a check and is skipped unless TESSERA_CAPACITY is set; TESSERA_CAPACITY_SIZES
// overrides the sizes, comma-separated, and TESSERA_CAPACITY_UNSORTED sorts
// every sorted query in the evaluator as before. Run with -v to see the table:
//
//	TESSERA_CAPACITY=1 go test ./internal/sqlengine -run TestCapacity -v -timeout 60m
func TestCapacity(t *testing.T) {
	if os.Getenv("TESSERA_CAPACITY") == "" {
		t.Skip("set TESSERA_CAPACITY to measure")
	}
	sizes := []int{1000, 10000, 100000, 500000}
	if raw := os.Getenv("TESSERA_CAPACITY_SIZES"); raw != "" {
		sizes = nil
		for _, field := range strings.Split(raw, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(field))
			if err != nil {
				t.Fatal(err)
			}
			sizes = append(sizes, n)
		}
	}
	engines := []struct {
		name string
		open func(*testing.T) *Store
	}{{"SQLite", func(t *testing.T) *Store { s, _, _ := fixture(t); return s }}}
	if os.Getenv("TESSERA_TEST_POSTGRES_DSN") != "" {
		engines = append(engines, struct {
			name string
			open func(*testing.T) *Store
		}{"PostgreSQL", func(t *testing.T) *Store { postgresAdmin(t); return postgresFixture(t) }})
	}
	t.Log("| Engine | Records | Insert one | Update one | Read one | Query ten | Newest ten | Aggregate all | Start page set (50 a page) |")
	t.Log("| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |")
	for _, engine := range engines {
		s := engine.open(t)
		// TESSERA_CAPACITY_UNSORTED measures sorted queries as they were
		// answered before SQL ordered them, for a before-and-after.
		s.unsortedReads = os.Getenv("TESSERA_CAPACITY_UNSORTED") != ""
		dataset := sole(t, s)
		if err := s.SetAuthorization(t.Context(), dataset, owner, store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true}, View: store.FullView()}); err != nil {
			t.Fatal(err)
		}
		limits, err := s.DatasetLimits(t.Context(), dataset)
		if err != nil {
			t.Fatal(err)
		}
		limits.MaxScannedRows, limits.MaxMaterializedBytes = 1<<21, 1<<30
		if err = s.SetDatasetLimits(t.Context(), dataset, limits); err != nil {
			t.Fatal(err)
		}
		if err = s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
			_, e := tx.CreateMetadata(p.Meta{ID: typeID, Fields: []p.Field{{ID: "n", MetaID: typeID, Type: p.IntegerType}}})
			return e
		}); err != nil && p.ErrorCode(err) != "meta_type_already_exists" {
			t.Fatal(err)
		}
		value := func(n int) p.Value {
			v, _ := p.NewValue(p.IntegerType, int64(n))
			return v
		}
		held := 0
		for _, size := range sizes {
			for held < size {
				batch := min(5000, size-held)
				if err = s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
					for i := 0; i < batch; i++ {
						if _, e := tx.Insert(typeID, map[string]p.Value{"n": value(held + i)}); e != nil {
							return e
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				held += batch
			}
			measure := func(runs int, op func(i int) error) string {
				took := make([]time.Duration, 0, runs)
				for i := 0; i < runs; i++ {
					start := time.Now()
					if err := op(i); err != nil {
						t.Fatal(err)
					}
					took = append(took, time.Since(start))
				}
				slices.Sort(took)
				return took[len(took)/2].Round(10 * time.Microsecond).String()
			}
			insert := measure(21, func(int) error {
				return s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
					_, e := tx.Insert(typeID, map[string]p.Value{"n": value(-1)})
					return e
				})
			})
			held += 21
			update := measure(21, func(i int) error {
				return s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
					return tx.Update(typeID, strconv.Itoa(1+i*(size/21)), map[string]p.Value{"n": value(i)})
				})
			})
			read := measure(21, func(i int) error {
				return s.ReadRecord(t.Context(), owner, dataset, func(tx store.Tx) error {
					_, e := tx.Object(typeID, strconv.Itoa(1+i*(size/21)), nil)
					return e
				})
			})
			query := measure(21, func(int) error {
				return s.Read(t.Context(), owner, dataset, func(tx store.Tx) error {
					_, e := tx.Query(typeID, p.Query{}, 10)
					return e
				})
			})
			// The list a person looks at: the ten with the greatest value,
			// which is what an app showing its most recent records asks.
			newest := measure(5, func(int) error {
				return s.Read(t.Context(), owner, dataset, func(tx store.Tx) error {
					_, e := tx.Query(typeID, p.Query{ResultSort: []p.SortTerm{{FieldID: "n", Ascending: false}}}, 10)
					return e
				})
			})
			aggregate := measure(5, func(int) error {
				return s.Read(t.Context(), owner, dataset, func(tx store.Tx) error {
					_, e := tx.Aggregate(typeID, "n", "", nil)
					return e
				})
			})
			page := "—"
			if size <= 100000 {
				page = measure(3, func(int) error {
					start, e := s.BeginPage(t.Context(), owner, dataset, typeID, p.Query{}, 50)
					if e != nil {
						return e
					}
					return s.EndPage(t.Context(), owner, dataset, start.Cursor)
				})
			}
			t.Logf("| %s | %d | %s | %s | %s | %s | %s | %s | %s |", engine.name, size, insert, update, read, query, newest, aggregate, page)
		}
	}
}
