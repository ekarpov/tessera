package protocol

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
)

// MarshalWire encodes protocol envelopes without encoding/json's compaction of
// nested Marshaler results. That compaction would rewrite opaque annotation
// values (including whitespace, escapes, and number spellings). Use this at the
// HTTP boundary and for persisted metadata. It accepts acyclic protocol models.
func MarshalWire(value any) ([]byte, error) { return appendWire(nil, reflect.ValueOf(value)) }

func appendWire(out []byte, v reflect.Value) ([]byte, error) {
	if !v.IsValid() {
		return append(out, "null"...), nil
	}
	if (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface || v.Kind() == reflect.Map || v.Kind() == reflect.Slice) && v.IsNil() {
		return append(out, "null"...), nil
	}
	if v.CanInterface() {
		if m, ok := v.Interface().(json.Marshaler); ok {
			b, err := m.MarshalJSON()
			if err != nil {
				return nil, err
			}
			if !json.Valid(b) {
				return nil, invalid("internal_error")
			}
			return append(out, b...), nil
		}
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		return appendWire(out, v.Elem())
	case reflect.Struct:
		out = append(out, '{')
		first := true
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if !f.IsExported() {
				continue
			}
			tag := strings.Split(f.Tag.Get("json"), ",")
			name := tag[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = f.Name
			}
			field := v.Field(i)
			if len(tag) > 1 && tag[1] == "omitempty" && emptyWire(field) {
				continue
			}
			if !first {
				out = append(out, ',')
			}
			first = false
			key, _ := json.Marshal(name)
			out = append(out, key...)
			out = append(out, ':')
			var err error
			out, err = appendWire(out, field)
			if err != nil {
				return nil, err
			}
		}
		return append(out, '}'), nil
	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String {
			return nil, invalid("internal_error")
		}
		keys := v.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
		out = append(out, '{')
		for i, k := range keys {
			if i > 0 {
				out = append(out, ',')
			}
			key, _ := json.Marshal(k.String())
			out = append(out, key...)
			out = append(out, ':')
			var err error
			out, err = appendWire(out, v.MapIndex(k))
			if err != nil {
				return nil, err
			}
		}
		return append(out, '}'), nil
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			b, err := json.Marshal(v.Interface())
			return append(out, b...), err
		}
		out = append(out, '[')
		for i := 0; i < v.Len(); i++ {
			if i > 0 {
				out = append(out, ',')
			}
			var err error
			out, err = appendWire(out, v.Index(i))
			if err != nil {
				return nil, err
			}
		}
		return append(out, ']'), nil
	default:
		b, err := json.Marshal(v.Interface())
		return append(out, b...), err
	}
}
func emptyWire(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	default:
		return v.IsZero()
	}
}
