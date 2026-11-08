package recordstate

import (
	"context"
	"testing"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// A scan inside a sourced write that is told to stop, stops — including when
// the write inserted records the storage does not hold yet. Carrying on would
// return the same answer and read what the stop was there to avoid.
func TestAScanInsideASourcedWriteStopsWhenToldTo(t *testing.T) {
	const typeID = "test.record"
	owner := store.Access{Principal: "owner", Authorization: "owner"}
	image := Image{ID: "s-0123456789abcdef0123456789abcdef", Next: 100, Watermark: "w:start",
		Types:   map[string]p.Meta{typeID: {ID: typeID, Fields: []p.Field{{ID: "n", MetaID: typeID, Type: p.IntegerType}}}},
		Objects: map[string]Row{}}
	engine, err := Restore(image, p.DevelopmentLimits(), owner, store.Authorization{Grants: store.Grants{Read: true, Write: true}, View: store.FullView()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	stored := []string{"1", "2", "3"}
	engine.SetObjectSource(func(id string) (Row, bool, error) { return Row{}, false, nil })
	engine.SetScanSource(func(types []string, visit func(string, Row) (bool, error)) error {
		for _, id := range stored {
			if more, err := visit(id, Row{TypeID: typeID, Fields: map[string]p.Value{}}); err != nil || !more {
				return err
			}
		}
		return nil
	})
	engine.EnableSourcedWrites()
	value, _ := p.NewValue(p.IntegerType, int64(1))
	err = engine.Transact(context.Background(), owner, image.ID, func(tx store.Tx) error {
		for i := 0; i < 3; i++ {
			if _, e := tx.Insert(typeID, map[string]p.Value{"n": value}); e != nil {
				return e
			}
		}
		visited := 0
		e := tx.(*transaction).scanRows([]string{typeID}, func(string, Row) (bool, error) {
			visited++
			return false, nil
		})
		if e != nil {
			return e
		}
		if visited != 1 {
			t.Fatal("a scan told to stop after one row visited", visited)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
