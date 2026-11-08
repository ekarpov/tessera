package protocol

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"
)

// DecodeRequest enforces the byte limit before parsing. It is for request bodies;
// json.Unmarshal should be used for forward-compatible response decoding.
// dst must be a non-nil pointer to a concrete wire type, not an interface{}.
func DecodeRequest(data []byte, dst any, maxBytes int) error {
	if maxBytes < 0 || len(data) > maxBytes {
		return invalid("query_result_limit_exceeded")
	}
	if err := checkJSON(data, true); err != nil {
		return err
	}
	if bytes.TrimSpace(data)[0] != '{' {
		return invalid("invalid_field")
	}
	t := reflect.TypeOf(dst)
	if t == nil || t.Kind() != reflect.Pointer || reflect.ValueOf(dst).IsNil() {
		return invalid("invalid_field")
	}
	if err := checkMembers(data, t.Elem()); err != nil {
		return err
	}
	if err := json.Unmarshal(data, dst); err != nil {
		if e, ok := err.(*Error); ok {
			return e
		}
		return invalid("invalid_field")
	}
	return nil
}

// checkJSON rejects the replacement-character repairs made by encoding/json for
// invalid UTF-8 and unpaired UTF-16 surrogates. Iterative token traversal detects
// duplicate keys without converting numbers to float64 or recursing on input.
func checkJSON(data []byte, duplicates bool) error {
	if !utf8.Valid(data) || !validEscapes(data) {
		return invalid("invalid_field_value")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	type frame struct {
		object, key bool
		names       map[string]bool
	}
	var stack []frame
	roots := 0
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return invalid("invalid_field_value")
		}
		if delimiter, ok := tok.(json.Delim); ok && (delimiter == '}' || delimiter == ']') {
			if len(stack) == 0 {
				return invalid("invalid_field_value")
			}
			stack = stack[:len(stack)-1]
			continue
		}
		if len(stack) == 0 {
			roots++
		} else {
			f := &stack[len(stack)-1]
			if f.object {
				if f.key {
					key, ok := tok.(string)
					if !ok {
						return invalid("invalid_field_value")
					}
					if duplicates && f.names[key] {
						return invalid("invalid_field")
					}
					f.names[key] = true
					f.key = false
					continue
				}
				f.key = true
			}
		}
		if delimiter, ok := tok.(json.Delim); ok {
			switch delimiter {
			case '{':
				stack = append(stack, frame{object: true, key: true, names: map[string]bool{}})
			case '[':
				stack = append(stack, frame{})
			}
		}
	}
	if roots != 1 || len(stack) != 0 {
		return invalid("invalid_field_value")
	}
	return nil
}

func validEscapes(data []byte) bool {
	inString := false
	for i := 0; i < len(data); i++ {
		if data[i] == '"' {
			inString = !inString
			continue
		}
		if !inString || data[i] != '\\' {
			continue
		}
		i++
		if i >= len(data) {
			return false
		}
		if data[i] != 'u' {
			continue
		}
		if i+4 >= len(data) {
			return false
		}
		n, err := strconv.ParseUint(string(data[i+1:i+5]), 16, 16)
		if err != nil {
			return false
		}
		i += 4
		if n >= 0xdc00 && n <= 0xdfff {
			return false
		}
		if n >= 0xd800 && n <= 0xdbff {
			if i+6 >= len(data) || data[i+1] != '\\' || data[i+2] != 'u' {
				return false
			}
			low, err := strconv.ParseUint(string(data[i+3:i+7]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 6
		}
	}
	return !inString
}

var rawType = reflect.TypeOf(json.RawMessage{})
var valueType = reflect.TypeOf(Value{})

func checkMembers(data []byte, t reflect.Type) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == rawType {
		return nil
	}
	if t == valueType {
		var fields map[string]json.RawMessage
		if json.Unmarshal(data, &fields) != nil {
			return invalid("invalid_field_value")
		}
		for k := range fields {
			if k != "type" && k != "value" {
				return invalid("invalid_request")
			}
		}
		var kind Type
		_ = json.Unmarshal(fields["type"], &kind)
		if kind == ReferenceType {
			return checkMembers(fields["value"], reflect.TypeOf(Reference{}))
		}
		return nil
	}
	switch t.Kind() {
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if json.Unmarshal(data, &fields) != nil {
			return invalid("invalid_field")
		}
		known := map[string]reflect.Type{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = f.Name
			}
			known[name] = f.Type
		}
		for key, raw := range fields {
			if t == reflect.TypeOf(Meta{}) || t == reflect.TypeOf(Field{}) {
				trimmed := bytes.TrimSpace(raw)
				if key == "schema_version" {
					return invalid("invalid_field")
				}
				if key == "annotations" && (len(trimmed) == 0 || trimmed[0] != '{') {
					return invalid("meta_invalid_field")
				}
				if key == "roles" && (len(trimmed) == 0 || trimmed[0] != '[') {
					return invalid("meta_invalid_field")
				}
			}
			field, ok := known[key]
			if !ok {
				return invalid("invalid_request")
			}
			if err := checkMembers(raw, field); err != nil {
				return err
			}
		}
	case reflect.Map:
		var fields map[string]json.RawMessage
		if json.Unmarshal(data, &fields) != nil {
			return invalid("invalid_field")
		}
		for _, raw := range fields {
			if err := checkMembers(raw, t.Elem()); err != nil {
				return err
			}
		}
	case reflect.Array, reflect.Slice:
		var items []json.RawMessage
		if json.Unmarshal(data, &items) != nil {
			return invalid("invalid_field")
		}
		for _, raw := range items {
			if err := checkMembers(raw, t.Elem()); err != nil {
				return err
			}
		}
	case reflect.Interface:
		return invalid("invalid_field")
	}
	return nil
}
