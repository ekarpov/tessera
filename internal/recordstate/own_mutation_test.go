package recordstate

import (
	"testing"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

func TestOwnMutationWithDirectRead(t *testing.T) {
	ctx := t.Context()
	engine, err := New(p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	first := store.Access{Principal: "writer", Authorization: "first"}
	second := store.Access{Principal: "writer", Authorization: "second"}
	const parent, child = "test.root", "test.child"
	if err = engine.SetAuthorization(ctx, engine.id, first, store.Authorization{
		View: store.FullView(), Grants: store.Grants{Read: true, Write: true, Schema: true},
	}); err != nil {
		t.Fatal(err)
	}
	var id string
	if err = engine.Transact(ctx, first, engine.id, func(tx store.Tx) error {
		if _, err := tx.CreateMetadata(p.Meta{ID: parent, Fields: []p.Field{
			{ID: "creator", MetaID: parent, Type: p.TextType, Creator: true},
			{ID: "membership", MetaID: parent, Type: p.TextType, Collection: true},
			{ID: "title", MetaID: parent, Type: p.TextType},
		}}); err != nil {
			return err
		}
		parentID := parent
		if _, err := tx.CreateMetadata(p.Meta{ID: child, ParentID: &parentID}); err != nil {
			return err
		}
		group, _ := p.NewValue(p.TextType, "group")
		result, err := tx.Insert(child, map[string]p.Value{"membership": group})
		if err == nil {
			id = result.Records[0].ID.ID
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}

	direct := store.ObjectView(store.ObjectKey{TypeID: child, ObjectID: id})
	for _, test := range []struct{ scope, code string }{
		{scope: parent, code: "permission_denied"},
		{scope: child},
	} {
		view, err := direct.WithOwnMutations([]store.CollectionScope{{TypeID: test.scope, ID: "group"}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err = engine.SetAuthorization(ctx, engine.id, second, store.Authorization{
			View: view, Grants: store.Grants{Read: true, UpdateOwn: true},
		}); err != nil {
			t.Fatal(err)
		}
		title, _ := p.NewValue(p.TextType, "revised")
		err = engine.Transact(ctx, second, engine.id, func(tx store.Tx) error {
			return tx.Update(child, id, map[string]p.Value{"title": title})
		})
		if (test.code == "" && err != nil) || (test.code != "" && p.ErrorCode(err) != test.code) {
			t.Fatalf("scope %s: got %v, want code %q", test.scope, err, test.code)
		}
	}
}
