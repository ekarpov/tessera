package protocol

import (
	"encoding/json"
	"strings"
)

type Target struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}
type Operation struct {
	SchemaVersion string                     `json:"schema_version,omitempty"`
	Op            string                     `json:"op"`
	Type          string                     `json:"type,omitempty"`
	Target        *Target                    `json:"target,omitempty"`
	Fields        map[string]json.RawMessage `json:"fields,omitempty"`
	As            string                     `json:"as,omitempty"`
	Meta          *Meta                      `json:"meta,omitempty"`
	Filter        json.RawMessage            `json:"filter,omitempty"`
	ObjectIDs     []string                   `json:"object_ids,omitempty"`
	Version       *string                    `json:"version,omitempty"`
	Guards        []string                   `json:"guards,omitempty"`
	Cascade       bool                       `json:"cascade,omitempty"`
	Confirm       string                     `json:"confirm,omitempty"`
}
type Transaction struct {
	Operations []Operation `json:"operations"`
}
type Identity struct {
	ID     string `json:"id"`
	MetaID string `json:"meta_id"`
}
type OperationResult struct {
	Objects []Identity `json:"objects,omitempty"`
	// RecordSet is the stored row, present only where the caller asked for it
	// with `Prefer: return=representation` and may read what it wrote. It is
	// what a read of the new object would return, produced by the same path,
	// so a client cannot be handed a representation that a subsequent read
	// would contradict.
	//
	// Objects stays whatever this carries. The identity is what every caller
	// of an insert needs and the representation is what some do, and the two
	// are built from one object inside one transaction, so they cannot
	// disagree — which is the objection that would otherwise apply to saying
	// the same thing twice.
	RecordSet      *RecordSet `json:"record_set,omitempty"`
	Affected       *int       `json:"affected,omitempty"`
	Type           *Meta      `json:"type,omitempty"`
	DeletedTypes   []string   `json:"deleted_types,omitempty"`
	DeletedObjects *int       `json:"deleted_objects,omitempty"`
}
type TransactionResult struct {
	Results   []OperationResult `json:"results"`
	Watermark string            `json:"watermark"`
}

func AtOperation(err error, index int) error {
	if err == nil {
		return nil
	}
	if e, ok := err.(*Error); ok {
		copy := *e
		copy.OperationIndex = &index
		return &copy
	}
	return err
}

