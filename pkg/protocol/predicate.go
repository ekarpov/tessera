package protocol

import (
	"encoding/json"
	"strings"
)

// Predicate is validated before any row is evaluated, even for empty datasets.
type Predicate struct {
	Kind         string
	Field        string
	Exact        *Value
	Lower, Upper *Value
	Text         string
	IDs          []string
	After        string
	Children     []*Predicate
}

func ParsePredicate(raw []byte, l Limits) (*Predicate, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if err := checkJSON(raw, true); err != nil {
		return nil, err
	}
	return parsePredicate(raw, l, 1)
}
func parsePredicate(raw []byte, l Limits, depth int) (*Predicate, error) {
	if depth > l.MaxPredicateDepth {
		return nil, invalid("invalid_filter")
	}
	var members map[string]json.RawMessage
	if json.Unmarshal(raw, &members) != nil || len(members) != 1 {
		return nil, invalid("invalid_filter")
	}
	p := &Predicate{}
	for kind, v := range members {
		p.Kind = kind
		switch kind {
		case "field":
			var f struct {
				ID    string `json:"id"`
				Exact *Value `json:"exact"`
				Range *struct {
					Lower *Value `json:"lower"`
					Upper *Value `json:"upper"`
				} `json:"range"`
			}
			if DecodeRequest(v, &f, l.MaxRequestBytes) != nil || (f.Exact == nil) == (f.Range == nil) {
				return nil, invalid("invalid_filter")
			}
			p.Field = f.ID
			p.Exact = f.Exact
			if f.Range != nil {
				p.Lower = f.Range.Lower
				p.Upper = f.Range.Upper
				if p.Lower == nil && p.Upper == nil {
					return nil, invalid("invalid_filter")
				}
			}
		case "text_contains":
			var f struct {
				FieldID string  `json:"field_id"`
				Text    *string `json:"text"`
			}
			if DecodeRequest(v, &f, l.MaxRequestBytes) != nil || f.Text == nil {
				return nil, invalid("invalid_filter")
			}
			p.Field = f.FieldID
			p.Text = *f.Text
		case "object_ids":
			if json.Unmarshal(v, &p.IDs) != nil || p.IDs == nil || len(p.IDs) > 256 {
				return nil, invalid("invalid_filter")
			}
			for _, id := range p.IDs {
				if ValidateIdentifier(ObjectIdentifier, id) != nil {
					return nil, invalid("invalid_filter")
				}
			}
		case "object_id_after":
			if json.Unmarshal(v, &p.After) != nil || ValidateIdentifier(ObjectIdentifier, p.After) != nil {
				return nil, invalid("invalid_filter")
			}
		case "and", "or", "not":
			var children []json.RawMessage
			if kind == "not" {
				children = []json.RawMessage{v}
			} else if json.Unmarshal(v, &children) != nil || children == nil || len(children) > l.MaxPredicateWidth {
				return nil, invalid("invalid_filter")
			}
			for _, child := range children {
				c, err := parsePredicate(child, l, depth+1)
				if err != nil {
					return nil, err
				}
				p.Children = append(p.Children, c)
			}
		default:
			return nil, invalid("invalid_filter")
		}
	}
	for _, v := range []*Value{p.Exact, p.Lower, p.Upper} {
		if v != nil {
			if err := v.Validate(l); err != nil {
				return nil, err
			}
		}
	}
	if len(p.Text) > l.MaxTextBytes {
		return nil, invalid("invalid_filter")
	}
	return p, nil
}
func (p *Predicate) Validate(meta Meta, textMatching bool) error {
	if p == nil {
		return nil
	}
	if p.Kind == "text_contains" && !textMatching {
		return invalid("not_supported")
	}
	if p.Kind == "field" || p.Kind == "text_contains" {
		var kind Type
		for _, f := range meta.Fields {
			if f.ID == p.Field {
				kind = f.Type
				break
			}
		}
		if kind == "" {
			return invalid("invalid_field")
		}
		if p.Kind == "text_contains" && kind != TextType {
			return invalid("invalid_filter")
		}
		for _, v := range []*Value{p.Exact, p.Lower, p.Upper} {
			if v != nil && v.Type() != NullType && !(numeric(kind) && numeric(v.Type())) && kind != v.Type() {
				return invalid("invalid_filter")
			}
		}
		if p.Exact == nil && p.Kind == "field" && (kind == JSONType || kind == BlobType || kind == MediaType) {
			return invalid("invalid_filter")
		}
	}
	for _, child := range p.Children {
		if err := child.Validate(meta, textMatching); err != nil {
			return err
		}
	}
	return nil
}
func (p *Predicate) Match(id string, fields map[string]Value) (bool, error) {
	if p == nil {
		return true, nil
	}
	switch p.Kind {
	case "field":
		v := fields[p.Field]
		if p.Exact != nil {
			return Equal(v, *p.Exact)
		}
		if v.Type() == NullType {
			return false, nil
		}
		if p.Lower != nil {
			n, err := Compare(v, *p.Lower)
			if err != nil || n < 0 {
				return false, err
			}
		}
		if p.Upper != nil {
			n, err := Compare(v, *p.Upper)
			if err != nil || n > 0 {
				return false, err
			}
		}
		return true, nil
	case "text_contains":
		v := fields[p.Field]
		if v.Type() == NullType {
			return false, nil
		}
		return strings.Contains(v.Payload().(string), p.Text), nil
	case "object_ids":
		for _, v := range p.IDs {
			if v == id {
				return true, nil
			}
		}
		return false, nil
	case "object_id_after":
		return id > p.After, nil
	case "not":
		b, err := p.Children[0].Match(id, fields)
		return !b, err
	case "and", "or":
		for _, c := range p.Children {
			b, err := c.Match(id, fields)
			if err != nil {
				return false, err
			}
			if p.Kind == "and" && !b {
				return false, nil
			}
			if p.Kind == "or" && b {
				return true, nil
			}
		}
		return p.Kind == "and", nil
	}
	return false, invalid("invalid_filter")
}
