package protocol_test

import (
	"encoding/json"
	"math"
	"os"
	"reflect"
	"strconv"
	"testing"

	p "github.com/ekarpov/tessera/pkg/protocol"
)

type valueVector struct {
	Name         string `json:"name"`
	Input        string `json:"input"`
	Expected     string `json:"expected"`
	Error        string `json:"error"`
	ConvertTo    p.Type `json:"convert_to"`
	MaxTextBytes *int   `json:"max_text_bytes"`
	MaxBlobBytes *int   `json:"max_inline_blob_bytes"`
}

func loadVectors[T any](t *testing.T, name string) []T {
	t.Helper()
	data, err := os.ReadFile("../../specs/vectors/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var result []T
	if err = json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if len(result) == 0 {
		t.Fatal("empty vector file")
	}
	return result
}

func checkCode(t *testing.T, err error, want string) {
	t.Helper()
	if want == "" {
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	if err == nil || p.ErrorCode(err) != want {
		t.Fatalf("error code = %v, want %s", err, want)
	}
}

func valueEqual(a, b p.Value) bool {
	if a.Type() != b.Type() {
		return false
	}
	if a.Type() == p.FloatType {
		return math.Float64bits(a.Payload().(float64)) == math.Float64bits(b.Payload().(float64))
	}
	return reflect.DeepEqual(a.Payload(), b.Payload())
}

func TestValueVectors(t *testing.T) {
	for _, v := range loadVectors[valueVector](t, "values") {
		t.Run(v.Name, func(t *testing.T) {
			var actual p.Value
			err := json.Unmarshal([]byte(v.Input), &actual)
			limits := p.DevelopmentLimits()
			if v.MaxTextBytes != nil {
				limits.MaxTextBytes = *v.MaxTextBytes
			}
			if v.MaxBlobBytes != nil {
				limits.MaxInlineBlobBytes = *v.MaxBlobBytes
			}
			if err == nil {
				err = actual.Validate(limits)
			}
			if err == nil && v.ConvertTo != "" {
				actual, err = actual.Convert(v.ConvertTo, limits)
			}
			checkCode(t, err, v.Error)
			if err != nil {
				return
			}
			var expected p.Value
			if err = json.Unmarshal([]byte(v.Expected), &expected); err != nil {
				t.Fatal("invalid expected fixture", err)
			}
			if !valueEqual(actual, expected) {
				t.Fatal("decoded/converted value does not match expected fixture")
			}
			encoded, err := json.Marshal(actual)
			if err != nil {
				t.Fatal(err)
			}
			var roundTrip p.Value
			if err = json.Unmarshal(encoded, &roundTrip); err != nil {
				t.Fatal(err)
			}
			if !valueEqual(roundTrip, expected) {
				t.Fatal("encoder changed the expected value")
			}
		})
	}
}

func TestComparisonVectors(t *testing.T) {
	type vector struct {
		Name, Left, Right, Error string
		Equal                    bool
		Order                    *int
		OrderError               string `json:"order_error"`
	}
	for _, v := range loadVectors[vector](t, "comparisons") {
		t.Run(v.Name, func(t *testing.T) {
			var left, right p.Value
			if err := json.Unmarshal([]byte(v.Left), &left); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(v.Right), &right); err != nil {
				t.Fatal(err)
			}
			equal, err := p.Equal(left, right)
			checkCode(t, err, v.Error)
			if err == nil && equal != v.Equal {
				t.Fatal("wrong equality")
			}
			if v.Order != nil || v.OrderError != "" {
				order, err := p.Compare(left, right)
				checkCode(t, err, v.OrderError)
				if err == nil && order != *v.Order {
					t.Fatalf("order %d, want %d", order, *v.Order)
				}
				if err == nil {
					reverse, err := p.Compare(right, left)
					if err != nil || reverse != -order {
						t.Fatal("ordering is not antisymmetric")
					}
				}
			}
		})
	}
}

func TestIdentifierVectors(t *testing.T) {
	type vector struct {
		Name         string
		Kind         p.IdentifierKind
		Value, Error string
	}
	for _, v := range loadVectors[vector](t, "identifiers") {
		t.Run(v.Name, func(t *testing.T) { checkCode(t, p.ValidateIdentifier(v.Kind, v.Value), v.Error) })
	}
}

// A mutation check on the actual public fixtures: an encoder that passes its
// integer through double must be caught, not merely exercised without assertions.
func TestVectorsCatchBrokenEncoders(t *testing.T) {
	mutants := map[string]func(p.Value) p.Value{
		"integer_through_double": func(v p.Value) p.Value {
			if v.Type() == p.IntegerType {
				i := v.Payload().(int64)
				if i == 9007199254740993 {
					result, _ := p.NewValue(p.IntegerType, int64(float64(i)))
					return result
				}
			}
			return v
		},
		// The two traps a `fixed` value exists to survive, one per language
		// family. Each is caught by its own fixture and by no other: Go's
		// arbitrary-precision arithmetic has no 38-digit cliff, and a platform
		// with a 38-digit decimal never reaches ParseFloat.
		"fixed_through_parse_float": func(v p.Value) p.Value {
			if v.Type() == p.FixedType {
				f, err := strconv.ParseFloat(v.Payload().(string), 64)
				if err == nil {
					result, _ := p.NewValue(p.FixedType, p.CanonicalFixed(strconv.FormatFloat(f, 'f', -1, 64)))
					return result
				}
			}
			return v
		},
		"fixed_truncated_to_38_digits": func(v p.Value) p.Value {
			if v.Type() == p.FixedType {
				literal := v.Payload().(string)
				if p.FixedDigits(literal) > 38 {
					// What a fixed-width decimal does to a value it cannot
					// hold: the low digits go, and nothing reports it.
					trimmed := literal[:len(literal)-1]
					result, err := p.NewValue(p.FixedType, p.CanonicalFixed(trimmed))
					if err == nil {
						return result
					}
				}
			}
			return v
		},
		"json_reformatted": func(v p.Value) p.Value {
			if v.Type() == p.JSONType {
				var payload any
				_ = json.Unmarshal([]byte(v.Payload().(string)), &payload)
				data, _ := json.Marshal(payload)
				result, _ := p.NewValue(p.JSONType, string(data))
				return result
			}
			return v
		},
	}
	for name, mutate := range mutants {
		t.Run(name, func(t *testing.T) {
			caught := false
			for _, v := range loadVectors[valueVector](t, "values") {
				if v.Error != "" || v.ConvertTo != "" {
					continue
				}
				var actual, expected p.Value
				if json.Unmarshal([]byte(v.Input), &actual) != nil || json.Unmarshal([]byte(v.Expected), &expected) != nil {
					t.Fatal("invalid fixture")
				}
				if !valueEqual(mutate(actual), expected) {
					caught = true
				}
			}
			if !caught {
				t.Fatal("fixtures did not reject the seeded defect")
			}
		})
	}
}
