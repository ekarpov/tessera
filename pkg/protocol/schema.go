package protocol

import (
	"encoding/json"
	"regexp"
	"unicode/utf8"
)

type Field struct {
	ID     string `json:"id"`
	MetaID string `json:"meta_id"`
	Name   string `json:"name"`
	Type   Type   `json:"type"`
	// Scale is the greatest number of fraction digits a value of this field
	// may carry. It is required on a `fixed` field and MUST be absent on every
	// other type, so that a reader never has to decide what scale means for a
	// type that has no fraction.
	//
	// It bounds rather than pads. A canonical fixed value carries no trailing
	// zero, so a scale-2 field stores `1`, `1.5` and `1.25`, and refuses
	// `1.255` — which would have to be rounded, and rounding is the caller's
	// decision rather than the store's.
	Scale *int `json:"scale,omitempty"`
	// Immutable fixes a value once it is written: an update may set the field
	// while it holds nothing, and may restate what it holds, and may do nothing
	// else to it. It constrains update, never deletion, and it is a rule the
	// server applies mechanically — the store still does not know what the
	// field means. `false` and absent are the same, and only `true` is returned.
	Immutable bool `json:"immutable,omitempty"`
	// Creator marks a text field populated by the server from the authenticated
	// principal on insert. A caller cannot supply or change its value.
	Creator     bool                       `json:"creator,omitempty"`
	Roles       []string                   `json:"roles,omitempty"`
	Annotations map[string]json.RawMessage `json:"annotations,omitempty"`
}

type Meta struct {
	SchemaVersion string                     `json:"schema_version,omitempty"`
	ID            string                     `json:"id"`
	ParentID      *string                    `json:"parent_id"`
	Name          string                     `json:"name"`
	Fields        []Field                    `json:"fields"`
	Roles         []string                   `json:"roles,omitempty"`
	Annotations   map[string]json.RawMessage `json:"annotations,omitempty"`
}

func (m Meta) MarshalJSON() ([]byte, error) {
	type wire Meta
	if m.Fields == nil {
		m.Fields = []Field{}
	}
	return MarshalWire(wire(m))
}

