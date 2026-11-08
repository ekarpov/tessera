package sqlengine

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/ekarpov/tessera/internal/recordstate"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// Reading only as far as the answer needs has to give exactly the answer that
// reading everything gives. Every query below is answered twice over the same
// data — once streaming from storage and stopping early where it may, once
// from the complete image this engine used to load for every read — and the
// two must agree record for record and value for value.
//
// Differential rather than hand-computed on purpose. The failure this guards
// against is not a query answered wrongly in some way a person would predict;
// it is the streaming path returning rows that are each genuine matches but
// are not the *first* matches, which looks right to anybody checking by eye.
func TestStreamingReadsAgreeWithReadingEverything(t *testing.T) {
	s, err := Open(t.Context(), filepath.Join(t.TempDir(), "pushdown.db"), p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	dataset, err := s.SoleDataset(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetAuthorization(t.Context(), dataset.ID, owner, store.Authorization{
		Grants: store.Grants{Read: true, Write: true, Schema: true}, View: store.FullView(),
	}); err != nil {
		t.Fatal(err)
	}
	const parent, child = "test.pushdown", "test.pushdown.child"
	scale := 2
	if err = s.Transact(t.Context(), owner, dataset.ID, func(tx store.Tx) error {
		if _, e := tx.CreateMetadata(p.Meta{ID: parent, Fields: []p.Field{
			{ID: "n", MetaID: parent, Type: p.IntegerType},
			{ID: "label", MetaID: parent, Type: p.TextType},
			{ID: "amount", MetaID: parent, Type: p.FixedType, Scale: &scale},
		}}); e != nil {
			return e
		}
		parentID := parent
		_, e := tx.CreateMetadata(p.Meta{ID: child, ParentID: &parentID, Fields: []p.Field{}})
		return e
	}); err != nil {
		t.Fatal(err)
	}
	// Interleaved types, a null in every field somewhere, and values chosen so
	// that filters match scattered rows rather than a prefix — a filter that
	// only ever matched the first rows would let a wrong early stop pass.
	if err = s.Transact(t.Context(), owner, dataset.ID, func(tx store.Tx) error {
		for i := 0; i < 240; i++ {
			fields := map[string]p.Value{}
			if i%7 != 0 {
				fields["n"], _ = p.NewValue(p.IntegerType, int64((i*37)%101))
			}
			if i%11 != 0 {
				fields["label"], _ = p.NewValue(p.TextType, fmt.Sprintf("L%d", i%5))
			}
			if i%13 != 0 {
				fields["amount"], _ = p.NewValue(p.FixedType, fmt.Sprintf("%d.%d", i%9, i%10+1))
			}
			typeID := parent
			if i%3 == 0 {
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

	filters := map[string]string{
		"everything":  ``,
		"selective":   `{"field":{"id":"n","exact":{"type":"integer","value":42}}}`,
		"scattered":   `{"field":{"id":"label","exact":{"type":"text","value":"L3"}}}`,
		"nothing":     `{"field":{"id":"n","exact":{"type":"integer","value":-1}}}`,
		"absent":      `{"field":{"id":"label","exact":{"type":"null"}}}`,
		"exact money": `{"field":{"id":"amount","exact":{"type":"fixed","value":"4.5"}}}`,
	}
	scopes := map[string]*p.TypeScope{
		"exact":       nil,
		"descendants": {Kind: "descendants"},
		"child only":  {Kind: "descendants", AllowedConcreteTypeIDs: []string{child}},
	}
	sorts := map[string][]p.SortTerm{
		"identity":   nil,
		"n asc":      {{FieldID: "n", Ascending: true}},
		"label desc": {{FieldID: "label", Ascending: false}},
	}
	compared := 0
	for filterName, filter := range filters {
		for scopeName, typeScope := range scopes {
			for sortName, sortTerms := range sorts {
				for _, limit := range []int{1, 3, 17, 1000} {
					q := p.Query{Filter: json.RawMessage(filter), TypeScope: typeScope, ResultSort: sortTerms}
					if filter == "" {
						q.Filter = nil
					}
					name := fmt.Sprintf("%s/%s/%s/limit=%d", filterName, scopeName, sortName, limit)
					streamed, e1 := queryVia(t, s, dataset.ID, false, parent, q, limit)
					whole, e2 := queryVia(t, s, dataset.ID, true, parent, q, limit)
					if p.ErrorCode(e1) != p.ErrorCode(e2) {
						t.Fatalf("%s: streaming answered %v, reading everything answered %v", name, e1, e2)
					}
					if streamed != whole {
						t.Fatalf("%s: the answers differ\nstreaming: %s\neverything: %s", name, streamed, whole)
					}
					compared++
				}
			}
		}
	}
	if compared != 6*3*3*4 {
		t.Fatal("the matrix ran", compared, "cases")
	}
}

// queryVia answers one query through the streaming read path or the complete
// image, and renders the records so two answers can be compared exactly.
func queryVia(t *testing.T, s *Store, dataset string, everything bool, typeID string, q p.Query, limit int) (string, error) {
	t.Helper()
	var rendered string
	err := s.read(t.Context(), owner, dataset, everything, func(tx store.Tx) error {
		set, e := store.ReadOnly{Tx: tx}.Query(typeID, q, limit)
		if e != nil {
			return e
		}
		raw, e := json.Marshal(set.Records)
		rendered = string(raw)
		return e
	})
	return rendered, err
}

// A scan read in batches returns every record once, in identity order, across
// the batch boundaries — on both engines, since PostgreSQL's order is asked for
// explicitly and a batch resumes after the last identity in that order.
func TestAScanReadInBatchesMissesAndRepeatsNothing(t *testing.T) {
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
			const total = scanBatchFirst + 4*scanBatchFirst + 16*scanBatchFirst + 7
			if err := s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
				_, e := tx.CreateMetadata(p.Meta{ID: typeID, Fields: []p.Field{{ID: "n", MetaID: typeID, Type: p.IntegerType}}})
				return e
			}); err != nil && p.ErrorCode(err) != "meta_type_already_exists" {
				t.Fatal(err)
			}
			if err := s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
				one, _ := p.NewValue(p.IntegerType, int64(1))
				for i := 0; i < total; i++ {
					if _, e := tx.Insert(typeID, map[string]p.Value{"n": one}); e != nil {
						return e
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			var ids []string
			if err := s.scanObjects(t.Context(), mustReadTx(t, s), dataset, []string{typeID}, func(id string, _ recordstate.Row) (bool, error) {
				ids = append(ids, id)
				return true, nil
			}); err != nil {
				t.Fatal(err)
			}
			if len(ids) != total {
				t.Fatal("a batched scan returned", len(ids), "records of", total)
			}
			for i := 1; i < len(ids); i++ {
				if ids[i] <= ids[i-1] {
					t.Fatal("a batched scan repeated or reordered records at", i, ids[i-1], ids[i])
				}
			}
		})
	}
}

func mustReadTx(t *testing.T, s *Store) *sql.Tx {
	t.Helper()
	tx, err := s.reader.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback() })
	return tx
}
