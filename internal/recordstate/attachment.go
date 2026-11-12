package recordstate

import (
	"context"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// AuthorizeAttachmentTarget applies the same write scope as the record
// mutation that would attach the item.
func (s *Store) AuthorizeAttachmentTarget(ctx context.Context, access store.Access, dataset string, target p.AttachmentTarget) error {
	return s.Transact(ctx, access, dataset, func(tx store.Tx) error {
		t := tx.(*transaction)
		if target.ObjectID == "" {
			if err := t.check(t.auth.Grants.Write || t.auth.Grants.Insert && t.auth.View.AllowsCollectionInsert(target.TypeID, target.CollectionID)); err != nil {
				return err
			}
			if CollectionField(t.state.types, target.TypeID) == "" {
				return store.Refuse("permission_denied")
			}
		} else {
			row, err := t.ownMutationTarget(target.TypeID, target.ObjectID, false)
			if err != nil {
				return err
			}
			if CollectionID(t.state.types, target.TypeID, row.fields) != target.CollectionID {
				return store.Refuse("permission_denied")
			}
		}
		meta, err := p.EffectiveMetadata(t.state.types, target.TypeID)
		if err != nil {
			return err
		}
		for _, field := range meta.Fields {
			if field.Type == p.MediaType {
				return nil
			}
		}
		return store.Refuse("permission_denied")
	})
}
