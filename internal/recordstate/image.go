package recordstate

import (
	"context"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
	"strconv"
)

// Image is trusted engine state, never an HTTP model. In particular Secret must
// never be returned to a caller or included in diagnostics.
type Image struct {
	ID        string
	Secret    [32]byte
	Next      uint64
	Watermark string
	Types     map[string]p.Meta
	Objects   map[string]Row
}
type Row struct {
	// Sequence comes from the minted engine identity, not a request parameter.
	Sequence uint64
	TypeID   string
	Fields   map[string]p.Value
	// Bytes is the row's stored size, when the source knows it. A streamed
	// scan charges the rows it keeps against max_materialized_bytes, which is
	// what turns that bound from one on the whole dataset into one on what a
	// request actually holds.
	Bytes int
}

// Restore rebuilds an evaluator for one transaction. reads carries the
// composition's advertised optional read capabilities, which are configuration
// rather than dataset state and so never travel in the Image.
// defaultDisplayName is what the dataset listing reported for every entry
// before a dataset could carry a name. A durable engine reads the stored name
// over the top of it; the evaluator has nowhere else to get one.
const defaultDisplayName = "Tessera dataset"

func Restore(image Image, limits p.Limits, a store.Access, grant store.Authorization, reads []string) (*Store, error) {
	return restore(image, limits, a, grant, reads, false)
}

// Adopt is Restore for a caller that built this image and will not look at it
// again. It takes the types and fields as they are instead of copying them.
//
// Restore copies because an image is ordinarily somebody else's, and a store
// that shared it would change what its owner still holds. An image decoded
// out of storage a moment ago has no owner but the store about to be built
// from it, and copying it was the single largest cost of answering a read.
// The caller promises not to keep it; nothing here can check that, which is
// why it is a second function and not a flag on the first.
func Adopt(image Image, limits p.Limits, a store.Access, grant store.Authorization, reads []string) (*Store, error) {
	return restore(image, limits, a, grant, reads, true)
}

func restore(image Image, limits p.Limits, a store.Access, grant store.Authorization, reads []string, owned bool) (*Store, error) {
	if p.ValidateIdentifier(p.DatasetIdentifier, image.ID) != nil || image.Next == 0 || image.Watermark == "" {
		return nil, store.Refuse("internal_error")
	}
	s := &Store{lock: make(chan struct{}, 1), id: image.ID, displayName: defaultDisplayName, secret: image.Secret, limits: limits, authorizations: map[store.Access]store.Authorization{a: grant}, state: state{types: make(map[string]p.Meta, len(image.Types)), objects: make(map[string]object, len(image.Objects)), next: image.Next, watermark: image.Watermark}}
	if err := s.EnableReads(reads...); err != nil {
		return nil, err
	}
	for id, m := range image.Types {
		if owned {
			s.state.types[id] = m
			continue
		}
		s.state.types[id] = cloneMeta(m)
	}
	for id, row := range image.Objects {
		if row.Sequence == 0 {
			row.Sequence, _ = strconv.ParseUint(id, 10, 64)
		}
		if owned {
			s.state.objects[id] = object{sequence: row.Sequence, typeID: row.TypeID, fields: row.Fields}
			continue
		}
		s.state.objects[id] = object{sequence: row.Sequence, typeID: row.TypeID, fields: cloneFields(row.Fields)}
	}
	return s, nil
}
func (s *Store) Image(ctx context.Context) (Image, error) {
	if err := s.acquire(ctx); err != nil {
		return Image{}, err
	}
	defer s.release()
	image := Image{ID: s.id, Secret: s.secret, Next: s.state.next, Watermark: s.state.watermark, Types: map[string]p.Meta{}, Objects: map[string]Row{}}
	for id, m := range s.state.types {
		image.Types[id] = cloneMeta(m)
	}
	for id, row := range s.state.objects {
		image.Objects[id] = Row{Sequence: row.sequence, TypeID: row.typeID, Fields: cloneFields(row.fields)}
	}
	return image, nil
}

// Visible interprets the reference engine's minted decimal sequence. This is
// engine identity state, not a protocol identity grammar: other implementations
// can mint differently and supply their own sequence to View.AllowsSequence.
func Visible(view store.View, typeID, id string) bool {
	if view.SequenceCut() == 0 {
		return view.Allows(store.ObjectKey{TypeID: typeID, ObjectID: id})
	}
	sequence, err := strconv.ParseUint(id, 10, 64)
	return err == nil && view.AllowsSequence(store.ObjectKey{TypeID: typeID, ObjectID: id}, sequence)
}
