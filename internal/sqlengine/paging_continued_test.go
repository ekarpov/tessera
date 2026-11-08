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

// walkPages reads a page sequence to its end and answers every record in
// order, rendered, with how many pages it took.
func walkPages(t *testing.T, s *Store, a store.Access, dataset, typeID string, q p.Query, size int) ([]string, int, error) {
	t.Helper()
	begun, err := s.BeginPage(t.Context(), a, dataset, typeID, q, size)
	if err != nil {
		return nil, 0, err
	}
	var rows []string
	pages := 0
	for cursor := begun.Cursor; cursor != ""; {
		raw, err := s.NextPage(t.Context(), a, dataset, cursor)
		if err != nil {
			return nil, pages, err
		}
		pages++
		var page struct {
			Records []json.RawMessage `json:"records"`
			Next    *string           `json:"next_cursor"`
		}
		if err = json.Unmarshal(raw, &page); err != nil {
			return nil, pages, err
		}
		for _, r := range page.Records {
			rows = append(rows, string(r))
		}
		cursor = ""
		if page.Next != nil {
			cursor = *page.Next
		}
	}
	return rows, pages, nil
}

func pagingEngines() map[string]func(*testing.T) *Store {
	engines := map[string]func(*testing.T) *Store{"sqlite": func(t *testing.T) *Store { s, _, _ := fixture(t); return s }}
	if os.Getenv("TESSERA_TEST_POSTGRES_DSN") != "" {
		engines["postgres"] = func(t *testing.T) *Store { postgresAdmin(t); return postgresFixture(t) }
	}
	return engines
}

