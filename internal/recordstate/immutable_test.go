package recordstate

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"testing"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

const (
	immutableRoot  = "test.dose"
	immutableChild = "test.dose.booster"
)

func immutableEngine(t *testing.T) (*Store, store.Access, string) {
	t.Helper()
	owner := store.Access{Principal: "owner", Authorization: "owner"}
	root := immutableRoot
	image := Image{ID: "s-0123456789abcdef0123456789abcdef", Next: 1, Watermark: "w:start",
		Types: map[string]p.Meta{
			immutableRoot: {ID: immutableRoot, Fields: []p.Field{
				{ID: "given", MetaID: immutableRoot, Type: p.IntegerType, Immutable: true},
				{ID: "reading", MetaID: immutableRoot, Type: p.FloatType, Immutable: true},
				{ID: "note", MetaID: immutableRoot, Type: p.TextType},
			}},
			immutableChild: {ID: immutableChild, ParentID: &root, Fields: []p.Field{
				{ID: "lot", MetaID: immutableChild, Type: p.TextType},
			}},
		},
		Objects: map[string]Row{}}
	engine, err := Restore(image, p.DevelopmentLimits(), owner, store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true}, View: store.FullView()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return engine, owner, image.ID
}

func value(t *testing.T, kind p.Type, payload any) p.Value {
	t.Helper()
	v, err := p.NewValue(kind, payload)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func insertDose(t *testing.T, engine *Store, owner store.Access, dataset, typeID string, fields map[string]p.Value) string {
	t.Helper()
	var id string
	err := engine.Transact(context.Background(), owner, dataset, func(tx store.Tx) error {
		result, e := tx.Insert(typeID, fields)
		if e == nil {
			id = result.Records[0].ID.ID
		}
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func updateDose(engine *Store, owner store.Access, dataset, typeID, id string, fields map[string]p.Value) error {
	return engine.Transact(context.Background(), owner, dataset, func(tx store.Tx) error {
		return tx.Update(typeID, id, fields)
	})
}

// refusedAsImmutable asserts the refusal a client can branch on: a conflict,
// with the reason, naming exactly the fields that would have changed.
func refusedAsImmutable(t *testing.T, err error, fields ...string) {
	t.Helper()
	var failure *p.Error
	if !errors.As(err, &failure) || failure.Code != "conflict" || failure.Details["reason"] != "immutable_field" {
		t.Fatalf("expected conflict/immutable_field, got %v", err)
	}
	if got := failure.Details["fields"]; !reflect.DeepEqual(got, fields) {
		t.Fatalf("refusal named %v, expected %v", got, fields)
	}
}

func storedFields(t *testing.T, engine *Store, owner store.Access, dataset, typeID, id string) map[string]p.Value {
	t.Helper()
	fields := map[string]p.Value{}
	err := engine.Read(context.Background(), owner, dataset, func(tx store.Tx) error {
		set, e := tx.Object(typeID, id, nil)
		if e != nil {
			return e
		}
		for i, f := range set.Meta.Fields {
			fields[f.ID] = set.Records[0].Values[i]
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return fields
}

// A written value stays written. Restating it is not a change, clearing it is,
// and a refused update leaves every field of it unapplied — including the ones
// that were free to change.
func TestAnImmutableFieldKeepsItsValue(t *testing.T) {
	engine, owner, dataset := immutableEngine(t)
	id := insertDose(t, engine, owner, dataset, immutableRoot, map[string]p.Value{"given": value(t, p.IntegerType, int64(5))})

	if err := updateDose(engine, owner, dataset, immutableRoot, id, map[string]p.Value{"given": value(t, p.IntegerType, int64(5)), "note": value(t, p.TextType, "restated")}); err != nil {
		t.Fatal("restating the stored value was refused:", err)
	}
	// Text converts to the declared integer, and the converted value is what
	// is compared: the same number spelled differently is still the same value.
	if err := updateDose(engine, owner, dataset, immutableRoot, id, map[string]p.Value{"given": value(t, p.TextType, "005")}); err != nil {
		t.Fatal("restating the stored value through a conversion was refused:", err)
	}
	refusedAsImmutable(t, updateDose(engine, owner, dataset, immutableRoot, id, map[string]p.Value{"given": value(t, p.IntegerType, int64(6)), "note": value(t, p.TextType, "changed")}), "given")
	refusedAsImmutable(t, updateDose(engine, owner, dataset, immutableRoot, id, map[string]p.Value{"given": {}}), "given")

	stored := storedFields(t, engine, owner, dataset, immutableRoot, id)
	if stored["given"].Payload() != int64(5) || stored["note"].Payload() != "restated" {
		t.Fatal("a refused update applied part of itself")
	}
	if err := updateDose(engine, owner, dataset, immutableRoot, id, map[string]p.Value{"note": value(t, p.TextType, "free")}); err != nil {
		t.Fatal("a field without the flag could not change:", err)
	}
}

// Null is never written. The first value may arrive by update, and from then
// on it is fixed like any other.
func TestAnImmutableFieldMayBeWrittenOnceByUpdate(t *testing.T) {
	engine, owner, dataset := immutableEngine(t)
	id := insertDose(t, engine, owner, dataset, immutableRoot, map[string]p.Value{"reading": {}})
	if err := updateDose(engine, owner, dataset, immutableRoot, id, map[string]p.Value{"reading": {}}); err != nil {
		t.Fatal("writing null over nothing was refused:", err)
	}
	if err := updateDose(engine, owner, dataset, immutableRoot, id, map[string]p.Value{"reading": value(t, p.FloatType, 0.0)}); err != nil {
		t.Fatal("the first value was refused:", err)
	}
	// -0 equals 0 in every comparison the protocol makes, and is a different
	// stored value: accepting it as unchanged would change what the record
	// returns.
	refusedAsImmutable(t, updateDose(engine, owner, dataset, immutableRoot, id, map[string]p.Value{"reading": value(t, p.FloatType, math.Copysign(0, -1))}), "reading")
	if bits := math.Float64bits(storedFields(t, engine, owner, dataset, immutableRoot, id)["reading"].Payload().(float64)); bits != 0 {
		t.Fatal("negative zero replaced a stored zero")
	}
}

// The flag is on the field, and a subtype carries its ancestors' fields
// unchanged, so a record of a subtype is held to it. update_where reaches
// descendants and is held to it too.
func TestASubtypeInheritsImmutability(t *testing.T) {
	engine, owner, dataset := immutableEngine(t)
	id := insertDose(t, engine, owner, dataset, immutableChild, map[string]p.Value{"given": value(t, p.IntegerType, int64(1))})
	refusedAsImmutable(t, updateDose(engine, owner, dataset, immutableChild, id, map[string]p.Value{"given": value(t, p.IntegerType, int64(2))}), "given")
	err := engine.Transact(context.Background(), owner, dataset, func(tx store.Tx) error {
		operation := p.Operation{Op: "update_where", Type: immutableRoot, Fields: map[string]json.RawMessage{"given": json.RawMessage(`{"type":"integer","value":3}`)}}
		_, e := store.Execute(tx, p.Transaction{Operations: []p.Operation{operation}}, dataset, p.DevelopmentLimits())
		return e
	})
	var failure *p.Error
	if !errors.As(err, &failure) || failure.Code != "conflict" || failure.Details["reason"] != "immutable_field" {
		t.Fatal("update_where changed an immutable value in a descendant:", err)
	}
}

// Immutability constrains update, not deletion: a record holding an immutable
// value can still be deleted.
func TestAnImmutableValueDoesNotPreventDeletion(t *testing.T) {
	engine, owner, dataset := immutableEngine(t)
	id := insertDose(t, engine, owner, dataset, immutableRoot, map[string]p.Value{"given": value(t, p.IntegerType, int64(1))})
	if err := engine.Transact(context.Background(), owner, dataset, func(tx store.Tx) error { return tx.Delete(immutableRoot, id) }); err != nil {
		t.Fatal("deleting a record with an immutable value was refused:", err)
	}
}

// The flag binds from the schema change that adds it, over values already
// written, and a schema holder may take it away again.
func TestImmutabilityFollowsTheSchema(t *testing.T) {
	engine, owner, dataset := immutableEngine(t)
	id := insertDose(t, engine, owner, dataset, immutableRoot, map[string]p.Value{"note": value(t, p.TextType, "before")})
	setNote := func(immutable bool) {
		t.Helper()
		err := engine.Transact(context.Background(), owner, dataset, func(tx store.Tx) error {
			m, e := tx.Metadata(immutableRoot)
			if e != nil {
				return e
			}
			declared := p.Meta{ID: m.ID, ParentID: m.ParentID, Name: m.Name}
			for _, f := range m.Fields {
				if f.MetaID != immutableRoot {
					continue
				}
				if f.ID == "note" {
					f.Immutable = immutable
				}
				declared.Fields = append(declared.Fields, f)
			}
			_, e = tx.UpdateMetadata(declared, "")
			return e
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	setNote(true)
	refusedAsImmutable(t, updateDose(engine, owner, dataset, immutableRoot, id, map[string]p.Value{"note": value(t, p.TextType, "after")}), "note")
	setNote(false)
	if err := updateDose(engine, owner, dataset, immutableRoot, id, map[string]p.Value{"note": value(t, p.TextType, "after")}); err != nil {
		t.Fatal("removing the flag did not release the field:", err)
	}
}
