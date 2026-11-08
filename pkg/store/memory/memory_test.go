package memory_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
	"github.com/ekarpov/tessera/pkg/store/memory"
	"sync"
	"testing"
)

var owner = store.Access{Principal: "synthetic", Authorization: "owner"}

const typeID = "test.record"

// held names the one dataset a store holds. Tests that are not about routing
// name it this way rather than carrying an identity through every call. It goes
// through the composition call, not the listing, because a grant has to be
// installed on a dataset before any listing can report one.
func held(t *testing.T, s *memory.Store) string {
	t.Helper()
	dataset, err := s.SoleDataset(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return dataset.ID
}

func fixture(t *testing.T) *memory.Store {
	t.Helper()
	s, err := memory.New(p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	err = s.SetAuthorization(t.Context(), held(t, s), owner, store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true}, View: store.FullView()})
	if err != nil {
		t.Fatal(err)
	}
	err = s.Transact(t.Context(), owner, held(t, s), func(tx store.Tx) error {
		_, err := tx.CreateMetadata(p.Meta{ID: typeID, Name: "Synthetic", Fields: []p.Field{{ID: "amount", MetaID: typeID, Type: p.IntegerType}}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func value(n int64) p.Value { v, _ := p.NewValue(p.IntegerType, n); return v }
func insert(t *testing.T, s *memory.Store) string {
	t.Helper()
	var id string
	err := s.Transact(t.Context(), owner, held(t, s), func(tx store.Tx) error {
		r, e := tx.Insert(typeID, map[string]p.Value{"amount": value(1)})
		if e == nil {
			id = r.Records[0].ID.ID
		}
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}
func checkValue(t *testing.T, s *memory.Store, id string, n int64) {
	t.Helper()
	err := s.Transact(t.Context(), owner, held(t, s), func(tx store.Tx) error {
		r, e := tx.Object(typeID, id, nil)
		if e != nil {
			return e
		}
		equal, e := p.Equal(r.Records[0].Values[0], value(n))
		if e != nil || !equal {
			return fmt.Errorf("unexpected value")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
func TestRollbackAndTransactionLifetime(t *testing.T) {
	for _, mode := range []string{"callback", "swallowed", "cancelled", "panic"} {
		t.Run(mode, func(t *testing.T) {
			s := fixture(t)
			dataset := held(t, s)
			id := insert(t, s)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var escaped store.Tx
			invoke := func() (err error) {
				defer func() {
					if recover() != nil {
						err = errors.New("panic recovered")
					}
				}()
				return s.Transact(ctx, owner, dataset, func(tx store.Tx) error {
					escaped = tx
					if e := tx.Update(typeID, id, map[string]p.Value{"amount": value(2)}); e != nil {
						return e
					}
					switch mode {
					case "callback":
						return errors.New("host abort")
					case "swallowed":
						_ = tx.Update(typeID, id, map[string]p.Value{"wrong": value(3)})
					case "cancelled":
						cancel()
					case "panic":
						panic("host abort")
					}
					return nil
				})
			}
			if err := invoke(); err == nil {
				t.Fatal("transaction committed")
			}
			checkValue(t, s, id, 1)
			if _, err := escaped.Object(typeID, id, nil); p.ErrorCode(err) != "internal_error" {
				t.Fatal("escaped transaction usable", err)
			}
		})
	}
}
func TestAtomicBatchRollsBack(t *testing.T) {
	s := fixture(t)
	a, b := insert(t, s), insert(t, s)
	err := s.Transact(t.Context(), owner, held(t, s), func(tx store.Tx) error {
		if err := tx.Update(typeID, a, map[string]p.Value{"amount": value(2)}); err != nil {
			return err
		}
		if err := tx.Expect(typeID, b, map[string]p.Value{"amount": value(9)}, nil); err != nil {
			return err
		}
		return tx.Update(typeID, b, map[string]p.Value{"amount": value(3)})
	})
	if p.ErrorCode(err) != "conflict" {
		t.Fatal(err)
	}
	checkValue(t, s, a, 1)
	checkValue(t, s, b, 1)
}
func TestAccessViewsAndRevocation(t *testing.T) {
	s := fixture(t)
	visible, hidden := insert(t, s), insert(t, s)
	a := store.Access{Principal: owner.Principal, Authorization: "limited"}
	grant := store.Authorization{Grants: store.Grants{Read: true}, View: store.ObjectView(store.ObjectKey{TypeID: typeID, ObjectID: visible})}
	if err := s.SetAuthorization(t.Context(), held(t, s), a, grant); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{hidden, "absent"} {
		err := s.Transact(t.Context(), a, held(t, s), func(tx store.Tx) error { _, e := tx.Object(typeID, id, nil); return e })
		if p.ErrorCode(err) != "unavailable" {
			t.Fatal(err)
		}
	}
	err := s.Transact(t.Context(), a, held(t, s), func(tx store.Tx) error {
		if err := tx.Update(typeID, visible, map[string]p.Value{"amount": value(2)}); err != nil {
			return err
		}
		return tx.Update(typeID, hidden, map[string]p.Value{"amount": value(3)})
	})
	if p.ErrorCode(err) != "permission_denied" {
		t.Fatal(err)
	}
	checkValue(t, s, visible, 1)
	err = s.Transact(t.Context(), a, held(t, s), func(tx store.Tx) error { _, e := tx.CreateMetadata(p.Meta{ID: "test.other"}); return e })
	if p.ErrorCode(err) != "permission_denied" {
		t.Fatal(err)
	}
	if err = s.Revoke(t.Context(), held(t, s), a); err != nil {
		t.Fatal(err)
	}
	for _, access := range []store.Access{a, {}, {Principal: owner.Principal, Authorization: "unregistered"}} {
		called := false
		err = s.Transact(t.Context(), access, held(t, s), func(store.Tx) error { called = true; return nil })
		// No usable grant conceals the dataset rather than refusing within it.
		if called || p.ErrorCode(err) != "unavailable" {
			t.Fatal("failed open", err)
		}
	}
	checkValue(t, s, visible, 1)
	if err = s.SetAuthorization(t.Context(), held(t, s), a, store.Authorization{Grants: grant.Grants}); p.ErrorCode(err) != "permission_denied" {
		t.Fatal("zero view accepted")
	}
}
func TestConcurrentIdentityAndCancellation(t *testing.T) {
	s := fixture(t)
	dataset := held(t, s)
	const n = 24
	ids := make(chan string, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
				r, e := tx.Insert(typeID, map[string]p.Value{})
				if e == nil {
					ids <- r.Records[0].ID.ID
				}
				return e
			})
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for id := range ids {
		if seen[id] {
			t.Fatal("identity reused")
		}
		seen[id] = true
	}
	if len(seen) != n {
		t.Fatal(len(seen))
	}
	err := s.Transact(t.Context(), owner, dataset, func(store.Tx) error {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, e := s.Info(ctx, owner, dataset); !errors.Is(e, context.Canceled) {
			return fmt.Errorf("cancelled lock wait failed")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestInheritanceConversionAndProjection(t *testing.T) {
	s := fixture(t)
	parent := typeID
	child := "test.child"
	err := s.Transact(t.Context(), owner, held(t, s), func(tx store.Tx) error {
		_, e := tx.CreateMetadata(p.Meta{ID: child, ParentID: &parent, Fields: []p.Field{{ID: "fraction", MetaID: child, Type: p.FloatType}}})
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	err = s.Transact(t.Context(), owner, held(t, s), func(tx store.Tx) error {
		r, e := tx.Insert(child, map[string]p.Value{"amount": value(1), "fraction": value(9007199254740994)})
		if e != nil {
			return e
		}
		id := r.Records[0].ID.ID
		r, e = tx.Object(child, id, []string{"fraction", "amount"})
		if e != nil {
			return e
		}
		if len(r.Meta.Fields) != 2 || r.Meta.Fields[0].ID != "fraction" || r.Records[0].Values[0].Type() != p.FloatType {
			return fmt.Errorf("inheritance or projection failed")
		}
		// Returned schema and records are independent of the engine's state.
		r.Meta.Fields[0].ID = "changed"
		*r.Meta.ParentID = "changed"
		r, e = tx.Object(child, id, []string{})
		if e != nil {
			return e
		}
		if len(r.Meta.Fields) != 0 || len(r.Records[0].Values) != 0 {
			return fmt.Errorf("empty projection was expanded")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = s.Transact(t.Context(), owner, held(t, s), func(tx store.Tx) error {
		_, e := tx.Insert(child, map[string]p.Value{"fraction": value(9007199254740993)})
		return e
	})
	if p.ErrorCode(err) != "invalid_field_value" {
		t.Fatal("rounded unrepresentable integer", err)
	}
}
func TestDeclaredBoundsAndUnsupportedMedia(t *testing.T) {
	limits := p.DevelopmentLimits()
	limits.MaxProjectedFields = 1
	limits.MaxTextBytes = 3
	s, err := memory.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetAuthorization(t.Context(), held(t, s), owner, store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true}, View: store.FullView()}); err != nil {
		t.Fatal(err)
	}
	err = s.Transact(t.Context(), owner, held(t, s), func(tx store.Tx) error {
		_, e := tx.CreateMetadata(p.Meta{ID: typeID, Fields: []p.Field{{ID: "note", MetaID: typeID, Type: p.TextType}, {ID: "photo", MetaID: typeID, Type: p.MediaType}}})
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		field string
		v     p.Value
		code  string
	}{
		{field: "note", v: func() p.Value { v, _ := p.NewValue(p.TextType, "long"); return v }(), code: "invalid_field_value"},
		{field: "photo", v: func() p.Value { v, _ := p.NewValue(p.MediaType, "c-example//opaque"); return v }(), code: "not_supported"},
	} {
		err = s.Transact(t.Context(), owner, held(t, s), func(tx store.Tx) error { _, e := tx.Insert(typeID, map[string]p.Value{tc.field: tc.v}); return e })
		if p.ErrorCode(err) != tc.code {
			t.Fatalf("want %s got %v", tc.code, err)
		}
	}
	err = s.Transact(t.Context(), owner, held(t, s), func(tx store.Tx) error { _, e := tx.Insert(typeID, nil); return e })
	if p.ErrorCode(err) != "query_result_limit_exceeded" {
		t.Fatal(err)
	}
}

func TestSchemaResponseCannotMutateStoredAnnotations(t *testing.T) {
	s := fixture(t)
	err := s.Transact(t.Context(), owner, held(t, s), func(tx store.Tx) error {
		_, e := tx.CreateMetadata(p.Meta{ID: "test.opaque", Annotations: map[string]json.RawMessage{"a": json.RawMessage(`{ "x": 1 }`)}, Fields: []p.Field{{ID: "f", MetaID: "test.opaque", Type: p.TextType, Roles: []string{"role"}, Annotations: map[string]json.RawMessage{"a": json.RawMessage(`1e3`)}}}})
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	info, _ := s.Info(t.Context(), owner, held(t, s))
	err = s.Transact(t.Context(), owner, held(t, s), func(tx store.Tx) error {
		m, e := tx.Metadata("test.opaque")
		if e != nil {
			return e
		}
		m.Annotations["a"][0] = '['
		m.Fields[0].Roles[0] = "changed"
		m.Fields[0].Annotations["a"][0] = '9'
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	after, _ := s.Info(t.Context(), owner, held(t, s))
	if after.SchemaVersion != info.SchemaVersion || after.Watermark != info.Watermark {
		t.Fatal("response aliases stored schema")
	}
}

func TestConcurrentDefineTypeOneWinner(t *testing.T) {
	s := fixture(t)
	dataset := held(t, s)
	results := make(chan error, 16)
	for range 16 {
		go func() {
			results <- s.Transact(context.Background(), owner, dataset, func(tx store.Tx) error { _, err := tx.CreateMetadata(p.Meta{ID: "test.race"}); return err })
		}()
	}
	winners := 0
	for range 16 {
		err := <-results
		if err == nil {
			winners++
		} else if p.ErrorCode(err) != "meta_type_already_exists" {
			t.Fatal(err)
		}
	}
	if winners != 1 {
		t.Fatalf("got %d winners", winners)
	}
}

// The evaluator holds one dataset and keeps no retirement ledger, so it can
// never prove a retirement: every other identity is concealed rather than
// declared absent, and the callback never runs for one.
func TestEvaluatorRefusesADatasetItDoesNotHold(t *testing.T) {
	s := fixture(t)
	if _, err := s.Info(t.Context(), owner, "d-elsewhere"); p.ErrorCode(err) != "unavailable" {
		t.Fatal("described a dataset it does not hold", err)
	}
	err := s.Transact(t.Context(), owner, "d-elsewhere", func(store.Tx) error {
		t.Fatal("the callback ran for a dataset it does not hold")
		return nil
	})
	if p.ErrorCode(err) != "unavailable" {
		t.Fatal("entered a dataset it does not hold", err)
	}
	// One answer covers both: an identity naming nothing here, and the dataset
	// this evaluator does hold seen by a caller with no grant on it. Neither
	// learns which of the two it met, which is the point.
	stranger := store.Access{Principal: "stranger", Authorization: "stranger"}
	if _, err = s.Info(t.Context(), stranger, "d-elsewhere"); p.ErrorCode(err) != "unavailable" {
		t.Fatal("an identity naming no dataset claimed a retirement", err)
	}
	if _, err = s.Info(t.Context(), stranger, held(t, s)); p.ErrorCode(err) != "unavailable" {
		t.Fatal("an ungranted caller could tell a live dataset from an absent one", err)
	}
	listed, err := s.Datasets(t.Context(), stranger)
	if err != nil || len(listed) != 0 {
		t.Fatal("listed a dataset for a caller with no grant", listed, err)
	}
	// A grant has to be over something this evaluator holds.
	grant := store.Authorization{Grants: store.Grants{Read: true}, View: store.FullView()}
	if err = s.SetAuthorization(t.Context(), "d-elsewhere", stranger, grant); p.ErrorCode(err) != "not_found" {
		t.Fatal("installed a grant over a dataset it does not hold", err)
	}
	if err = s.Revoke(t.Context(), "d-elsewhere", owner); p.ErrorCode(err) != "not_found" {
		t.Fatal("revoked a grant over a dataset it does not hold", err)
	}
	if _, err = s.Info(t.Context(), stranger, held(t, s)); p.ErrorCode(err) != "unavailable" {
		t.Fatal("a refused grant was installed anyway", err)
	}
}
