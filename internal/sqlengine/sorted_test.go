package sqlengine

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"testing"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// A sorted query read in SQL order gives exactly the answer that reading and
// sorting everything gives, on both engines, over values chosen to find where
// SQL and the protocol could disagree: integers at both ends of their range
// and past 2⁵³, negative zero beside zero, floats far apart in magnitude, text
// whose code-point order a locale collation would change, nulls in every field,
// ties that only the identity breaks, and identities whose byte order is not
// their numeric order.
//
// It also proves the SQL path answered. A comparison that always fell back
// would agree with itself and prove nothing.
func TestSortedReadsAgreeWithSortingEverything(t *testing.T) {
	engines := map[string]func(*testing.T) *Store{"sqlite": func(t *testing.T) *Store { s, _, _ := fixture(t); return s }}
	if os.Getenv("TESSERA_TEST_POSTGRES_DSN") != "" {
		engines["postgres"] = func(t *testing.T) *Store { postgresAdmin(t); return postgresFixture(t) }
	}
	for name, open := range engines {
		t.Run(name, func(t *testing.T) {
			s := open(t)
			dataset := sole(t, s)
			if err := s.SetAuthorization(t.Context(), dataset, owner, store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true}, View: store.FullView()}); err != nil {
				t.Fatal(err)
			}
			const parent, child = "test.sorted", "test.sorted.child"
			scale := 3
			if err := s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
				if _, e := tx.CreateMetadata(p.Meta{ID: parent, Fields: []p.Field{
					{ID: "i", MetaID: parent, Type: p.IntegerType},
					{ID: "f", MetaID: parent, Type: p.FloatType},
					{ID: "s", MetaID: parent, Type: p.TextType},
					{ID: "b", MetaID: parent, Type: p.BooleanType},
					{ID: "d", MetaID: parent, Type: p.DateTimeType},
					{ID: "x", MetaID: parent, Type: p.FixedType, Scale: &scale},
				}}); e != nil {
					return e
				}
				parentID := parent
				_, e := tx.CreateMetadata(p.Meta{ID: child, ParentID: &parentID, Fields: []p.Field{}})
				return e
			}); err != nil {
				t.Fatal(err)
			}
			integers := []int64{math.MinInt64, math.MaxInt64, 0, -1, 1, 1<<53 + 1, 1 << 53, -(1<<53 + 1), 42, 42}
			floats := []float64{math.Copysign(0, -1), 0, 1e-300, -1e300, 1e300, 1e21, 0.1, 0.30000000000000004, 1 << 53, 1.5, -1.5, 1.5}
			texts := []string{"", "a", "A", "b", "B", "é", "e", "z", "Z", "ä", "😀", "aa", "a b", "a-b", "ab", "ﬀ", "a", "Ω"}
			times := []string{"0001-01-01T00:00:00.000Z", "9999-12-31T23:59:59.999Z", "2026-09-27T12:00:00.000Z", "2026-09-27T12:00:00.001Z", "1970-01-01T00:00:00.000Z", "2026-09-27T12:00:00.000Z"}
			fixeds := []string{"10", "9.5", "-0.001", "100", "2"}
			if err := s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
				for n := 0; n < 150; n++ {
					fields := map[string]p.Value{}
					if n%9 != 0 {
						fields["i"], _ = p.NewValue(p.IntegerType, integers[n%len(integers)])
					}
					if n%8 != 1 {
						fields["f"], _ = p.NewValue(p.FloatType, floats[n%len(floats)])
					}
					if n%7 != 2 {
						fields["s"], _ = p.NewValue(p.TextType, texts[n%len(texts)])
					}
					if n%5 != 3 {
						fields["b"], _ = p.NewValue(p.BooleanType, n%3 == 0)
					}
					if n%6 != 4 {
						fields["d"], _ = p.NewValue(p.DateTimeType, times[n%len(times)])
					}
					if n%4 != 0 {
						fields["x"], _ = p.NewValue(p.FixedType, fixeds[n%len(fixeds)])
					}
					typeID := parent
					if n%3 == 1 {
						typeID = child
					}
					if _, e := tx.Insert(typeID, fields); e != nil {
						return e
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			sorts := map[string][]p.SortTerm{}
			for _, field := range []string{"i", "f", "s", "b", "d", "x"} {
				sorts[field+" asc"] = []p.SortTerm{{FieldID: field, Ascending: true}}
				sorts[field+" desc"] = []p.SortTerm{{FieldID: field, Ascending: false}}
			}
			sorts["b asc, i desc"] = []p.SortTerm{{FieldID: "b", Ascending: true}, {FieldID: "i", Ascending: false}}
			sorts["s desc, d asc, f desc"] = []p.SortTerm{{FieldID: "s", Ascending: false}, {FieldID: "d", Ascending: true}, {FieldID: "f", Ascending: false}}
			sorts["d asc, x asc"] = []p.SortTerm{{FieldID: "d", Ascending: true}, {FieldID: "x", Ascending: true}}
			filters := map[string]string{
				"everything": ``,
				"present":    `{"not":{"field":{"id":"i","exact":{"type":"null"}}}}`,
				"scattered":  `{"field":{"id":"s","exact":{"type":"text","value":"a"}}}`,
				"nothing":    `{"field":{"id":"i","exact":{"type":"integer","value":7}}}`,
			}
			scopes := map[string]*p.TypeScope{"exact": nil, "descendants": {Kind: "descendants"}}
			sqlOrdered := 0
			s.sortHook = func() { sqlOrdered++ }
			for sortName, terms := range sorts {
				for filterName, filter := range filters {
					for scopeName, scope := range scopes {
						for _, limit := range []int{1, 4, 65, 1000} {
							q := p.Query{ResultSort: terms, TypeScope: scope}
							if filter != "" {
								q.Filter = json.RawMessage(filter)
							}
							label := fmt.Sprintf("%s/%s/%s/limit=%d", sortName, filterName, scopeName, limit)
							s.unsortedReads = false
							sorted, e1 := queryVia(t, s, dataset, false, parent, q, limit)
							s.unsortedReads = true
							everything, e2 := queryVia(t, s, dataset, false, parent, q, limit)
							if p.ErrorCode(e1) != p.ErrorCode(e2) {
								t.Fatalf("%s: SQL order answered %v, sorting everything answered %v", label, e1, e2)
							}
							if sorted != everything {
								t.Fatalf("%s: the answers differ\nSQL order:  %s\neverything: %s", label, sorted, everything)
							}
						}
					}
				}
			}
			s.unsortedReads = false
			// Every sort whose terms SQL can order was answered in SQL: 12 of
			// the 15, the three naming the fixed field falling back whole.
			if want := 12 * len(filters) * len(scopes) * 4; sqlOrdered != want {
				t.Fatalf("SQL ordered %d queries, expected %d", sqlOrdered, want)
			}
			// A guest granted the parent alone asks for descendants: the child's
			// rows are outside its view, and SQL order must not bring them in.
			guest := store.Access{Principal: "guest", Authorization: "guest"}
			if err := s.SetAuthorization(t.Context(), dataset, guest, store.Authorization{Grants: store.Grants{Read: true}, View: store.SharedView([]string{parent})}); err != nil {
				t.Fatal(err)
			}
			for _, unsorted := range []bool{false, true} {
				s.unsortedReads = unsorted
				var got []p.Record
				if err := s.read(t.Context(), guest, dataset, false, func(tx store.Tx) error {
					set, e := store.ReadOnly{Tx: tx}.Query(parent, p.Query{ResultSort: []p.SortTerm{{FieldID: "i", Ascending: false}}, TypeScope: &p.TypeScope{Kind: "descendants"}}, 1000)
					got = set.Records
					return e
				}); err != nil {
					t.Fatal(err)
				}
				if len(got) != 100 {
					t.Fatalf("a guest granted the parent read %d records, not its 100, unsorted = %v", len(got), unsorted)
				}
				for _, record := range got {
					if record.MetaID != parent {
						t.Fatal("a sorted query brought a guest a record of a type outside its view")
					}
				}
			}
			s.unsortedReads = false
		})
	}
}

// A sorted query refuses a scope past the scan bound as reading the whole
// scope did, although it would read far fewer rows now: the bound is part of
// what a caller was told, and a query refused yesterday is refused today.
func TestASortedQueryKeepsTheScanBound(t *testing.T) {
	s, _, _ := fixture(t)
	dataset := sole(t, s)
	limits, err := s.DatasetLimits(t.Context(), dataset)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
		for n := 0; n < 30; n++ {
			v, _ := p.NewValue(p.IntegerType, int64(n))
			if _, e := tx.Insert(typeID, map[string]p.Value{"n": v}); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	limits.MaxScannedRows = 20
	if err = s.SetDatasetLimits(t.Context(), dataset, limits); err != nil {
		t.Fatal(err)
	}
	q := p.Query{ResultSort: []p.SortTerm{{FieldID: "n", Ascending: false}}}
	for _, unsorted := range []bool{false, true} {
		s.unsortedReads = unsorted
		if _, err = queryVia(t, s, dataset, false, typeID, q, 3); p.ErrorCode(err) != "query_result_limit_exceeded" {
			t.Fatal("a sorted query past the scan bound was answered, unsorted =", unsorted, err)
		}
	}
}