// DecodeTransaction validates the entire grammar before execution or a key claim.
// Field conversion and checks against transaction-local schema happen in order.
func DecodeTransaction(data []byte, limits Limits) (Transaction, error) {
	var t Transaction
	var envelope struct {
		Operations []json.RawMessage `json:"operations"`
	}
	if err := DecodeRequest(data, &envelope, limits.MaxRequestBytes); err != nil {
		return t, err
	}
	// Decode each operation after checking its members so an unknown grammar
	// member reports invalid_request with its operation index.
	for i, raw := range envelope.Operations {
		var members map[string]json.RawMessage
		if json.Unmarshal(raw, &members) != nil || members == nil {
			return t, AtOperation(invalid("invalid_request"), i)
		}
		for key := range members {
			if !strings.Contains(" op schema_version type target fields as meta filter object_ids version guards cascade confirm ", " "+key+" ") {
				return t, AtOperation(invalid("invalid_request"), i)
			}
		}
		var op Operation
		if err := DecodeRequest(raw, &op, limits.MaxRequestBytes); err != nil {
			return t, AtOperation(err, i)
		}
		t.Operations = append(t.Operations, op)
	}
	if len(t.Operations) < 1 {
		return t, invalid("invalid_field")
	}
	bound := limits.MaxTransactionOperations
	if bound <= 0 {
		bound = DefaultTransactionOperations
	}
	if len(t.Operations) > bound {
		return t, &Error{Code: "query_result_limit_exceeded", Message: "Transaction has more operations than this dataset accepts",
			Details: map[string]any{"reason": "operation_limit", "maximum": bound}}
	}
	var body struct {
		Operations []map[string]json.RawMessage `json:"operations"`
	}
	_ = json.Unmarshal(data, &body)
	aliases := map[string]int{}
	for i, op := range t.Operations {
		allowed := "op"
		required := ""
		switch op.Op {
		case "insert":
			allowed += " type fields as"
			required = "type fields"
		case "update":
			allowed += " target fields"
			required = "target fields"
		case "delete":
			allowed += " target"
			required = "target"
		case "update_where":
			allowed += " type filter fields"
			required = "type fields"
		case "delete_where":
			allowed += " type filter object_ids"
			required = "type"
		case "define_type":
			allowed += " meta"
			required = "meta"
		case "update_type":
			allowed += " meta confirm"
			required = "meta"
		case "delete_type":
			allowed += " type cascade"
			required = "type"
		case "expect":
			allowed += " target fields version"
			required = "target"
		case "expect_type":
			allowed += " type schema_version"
			required = "type schema_version"
		case "expect_absent":
			allowed += " type filter guards"
			required = "type filter guards"
		default:
			return t, AtOperation(invalid("invalid_request"), i)
		}
		for k := range body.Operations[i] {
			if !strings.Contains(" "+allowed+" ", " "+k+" ") {
				return t, AtOperation(invalid("invalid_request"), i)
			}
		}
		for _, k := range strings.Fields(required) {
			v, ok := body.Operations[i][k]
			if !ok || string(v) == "null" {
				return t, AtOperation(invalid("invalid_request"), i)
			}
		}
		if op.Op == "expect" && ((op.Fields == nil) == (op.Version == nil)) {
			return t, AtOperation(invalid("invalid_field"), i)
		}
		if op.Op == "delete_where" && op.Filter != nil && op.ObjectIDs != nil {
			return t, AtOperation(invalid("invalid_filter"), i)
		}
		if op.Meta != nil && op.Meta.SchemaVersion != "" {
			return t, AtOperation(invalid("invalid_field"), i)
		}
		if _, present := body.Operations[i]["type"]; present {
			if err := ValidateIdentifier(MetaIdentifier, op.Type); err != nil {
				return t, AtOperation(err, i)
			}
		}
		if op.Target != nil {
			if err := ValidateIdentifier(MetaIdentifier, op.Target.Type); err != nil {
				return t, AtOperation(err, i)
			}
			if err := validateAliasID(op.Target.ID, aliases); err != nil {
				return t, AtOperation(err, i)
			}
		}
		refs := map[string]Reference{}
		for name := range aliases {
			refs[name] = Reference{DatasetID: "validation", MetaID: "validation", ObjectID: "validation"}
		}
		for _, raw := range op.Fields {
			resolved, err := resolveReferenceAlias(raw, refs)
			if err != nil {
				return t, AtOperation(err, i)
			}
			var value Value
			if err := DecodeRequest(resolved, &value, limits.MaxRequestBytes); err != nil {
				return t, AtOperation(err, i)
			}
		}
		if op.As != "" {
			// Alias names follow the protocol grammar, independent of object IDs.
			if strings.HasPrefix(op.As, "$") {
				return t, AtOperation(invalid("invalid_identifier"), i)
			}
			if err := ValidateIdentifier(AliasIdentifier, op.As); err != nil {
				return t, AtOperation(err, i)
			}
			if _, exists := aliases[op.As]; exists {
				return t, AtOperation(invalid("invalid_request"), i)
			}
			aliases[op.As] = i
		} else if _, exists := body.Operations[i]["as"]; exists {
			return t, AtOperation(invalid("invalid_identifier"), i)
		}
		if len(op.Filter) > 0 && string(op.Filter) != "null" {
			refs := map[string]Reference{}
			for name := range aliases {
				refs[name] = Reference{DatasetID: "validation", ObjectID: "validation", MetaID: "validation"}
			}
			resolved, e := ResolvePredicateAliases(op.Filter, refs)
			if e != nil {
				return t, AtOperation(e, i)
			}
			if _, err := ParsePredicate(resolved, limits); err != nil {
				return t, AtOperation(err, i)
			}
		}
		if len(op.ObjectIDs) > 256 {
			return t, AtOperation(invalid("invalid_filter"), i)
		}
		for _, id := range op.ObjectIDs {
			if err := validateAliasID(id, aliases); err != nil {
				return t, AtOperation(err, i)
			}
		}
	}
	for i, op := range t.Operations {
		if op.Op == "expect_absent" {
			if len(op.Guards) == 0 || string(op.Filter) == "null" {
				return t, AtOperation(invalid("invalid_filter"), i)
			}
			seen := map[string]bool{}
			for _, name := range op.Guards {
				idx, ok := aliases[name]
				if !ok || idx <= i || seen[name] {
					return t, AtOperation(invalid("invalid_request"), i)
				}
				seen[name] = true
			}
		}
	}
	return t, nil
}
func validateAliasID(id string, aliases map[string]int) error {
	if strings.HasPrefix(id, "$") {
		if _, ok := aliases[strings.TrimPrefix(id, "$")]; !ok {
			return invalid("invalid_request")
		}
		return nil
	}
	return ValidateIdentifier(ObjectIdentifier, id)
}

func (op Operation) Values(aliases map[string]Reference, limits Limits) (map[string]Value, error) {
	result := make(map[string]Value, len(op.Fields))
	for field, raw := range op.Fields {
		resolved, err := resolveReferenceAlias(raw, aliases)
		if err != nil {
			return nil, err
		}
		var v Value
		if err := DecodeRequest(resolved, &v, limits.MaxRequestBytes); err != nil {
			return nil, err
		}
		result[field] = v
	}
	return result, nil
}
func (op Operation) ResolveTarget(aliases map[string]Reference) (Target, error) {
	if op.Target == nil {
		return Target{}, invalid("invalid_field")
	}
	t := *op.Target
	if strings.HasPrefix(t.ID, "$") {
		r, ok := aliases[strings.TrimPrefix(t.ID, "$")]
		if !ok {
			return t, invalid("invalid_request")
		}
		if t.Type != r.MetaID {
			return t, invalid("not_found")
		}
		t.ID = r.ObjectID
	}
	return t, nil
}

func (op Operation) MarshalJSON() ([]byte, error) {
	fields := map[string]any{"op": op.Op}
	if op.SchemaVersion != "" {
		fields["schema_version"] = op.SchemaVersion
	}
	if op.Type != "" {
		fields["type"] = op.Type
	}
	if op.Target != nil {
		fields["target"] = op.Target
	}
	if op.Meta != nil {
		fields["meta"] = op.Meta
	}
	if op.Fields != nil {
		fields["fields"] = op.Fields
	}
	if op.As != "" {
		fields["as"] = op.As
	}
	if op.Filter != nil {
		fields["filter"] = op.Filter
	}
	if op.ObjectIDs != nil {
		fields["object_ids"] = op.ObjectIDs
	}
	if op.Version != nil {
		fields["version"] = op.Version
	}
	if op.Guards != nil {
		fields["guards"] = op.Guards
	}
	if op.Cascade {
		fields["cascade"] = true
	}
	if op.Confirm != "" {
		fields["confirm"] = op.Confirm
	}
	return MarshalWire(fields)
}