// seedPaged gives a dataset a parent and child type with values chosen to
// find where continuing a sequence and snapshotting it could disagree: ties
// that only the identity breaks, nulls in each field, extreme numbers, text a
// locale would reorder, and more rows than one batch.
func seedPaged(t *testing.T, s *Store, dataset string) {
	t.Helper()
	const parent, child = "test.paged", "test.paged.child"
	scale := 2
	if err := s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
		if _, e := tx.CreateMetadata(p.Meta{ID: parent, Fields: []p.Field{
			{ID: "i", MetaID: parent, Type: p.IntegerType},
			{ID: "f", MetaID: parent, Type: p.FloatType},
			{ID: "s", MetaID: parent, Type: p.TextType},
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
	integers := []int64{math.MinInt64, math.MaxInt64, 0, -1, 1 << 53, 42, 42, 7}
	floats := []float64{math.Copysign(0, -1), 0, 1e300, -1e-300, 0.1, 1.5, 1.5}
	texts := []string{"", "a", "A", "é", "z", "Z", "a", "😀"}
	if err := s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
		for n := 0; n < 160; n++ {
			fields := map[string]p.Value{}
			if n%9 != 0 {
				fields["i"], _ = p.NewValue(p.IntegerType, integers[n%len(integers)])
			}
			if n%7 != 1 {
				fields["f"], _ = p.NewValue(p.FloatType, floats[n%len(floats)])
			}
			if n%5 != 2 {
				fields["s"], _ = p.NewValue(p.TextType, texts[n%len(texts)])
			}
			if n%4 != 0 {
				fields["x"], _ = p.NewValue(p.FixedType, fmt.Sprintf("%d.%d", n%6, n%9+1))
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
}

// A sequence read page by page, each computed when it is asked for, gives
// exactly the rows the snapshot did, in the same order, on both engines —
// under identity order, every sort the storage orders, the sorts it does not
// (which stay snapshots), filters, scopes and page sizes that cross a batch.
func TestContinuedPagingAgreesWithTheSnapshot(t *testing.T) {
	for name, open := range pagingEngines() {
		t.Run(name, func(t *testing.T) {
			s := open(t)
			dataset := sole(t, s)
			if err := s.SetAuthorization(t.Context(), dataset, owner, store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true}, View: store.FullView()}); err != nil {
				t.Fatal(err)
			}
			seedPaged(t, s, dataset)
			sorts := map[string][]p.SortTerm{"identity": nil}
			for _, field := range []string{"i", "f", "s", "x"} {
				sorts[field+" asc"] = []p.SortTerm{{FieldID: field, Ascending: true}}
				sorts[field+" desc"] = []p.SortTerm{{FieldID: field, Ascending: false}}
			}
			sorts["s asc, i desc"] = []p.SortTerm{{FieldID: "s", Ascending: true}, {FieldID: "i", Ascending: false}}
			sorts["f desc, s desc"] = []p.SortTerm{{FieldID: "f", Ascending: false}, {FieldID: "s", Ascending: false}}
			filters := map[string]string{
				"everything": ``,
				"present":    `{"not":{"field":{"id":"i","exact":{"type":"null"}}}}`,
				"scattered":  `{"field":{"id":"s","exact":{"type":"text","value":"a"}}}`,
				"nothing":    `{"field":{"id":"i","exact":{"type":"integer","value":-99}}}`,
			}
			scopes := map[string]*p.TypeScope{"exact": nil, "descendants": {Kind: "descendants"}}
			for sortName, terms := range sorts {
				for filterName, filter := range filters {
					for scopeName, scope := range scopes {
						for _, size := range []int{1, 7, 70, 1000} {
							// A page of one crosses every boundary there is,
							// and a walk of one row per page is slow: once
							// per sort is enough.
							if size == 1 && (filter != "" || scope != nil) {
								continue
							}
							q := p.Query{ResultSort: terms, TypeScope: scope}
							if filter != "" {
								q.Filter = json.RawMessage(filter)
							}
							label := fmt.Sprintf("%s/%s/%s/size=%d", sortName, filterName, scopeName, size)
							s.snapshotPages = false
							continued, pages, e1 := walkPages(t, s, owner, dataset, "test.paged", q, size)
							s.snapshotPages = true
							snapshot, _, e2 := walkPages(t, s, owner, dataset, "test.paged", q, size)
							s.snapshotPages = false
							if p.ErrorCode(e1) != p.ErrorCode(e2) {
								t.Fatalf("%s: continued answered %v, the snapshot %v", label, e1, e2)
							}
							if fmt.Sprint(continued) != fmt.Sprint(snapshot) {
								t.Fatalf("%s: the sequences differ: continued %d pages, %d rows; snapshot %d rows", label, pages, len(continued), len(snapshot))
							}
							if size == 1 && len(snapshot) > 1 && pages < len(snapshot) {
								t.Fatalf("%s: %d rows came in %d pages of one", label, len(snapshot), pages)
							}
						}
					}
				}
			}
			if _, err := s.db.Exec(`DELETE FROM "tessera_sequences"`); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// What a continued sequence owes while the data moves under it, as the
// protocol states it: a row that stays put is returned once whatever happens
// to the others; no row is returned twice even when its sort value moves past
// the position; a cursor asked for again returns the same page; and a schema
// change ends the sequence rather than serving pages it cannot describe.
func TestAContinuedSequenceKeepsItsGuaranteesWhileTheDataMoves(t *testing.T) {
	for name, open := range pagingEngines() {
		t.Run(name, func(t *testing.T) {
			s := open(t)
			dataset := sole(t, s)
			if err := s.SetAuthorization(t.Context(), dataset, owner, store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true}, View: store.FullView()}); err != nil {
				t.Fatal(err)
			}
			if err := s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
				_, e := tx.CreateMetadata(p.Meta{ID: "test.moving", Fields: []p.Field{{ID: "n", MetaID: "test.moving", Type: p.IntegerType}}})
				return e
			}); err != nil {
				t.Fatal(err)
			}
			ids := make([]string, 20)
			if err := s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
				for n := range ids {
					v, _ := p.NewValue(p.IntegerType, int64(n))
					set, e := tx.Insert("test.moving", map[string]p.Value{"n": v})
					if e != nil {
						return e
					}
					ids[n] = set.Records[0].ID.ID
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			q := p.Query{ResultSort: []p.SortTerm{{FieldID: "n", Ascending: true}}}
			begun, err := s.BeginPage(t.Context(), owner, dataset, "test.moving", q, 5)
			if err != nil {
				t.Fatal(err)
			}
			first, err := s.NextPage(t.Context(), owner, dataset, begun.Cursor)
			if err != nil {
				t.Fatal(err)
			}
			var page struct {
				Records []p.Record `json:"records"`
				Next    *string    `json:"next_cursor"`
			}
			if err = json.Unmarshal(first, &page); err != nil || page.Next == nil {
				t.Fatal("the first page did not continue:", err)
			}
			// The first page held n = 0..4. Row 0 moves ahead of everything;
			// rows 10 and 11 are deleted; nothing else changes.
			if err = s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
				far, _ := p.NewValue(p.IntegerType, int64(100))
				if e := tx.Update("test.moving", ids[0], map[string]p.Value{"n": far}); e != nil {
					return e
				}
				if e := tx.Delete("test.moving", ids[10]); e != nil {
					return e
				}
				return tx.Delete("test.moving", ids[11])
			}); err != nil {
				t.Fatal(err)
			}
			seen := map[string]int{}
			for _, r := range page.Records {
				seen[r.ID.ID]++
			}
			second := *page.Next
			for cursor := second; cursor != ""; {
				raw, err := s.NextPage(t.Context(), owner, dataset, cursor)
				if err != nil {
					t.Fatal(err)
				}
				if cursor == second {
					again, err := s.NextPage(t.Context(), owner, dataset, cursor)
					if err != nil || string(again) != string(raw) {
						t.Fatal("a cursor asked for again did not replay its page:", err)
					}
					// Two requests racing for a pending page: the one that
					// arrives second finds it computed, and serves it.
					raced, err := s.continuePage(t.Context(), owner, dataset, cursor)
					if err != nil || string(raced) != string(raw) {
						t.Fatal("a page computed by another request was computed again:", err)
					}
				}
				var next struct {
					Records []p.Record `json:"records"`
					Next    *string    `json:"next_cursor"`
				}
				if err = json.Unmarshal(raw, &next); err != nil {
					t.Fatal(err)
				}
				for _, r := range next.Records {
					seen[r.ID.ID]++
				}
				cursor = ""
				if next.Next != nil {
					cursor = *next.Next
				}
			}
			for id, count := range seen {
				if count > 1 {
					t.Fatal("a row was returned twice:", id)
				}
			}
			for n, id := range ids {
				if n == 0 || n == 10 || n == 11 {
					continue
				}
				if seen[id] != 1 {
					t.Fatal("a row that stayed put was skipped:", n)
				}
			}

			// A schema change ends a sequence that cannot describe its pages.
			begun, err = s.BeginPage(t.Context(), owner, dataset, "test.moving", p.Query{}, 5)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := s.NextPage(t.Context(), owner, dataset, begun.Cursor)
			if err != nil || json.Unmarshal(raw, &page) != nil || page.Next == nil {
				t.Fatal(err)
			}
			if err = s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
				_, e := tx.UpdateMetadata(p.Meta{ID: "test.moving", Fields: []p.Field{
					{ID: "n", MetaID: "test.moving", Type: p.IntegerType},
					{ID: "added", MetaID: "test.moving", Type: p.TextType},
				}}, "")
				return e
			}); err != nil {
				t.Fatal(err)
			}
			if _, err = s.NextPage(t.Context(), owner, dataset, *page.Next); p.ErrorCode(err) != "cursor_expired" {
				t.Fatal("a sequence went on after its schema changed:", err)
			}
		})
	}
}
