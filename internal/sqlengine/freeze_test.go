package sqlengine

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"

	"github.com/ekarpov/tessera/internal/recordstate"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// The reference image contains only records present at the cut. It carries
// no freeze logic, so agreement cannot be produced by the same wrong cut on
// both sides. Decimal boundaries also distinguish sequence order from ID order.
func TestFrozenReadsEqualThePrefreezeDataset(t *testing.T) {
	engines := map[string]func(*testing.T) *Store{"sqlite": func(t *testing.T) *Store { s, _, _ := fixture(t); return s }}
	if os.Getenv("TESSERA_TEST_POSTGRES_DSN") != "" {
		engines["postgres"] = func(t *testing.T) *Store { postgresAdmin(t); return postgresFixture(t) }
	}
	for name, open := range engines {
		t.Run(name, func(t *testing.T) {
			s := open(t)
			dataset := sole(t, s)
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			must(s.SetAuthorization(t.Context(), dataset, owner, store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true}, View: store.FullView()}))
			const typ = "test.frozen"
			scale := 2
			must(s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
				_, err := tx.CreateMetadata(p.Meta{ID: typ, Fields: []p.Field{{ID: "n", MetaID: typ, Type: p.IntegerType}, {ID: "x", MetaID: typ, Type: p.FixedType, Scale: &scale}}})
				return err
			}))
			insert := func(start, end int) {
				must(s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
					for i := start; i < end; i++ {
						n, _ := p.NewValue(p.IntegerType, int64((i*7)%13))
						x, _ := p.NewValue(p.FixedType, fmt.Sprintf("%d.25", i%5))
						if _, err := tx.Insert(typ, map[string]p.Value{"n": n, "x": x}); err != nil {
							return err
						}
					}
					return nil
				}))
			}
			insert(0, 11)
			cut, err := s.ReaderCut(t.Context(), dataset)
			must(err)
			if cut != 12 {
				t.Fatalf("unexpected cut %d", cut)
			}
			sqltx, err := s.reader.BeginTx(t.Context(), nil)
			must(err)
			h, err := s.load(t.Context(), sqltx, owner, dataset, true)
			must(err)
			must(sqltx.Rollback())
			reference, err := recordstate.Restore(h.image, h.limits, owner, store.Authorization{Grants: store.Grants{Read: true}, View: store.FullView()}, nil)
			must(err)
			insert(11, 25)
			guest := store.Access{Principal: "frozen", Authorization: "read"}
			for _, view := range []store.View{store.FullView(), store.SharedView([]string{typ})} {
				must(s.SetAuthorization(t.Context(), dataset, guest, store.Authorization{Grants: store.Grants{Read: true}, View: view.BeforeSequence(cut)}))
				for _, sort := range [][]p.SortTerm{nil, {{FieldID: "n", Ascending: true}}, {{FieldID: "n", Ascending: false}}, {{FieldID: "x", Ascending: true}}} {
					for _, filter := range []string{"", `{"field":{"id":"n","range":{"lower":{"type":"integer","value":5}}}}`} {
						q := p.Query{ResultSort: sort, Filter: json.RawMessage(filter)}
						var expected p.RecordSet
						must(reference.Read(t.Context(), owner, dataset, func(tx store.Tx) error { var e error; expected, e = tx.Query(typ, q, 100); return e }))
						for _, size := range []int{1, 3, 8, 30} {
							var actual p.RecordSet
							must(s.Read(t.Context(), guest, dataset, func(tx store.Tx) error { var e error; actual, e = tx.Query(typ, q, size); return e }))
							want := expected.Records
							if len(want) > size {
								want = want[:size]
							}
							if !reflect.DeepEqual(actual.Records, want) {
								t.Fatalf("query sort=%v filter=%s size=%d: got %v want %v", sort, filter, size, actual.Records, want)
							}
							start, e := s.BeginPage(t.Context(), guest, dataset, typ, q, size)
							must(e)
							records := []p.Record{}
							cursor := start.Cursor
							for cursor != "" {
								raw, e := s.NextPage(t.Context(), guest, dataset, cursor)
								must(e)
								var page struct {
									p.RecordSet
									Next *string `json:"next_cursor"`
								}
								must(json.Unmarshal(raw, &page))
								records = append(records, page.Records...)
								cursor = ""
								if page.Next != nil {
									cursor = *page.Next
								}
							}
							if !reflect.DeepEqual(records, expected.Records) {
								t.Fatalf("pages sort=%v filter=%s size=%d: got %v want %v", sort, filter, size, records, expected.Records)
							}
						}
					}
				}
			}
			// A reader that also contributes reaches the sourced write path. Its
			// newly inserted row is outside its reading view even in that transaction.
			must(s.SetAuthorization(t.Context(), dataset, guest, store.Authorization{Grants: store.Grants{Read: true, Insert: true}, View: store.SharedView([]string{typ}).WithInserts([]string{typ}).BeforeSequence(cut)}))
			must(s.Transact(t.Context(), guest, dataset, func(tx store.Tx) error {
				n, _ := p.NewValue(p.IntegerType, int64(999))
				if _, e := tx.Insert(typ, map[string]p.Value{"n": n}); e != nil {
					return e
				}
				set, e := tx.Query(typ, p.Query{}, 100)
				if e == nil && len(set.Records) != 11 {
					t.Fatal("sourced write scan escaped the cut", set.Records)
				}
				return e
			}))
			// Hidden rows cannot make the SQL sort admission report a larger scope.
			limits := h.limits
			limits.MaxScannedRows = 11
			must(s.SetDatasetLimits(t.Context(), dataset, limits))
			must(s.Read(t.Context(), guest, dataset, func(tx store.Tx) error {
				_, e := tx.Query(typ, p.Query{ResultSort: []p.SortTerm{{FieldID: "n", Ascending: true}}}, 1)
				return e
			}))
		})
	}
}
