package protocol

import (
	"encoding/json"
	"strings"
)

// ResolvePredicateAliases only traverses predicate grammar. It never examines
// application JSON/text, annotations, or arbitrary values for '$' spellings.
func ResolvePredicateAliases(raw json.RawMessage, aliases map[string]Reference) (json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return raw, nil
	}
	var predicate map[string]json.RawMessage
	if err := json.Unmarshal(raw, &predicate); err != nil {
		return nil, invalid("invalid_filter")
	}
	for kind, value := range predicate {
		switch kind {
		case "and", "or":
			var children []json.RawMessage
			if json.Unmarshal(value, &children) != nil {
				return nil, invalid("invalid_filter")
			}
			for i, child := range children {
				resolved, err := ResolvePredicateAliases(child, aliases)
				if err != nil {
					return nil, err
				}
				children[i] = resolved
			}
			b, err := MarshalWire(children)
			if err != nil {
				return nil, err
			}
			predicate[kind] = b
		case "not":
			resolved, err := ResolvePredicateAliases(value, aliases)
			if err != nil {
				return nil, err
			}
			predicate[kind] = resolved
		case "object_ids":
			var ids []string
			if json.Unmarshal(value, &ids) != nil {
				return nil, invalid("invalid_filter")
			}
			ids, err := ResolveObjectAliases(ids, aliases)
			if err != nil {
				return nil, err
			}
			predicate[kind], err = MarshalWire(ids)
			if err != nil {
				return nil, err
			}
		case "field":
			var field map[string]json.RawMessage
			if json.Unmarshal(value, &field) != nil {
				return nil, invalid("invalid_filter")
			}
			if exact, ok := field["exact"]; ok {
				resolved, err := resolveReferenceAlias(exact, aliases)
				if err != nil {
					return nil, err
				}
				field["exact"] = resolved
			}
			if rawRange, ok := field["range"]; ok {
				var bounds map[string]json.RawMessage
				if json.Unmarshal(rawRange, &bounds) != nil {
					return nil, invalid("invalid_filter")
				}
				for key, v := range bounds {
					resolved, err := resolveReferenceAlias(v, aliases)
					if err != nil {
						return nil, err
					}
					bounds[key] = resolved
				}
				b, err := MarshalWire(bounds)
				if err != nil {
					return nil, err
				}
				field["range"] = b
			}
			b, err := MarshalWire(field)
			if err != nil {
				return nil, err
			}
			predicate[kind] = b
		}
	}
	return MarshalWire(predicate)
}
func resolveReferenceAlias(raw json.RawMessage, aliases map[string]Reference) (json.RawMessage, error) {
	var value struct {
		Type  Type            `json:"type"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, invalid("invalid_field_value")
	}
	if value.Type != ReferenceType {
		return raw, nil
	}
	var members map[string]json.RawMessage
	_ = json.Unmarshal(raw, &members)
	if len(members) != 2 {
		return nil, invalid("invalid_request")
	}
	var name string
	if json.Unmarshal(value.Value, &name) == nil && strings.HasPrefix(name, "$") {
		ref, ok := aliases[strings.TrimPrefix(name, "$")]
		if !ok {
			return nil, invalid("invalid_request")
		}
		v, err := NewValue(ReferenceType, ref)
		if err != nil {
			return nil, err
		}
		return v.MarshalJSON()
	}

	return raw, nil
}

// ResolveObjectAliases applies only to grammar positions holding object IDs.
func ResolveObjectAliases(ids []string, aliases map[string]Reference) ([]string, error) {
	result := append([]string{}, ids...)
	for i, id := range result {
		if strings.HasPrefix(id, "$") {
			ref, ok := aliases[strings.TrimPrefix(id, "$")]
			if !ok {
				return nil, invalid("invalid_request")
			}
			result[i] = ref.ObjectID
		}
	}
	return result, nil
}
