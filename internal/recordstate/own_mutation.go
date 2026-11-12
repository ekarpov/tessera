package recordstate

import (
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

func (t *transaction) creatorID() string {
	digest := t.engine.digest(&t.engine.creatorSecret, "record_creator", []byte(t.principal))
	return digest[2 : 2+p.CreatorIDBytes]
}

func (t *transaction) canMutateOwn(remove bool) bool {
	grants := t.auth.Grants
	if grants.Write {
		return true
	}
	if remove {
		return grants.Read && grants.DeleteOwn
	}
	return grants.Read && grants.UpdateOwn
}

func (t *transaction) ownMutationTarget(typeID, id string, remove bool) (object, error) {
	if err := t.check(t.canMutateOwn(remove)); err != nil {
		return object{}, err
	}
	o, err := t.lookup(typeID, id)
	if err != nil || t.auth.Grants.Write {
		return o, err
	}
	if t.auth.View.AllowsOwnMutation(typeID, CollectionID(t.state.types, typeID, o.fields), remove) {
		meta, err := p.EffectiveMetadata(t.state.types, typeID)
		if err != nil {
			return object{}, err
		}
		creator := t.creatorID()
		for _, f := range meta.Fields {
			if !f.Creator {
				continue
			}
			value := o.fields[f.ID]
			if value.Type() == p.TextType && value.Payload() == creator {
				return o, nil
			}
		}
	}
	return object{}, store.Refuse("permission_denied")
}
