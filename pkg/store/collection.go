package store

import (
	"slices"
	"sort"

	p "github.com/ekarpov/tessera/pkg/protocol"
)

// CollectionScope pairs an exact concrete type with a dataset-local collection.
type CollectionScope struct {
	TypeID string `json:"type_id"`
	ID     string `json:"collection_id"`
}

// WithCollections supplies independent read and insert scopes to a trusted host
// view. Dataset-level rights still govern which scopes are effective.
func (v View) WithCollections(read, insert []CollectionScope) (View, error) {
	if err := v.validateCollections(read, insert); err != nil {
		return View{}, err
	}
	v.readCollections, v.insertCollections = canonicalCollections(read), canonicalCollections(insert)
	return v, nil
}

// WithOwnMutations adds independent creator-scoped update and delete permissions.
func (v View) WithOwnMutations(update, remove []CollectionScope) (View, error) {
	if err := v.validateCollections(update, remove); err != nil {
		return View{}, err
	}
	v.updateOwn, v.deleteOwn = canonicalCollections(update), canonicalCollections(remove)
	return v, nil
}

func (v View) validateCollections(first, second []CollectionScope) error {
	if !v.Valid() || len(first)+len(second) > 256 {
		return Refuse("invalid_field")
	}
	for _, list := range [][]CollectionScope{first, second} {
		for _, scope := range list {
			if p.ValidateIdentifier(p.MetaIdentifier, scope.TypeID) != nil ||
				p.ValidateIdentifier(p.ObjectIdentifier, scope.ID) != nil {
				return Refuse("invalid_identifier")
			}
		}
	}
	return nil
}

func canonicalCollections(scopes []CollectionScope) []CollectionScope {
	if len(scopes) == 0 {
		return nil
	}
	out := slices.Clone(scopes)
	sort.Slice(out, func(i, j int) bool {
		if out[i].TypeID != out[j].TypeID {
			return out[i].TypeID < out[j].TypeID
		}
		return out[i].ID < out[j].ID
	})
	return slices.Compact(out)
}

// AllowsOwnMutation checks an exact type and collection. The evaluator
// separately checks visibility and creator attribution.
func (v View) AllowsOwnMutation(typeID, id string, remove bool) bool {
	scopes := v.updateOwn
	if remove {
		scopes = v.deleteOwn
	}
	return id != "" && slices.Contains(scopes, CollectionScope{typeID, id})
}

func (v View) HasCollectionReads() bool { return len(v.readCollections) != 0 }

func (v View) AllowsQueryType(id string) bool {
	return v.AllowsType(id) || collectionType(v.readCollections, id)
}

func (v View) AllowsInsertType(id string) bool {
	return v.AllowsInsert(id) || collectionType(v.insertCollections, id)
}

func (v View) AllowsCollectionInsert(typeID, id string) bool {
	return id != "" && slices.Contains(v.insertCollections, CollectionScope{typeID, id})
}

func (v View) AllowsCollectionRecord(k ObjectKey, sequence uint64, id string) bool {
	return id != "" && slices.Contains(v.readCollections, CollectionScope{k.TypeID, id}) &&
		(v.sequenceCut == 0 || (sequence > 0 && sequence < v.sequenceCut))
}

func collectionType(scopes []CollectionScope, id string) bool {
	return slices.ContainsFunc(scopes, func(scope CollectionScope) bool { return scope.TypeID == id })
}

// ReadCollections returns the explicit scopes without exposing view state.
func (v View) ReadCollections() []CollectionScope { return slices.Clone(v.readCollections) }

// WithCollectionRights removes scopes for rights the installation did not receive.
func (v View) WithCollectionRights(grants Grants) View {
	if !grants.Read {
		v.readCollections = nil
	}
	if !grants.Insert {
		v.insertCollections = nil
	}
	return v
}

// AllowsScopedAttachmentUpload indicates that at least one target can be
// requested. The upload still authorizes its concrete scope and record.
func (v View) AllowsScopedAttachmentUpload(grants Grants) bool {
	return (grants.Insert && len(v.insertCollections) != 0) ||
		(grants.Read && grants.UpdateOwn && len(v.updateOwn) != 0)
}