// ValidateDeclared validates one type's own fields. Inherited collisions and
// graph constraints are checked by EffectiveMetadata before publishing a schema.
func (m Meta) ValidateDeclared() error {
	if err := ValidateIdentifier(MetaIdentifier, m.ID); err != nil {
		return err
	}
	if !utf8.ValidString(m.Name) {
		return invalid("invalid_name")
	}
	if m.ParentID != nil {
		if err := ValidateIdentifier(MetaIdentifier, *m.ParentID); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for _, f := range m.Fields {
		if err := ValidateIdentifier(FieldIdentifier, f.ID); err != nil {
			return err
		}
		if !utf8.ValidString(f.Name) {
			return invalid("invalid_name")
		}
		if !f.Type.ValidFieldType() {
			return invalid("invalid_type")
		}
		if err := f.validateScale(); err != nil {
			return err
		}
		if f.Creator && f.Type != TextType {
			return invalid("meta_invalid_field")
		}
		if f.MetaID != m.ID || seen[f.ID] {
			return invalid("meta_invalid_field")
		}
		seen[f.ID] = true
	}
	return nil
}

// EffectiveMetadata resolves a declared schema without mutating caller-owned
// slices. The reference implementation's stable order is ancestor-first, then
// declaration order. The protocol permits other stable field orders.
func EffectiveMetadata(types map[string]Meta, id string) (Meta, error) {
	m, ok := types[id]
	if !ok {
		return Meta{}, invalid("meta_type_not_found")
	}
	chain := []Meta{}
	seen := map[string]bool{}
	for current := m; ; {
		if seen[current.ID] {
			return Meta{}, invalid("meta_schema_error")
		}
		seen[current.ID] = true
		if err := current.ValidateDeclared(); err != nil {
			return Meta{}, err
		}
		chain = append(chain, current)
		if current.ParentID == nil {
			break
		}
		parent, ok := types[*current.ParentID]
		if !ok || parent.ID != *current.ParentID {
			return Meta{}, invalid("meta_schema_error")
		}
		current = parent
	}
	if m.ID != id {
		return Meta{}, invalid("meta_schema_error")
	}
	fields := make([]Field, 0)
	ids := map[string]bool{}
	for i := len(chain) - 1; i >= 0; i-- {
		for _, field := range chain[i].Fields {
			if ids[field.ID] {
				return Meta{}, invalid("meta_invalid_field")
			}
			ids[field.ID] = true
			field.Roles = append([]string(nil), field.Roles...)
			if field.Annotations != nil {
				annotations := map[string]json.RawMessage{}
				for k, v := range field.Annotations {
					annotations[k] = append(json.RawMessage(nil), v...)
				}
				field.Annotations = annotations
			}
			fields = append(fields, field)
		}
	}
	m.Fields = fields
	roles := []string{}
	roleSet := map[string]bool{}
	annotations := map[string]json.RawMessage{}
	for i := len(chain) - 1; i >= 0; i-- {
		for _, role := range chain[i].Roles {
			if !roleSet[role] {
				roleSet[role] = true
				roles = append(roles, role)
			}
		}
		for key, value := range chain[i].Annotations {
			if _, exists := annotations[key]; exists {
				return Meta{}, invalid("meta_invalid_field")
			}
			annotations[key] = append(json.RawMessage(nil), value...)
		}
	}
	m.Roles = roles
	m.Annotations = annotations
	if m.ParentID != nil {
		parent := *m.ParentID
		m.ParentID = &parent
	}
	return m, nil
}

type ObjectID struct {
	ID        string `json:"id"`
	DatasetID string `json:"dataset_id"`
}

type Record struct {
	ID      ObjectID `json:"id"`
	MetaID  string   `json:"meta_id"`
	Values  []Value  `json:"values"`
	Version string   `json:"version,omitempty"`
}

func (r Record) MarshalJSON() ([]byte, error) {
	type wire Record
	if r.Values == nil {
		r.Values = []Value{}
	}
	return MarshalWire(wire(r))
}

type RecordSet struct {
	Meta      Meta     `json:"meta"`
	Records   []Record `json:"records"`
	Watermark string   `json:"watermark"`
}

func (r RecordSet) MarshalJSON() ([]byte, error) {
	type wire RecordSet
	if r.Records == nil {
		r.Records = []Record{}
	}
	return MarshalWire(wire(r))
}

// Validate checks the record-set envelope against its projection. Validating
// concrete-type ancestry additionally requires the server's schema catalogue.
func (r RecordSet) Validate(dataset string, limits Limits) error {
	if err := ValidateIdentifier(DatasetIdentifier, dataset); err != nil {
		return err
	}
	if err := ValidateIdentifier(MetaIdentifier, r.Meta.ID); err != nil {
		return err
	}
	if len(r.Meta.Fields) > limits.MaxProjectedFields {
		return invalid("query_result_limit_exceeded")
	}
	seen := map[string]bool{}
	for _, f := range r.Meta.Fields {
		if err := ValidateIdentifier(FieldIdentifier, f.ID); err != nil {
			return err
		}
		if err := ValidateIdentifier(MetaIdentifier, f.MetaID); err != nil {
			return err
		}
		if !f.Type.ValidFieldType() || seen[f.ID] {
			return invalid("meta_invalid_field")
		}
		if err := f.validateScale(); err != nil {
			return err
		}
		seen[f.ID] = true
	}
	for _, record := range r.Records {
		if record.ID.DatasetID != dataset || len(record.Values) != len(r.Meta.Fields) {
			return invalid("invalid_field_value")
		}
		if err := ValidateIdentifier(ObjectIdentifier, record.ID.ID); err != nil {
			return err
		}
		if err := ValidateIdentifier(MetaIdentifier, record.MetaID); err != nil {
			return err
		}
		for i, v := range record.Values {
			if v.Type() != NullType && v.Type() != r.Meta.Fields[i].Type {
				return invalid("invalid_field_value")
			}
			if err := v.Validate(limits); err != nil {
				return err
			}
		}
	}
	return nil
}

var rolePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ValidateSemantics enforces structure only, never any application vocabulary.
func (m Meta) ValidateSemantics(limits Limits) error {
	if err := validateSemantics(m.Roles, m.Annotations, limits); err != nil {
		return err
	}
	for _, f := range m.Fields {
		if f.Creator && limits.MaxTextBytes < CreatorIDBytes {
			return invalid("not_supported")
		}
		if err := validateSemantics(f.Roles, f.Annotations, limits); err != nil {
			return err
		}
	}
	return nil
}
func validateSemantics(roles []string, annotations map[string]json.RawMessage, limits Limits) error {
	if len(roles) > limits.MaxRoles {
		return invalid("meta_invalid_field")
	}
	seen := map[string]bool{}
	for _, r := range roles {
		if len(r) > limits.MaxRoleLength || !rolePattern.MatchString(r) || seen[r] {
			return invalid("meta_invalid_field")
		}
		seen[r] = true
	}
	for k, v := range annotations {
		if !utf8.ValidString(k) {
			return invalid("meta_invalid_field")
		}
		if err := checkJSON(v, true); err != nil {
			return err
		}
	}
	return nil
}

// MaxFixedScale bounds a declared scale. A field cannot usefully declare more
// fraction digits than a value may carry in total, and the two bounds are kept
// apart because they answer different questions: this one is what the schema
// may say, MaxFixedDigits is what a value may be.
const MaxFixedScale = 32

func (f Field) validateScale() error {
	if f.Type != FixedType {
		if f.Scale != nil {
			return invalid("meta_invalid_field")
		}
		return nil
	}
	if f.Scale == nil || *f.Scale < 0 || *f.Scale > MaxFixedScale {
		return invalid("meta_invalid_field")
	}
	return nil
}

// FixedScaleOf reports the declared scale of a field, and whether it has one.
func (f Field) FixedScaleOf() (int, bool) {
	if f.Type != FixedType || f.Scale == nil {
		return 0, false
	}
	return *f.Scale, true
}
