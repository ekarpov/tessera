package recordstate

import (
	"slices"
	"testing"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

func TestCreatorAttributionUsesThePrincipalAndDataset(t *testing.T) {
	ctx := t.Context()
	owner := store.Access{Principal: "synthetic-owner", Authorization: "first"}
	second := store.Access{Principal: owner.Principal, Authorization: "second"}
	contributor := store.Access{Principal: "synthetic-contributor", Authorization: "first"}
	const typeID = "test.attribution"

	open := func() *Store {
		engine, err := New(p.DevelopmentLimits())
		if err != nil {
			t.Fatal(err)
		}
		full := store.Authorization{View: store.FullView(), Grants: store.Grants{Read: true, Write: true, Schema: true}}
		insert := store.Authorization{View: store.SharedView(nil).WithInserts([]string{typeID}), Grants: store.Grants{Insert: true}}
		for access, grant := range map[store.Access]store.Authorization{owner: full, second: full, contributor: insert} {
			if err = engine.SetAuthorization(ctx, engine.id, access, grant); err != nil {
				t.Fatal(err)
			}
		}
		if err = engine.Transact(ctx, owner, engine.id, func(tx store.Tx) error {
			_, err := tx.CreateMetadata(p.Meta{ID: typeID, Fields: []p.Field{{ID: "created_by", MetaID: typeID, Type: p.TextType, Creator: true}}})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return engine
	}
	writeAndRead := func(engine *Store, writer store.Access) string {
		var id string
		if err := engine.Transact(ctx, writer, engine.id, func(tx store.Tx) error {
			result, err := tx.Insert(typeID, nil)
			if err == nil {
				id = result.Records[0].ID.ID
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		var creator string
		if err := engine.Read(ctx, owner, engine.id, func(tx store.Tx) error {
			result, err := tx.Object(typeID, id, nil)
			if err == nil {
				creator, _ = result.Records[0].Values[0].Payload().(string)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if len(creator) != p.CreatorIDBytes {
			t.Fatal("record lacks an opaque creator identifier")
		}
		return creator
	}

	engine := open()
	original := writeAndRead(engine, owner)
	if writeAndRead(engine, second) != original {
		t.Fatal("a new authorization changed the creator identifier")
	}
	if writeAndRead(engine, contributor) == original {
		t.Fatal("the contributor was attributed to the owner")
	}
	if writeAndRead(open(), owner) == original {
		t.Fatal("two datasets exposed the same creator identifier")
	}
}

func TestCreatorAttributionRequiresEnoughTextCapacity(t *testing.T) {
	limits := p.DevelopmentLimits()
	limits.MaxTextBytes = p.CreatorIDBytes - 1
	engine, err := New(limits)
	if err != nil {
		t.Fatal(err)
	}
	owner := store.Access{Principal: "synthetic-owner", Authorization: "first"}
	ctx := t.Context()
	auth := store.Authorization{View: store.FullView(), Grants: store.Grants{Read: true, Write: true, Schema: true}}
	if err = engine.SetAuthorization(ctx, engine.id, owner, auth); err != nil {
		t.Fatal(err)
	}
	info, err := engine.Info(ctx, owner, engine.id)
	if err != nil || slices.Contains(info.Capabilities, p.CreatorAttributionCapability) {
		t.Fatal("dataset cannot advertise creator attribution at this text bound", err)
	}
	const typeID = "test.attribution"
	err = engine.Transact(ctx, owner, engine.id, func(tx store.Tx) error {
		_, err := tx.CreateMetadata(p.Meta{ID: typeID, Fields: []p.Field{{ID: "created_by", MetaID: typeID, Type: p.TextType, Creator: true}}})
		return err
	})
	if p.ErrorCode(err) != "not_supported" {
		t.Fatal("unsupported creator declaration was not refused", err)
	}
}
