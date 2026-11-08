package store

import (
	"encoding/json"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"sort"
)

// SharedView grants exact concrete types for queries and explicit identities for
// direct reads. It never conveys mutation rights or follows object references.
func SharedView(types []string, objects ...ObjectKey) View {
	v := ObjectView(objects...)
	v.types = make(map[string]struct{}, len(types))
	for _, id := range types {
		v.types[id] = struct{}{}
	}
	return v
}

// WithInserts is this view, able also to add records of exactly the named
// concrete types under an Insert grant. It shows nothing more.
func (v View) WithInserts(types []string) View {
	if len(types) == 0 {
		return v
	}
	v.inserts = make(map[string]struct{}, len(types))
	for _, id := range types {
		v.inserts[id] = struct{}{}
	}
	return v
}

// AllowsInsert reports whether an Insert grant reaches this concrete type. A
// full view does not imply it: insertion is named type by type, and an owner
// adds records through Write.
func (v View) AllowsInsert(id string) bool {
	_, ok := v.inserts[id]
	return v.initialized && ok
}

func (v View) AllowsType(id string) bool {
	if v.initialized && v.full {
		return true
	}
	_, ok := v.types[id]
	return v.initialized && ok
}

// AllowsMetadata includes the types a caller may insert into: writing a record
// of a type needs its schema, and a schema discloses no record.
func (v View) AllowsMetadata(id string) bool {
	if v.AllowsType(id) || v.AllowsInsert(id) {
		return true
	}
	for k := range v.objects {
		if k.TypeID == id {
			return true
		}
	}
	return false
}
func (v View) MarshalJSON() ([]byte, error) {
	if !v.Valid() {
		return nil, Refuse("permission_denied")
	}
	types := []string{}
	for id := range v.types {
		types = append(types, id)
	}
	sort.Strings(types)
	inserts := []string{}
	for id := range v.inserts {
		inserts = append(inserts, id)
	}
	sort.Strings(inserts)
	objects := []ObjectKey{}
	for key := range v.objects {
		objects = append(objects, key)
	}
	sort.Slice(objects, func(i, j int) bool {
		if objects[i].TypeID != objects[j].TypeID {
			return objects[i].TypeID < objects[j].TypeID
		}
		return objects[i].ObjectID < objects[j].ObjectID
	})
	return json.Marshal(struct {
		Full        bool        `json:"full"`
		SequenceCut uint64      `json:"sequence_cut,omitempty"`
		Types       []string    `json:"types,omitempty"`
		Objects     []ObjectKey `json:"objects,omitempty"`
		Inserts     []string    `json:"insert_types,omitempty"`
	}{v.full, v.sequenceCut, types, objects, inserts})
}
func (v *View) UnmarshalJSON(raw []byte) error {
	var wire struct {
		Full        bool        `json:"full"`
		SequenceCut uint64      `json:"sequence_cut,omitempty"`
		Types       []string    `json:"types"`
		Objects     []ObjectKey `json:"objects"`
		Inserts     []string    `json:"insert_types"`
	}
	if json.Unmarshal(raw, &wire) != nil {
		return Refuse("invalid_field")
	}
	for _, id := range wire.Inserts {
		if p.ValidateIdentifier(p.MetaIdentifier, id) != nil {
			return Refuse("invalid_identifier")
		}
	}
	if wire.Full {
		if len(wire.Types) > 0 || len(wire.Objects) > 0 {
			return Refuse("invalid_field")
		}
		*v = FullView().WithInserts(wire.Inserts).BeforeSequence(wire.SequenceCut)
		return nil
	}
	for _, id := range wire.Types {
		if p.ValidateIdentifier(p.MetaIdentifier, id) != nil {
			return Refuse("invalid_identifier")
		}
	}
	for _, key := range wire.Objects {
		if p.ValidateIdentifier(p.MetaIdentifier, key.TypeID) != nil || p.ValidateIdentifier(p.ObjectIdentifier, key.ObjectID) != nil {
			return Refuse("invalid_identifier")
		}
	}
	*v = SharedView(wire.Types, wire.Objects...).WithInserts(wire.Inserts).BeforeSequence(wire.SequenceCut)
	return nil
}

// BeforeSequence narrows records to those minted before cut. Zero removes the
// cut; schema and insert scope do not change.
func (v View) BeforeSequence(cut uint64) View { v.sequenceCut = cut; return v }
func (v View) SequenceCut() uint64            { return v.sequenceCut }

// AllowsSequence checks a trusted engine's minted sequence, never an identity
// interpreted by the transport. A cut intersects the existing authorized view.
func (v View) AllowsSequence(k ObjectKey, sequence uint64) bool {
	return v.Allows(k) && (v.sequenceCut == 0 || (sequence > 0 && sequence < v.sequenceCut))
}
