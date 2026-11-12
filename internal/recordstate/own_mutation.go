package recordstate

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

func (t *transaction) creatorID() string {
	return CreatorID(t.engine.creatorSecret[:], t.engine.id, t.principal)
}

// CreatorID derives the same dataset-local stamp for trusted storage operations.
func CreatorID(secret []byte, dataset, principal string) string {
	h := hmac.New(sha256.New, secret)
	h.Write([]byte("record_creator\x00"))
	h.Write([]byte(dataset))
	h.Write([]byte{0})
	h.Write([]byte(principal))
	return hex.EncodeToString(h.Sum(nil)[:p.CreatorIDBytes/2])
}

func CreatorField(types map[string]p.Meta, typeID string) string {
	for range len(types) {
		meta, ok := types[typeID]
		if !ok {
			return ""
		}
		for _, field := range meta.Fields {
			if field.Creator {
				return field.ID
			}
		}
		if meta.ParentID == nil {
			return ""
		}
		typeID = *meta.ParentID
	}
	return ""
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
