package recordstate

import (
	"testing"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

func TestCollectionQueriesExcludeDirectReadGrants(t *testing.T) {
	ctx := t.Context()
	engine, err := New(p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	owner := store.Access{Principal: "owner", Authorization: "owner"}
	guest := store.Access{Principal: "reader", Authorization: "reader"}
	if err = engine.SetAuthorization(ctx, engine.id, owner, store.Authorization{
		Grants: store.Grants{Read: true, Write: true, Schema: true}, View: store.FullView(),
	}); err != nil {
		t.Fatal(err)
	}
	const typ = "test.collections"
	var ids []string
	if err = engine.Transact(ctx, owner, engine.id, func(tx store.Tx) error {
		_, err := tx.CreateMetadata(p.Meta{ID: typ, Fields: []p.Field{
			{ID: "membership", MetaID: typ, Type: p.TextType, Collection: true},
		}})
		if err != nil {
			return err
		}
		for _, collection := range []string{"alpha", "beta"} {
			value, _ := p.NewValue(p.TextType, collection)
			set, err := tx.Insert(typ, map[string]p.Value{"membership": value})
			if err != nil {
				return err
			}
			ids = append(ids, set.Records[0].ID.ID)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	view, err := store.ObjectView(store.ObjectKey{TypeID: typ, ObjectID: ids[1]}).
		WithCollections([]store.CollectionScope{{TypeID: typ, ID: "alpha"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = engine.SetAuthorization(ctx, engine.id, guest, store.Authorization{Grants: store.Grants{Read: true}, View: view}); err != nil {
		t.Fatal(err)
	}
	if err = engine.Read(ctx, guest, engine.id, func(tx store.Tx) error {
		if _, err := tx.Object(typ, ids[1], nil); err != nil {
			return err
		}
		set, err := tx.Query(typ, p.Query{}, 10)
		if err == nil && (len(set.Records) != 1 || set.Records[0].ID.ID != ids[0]) {
			t.Fatal("a direct-read object was added to the collection query")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
