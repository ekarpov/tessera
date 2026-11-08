package protocol

import (
	"cmp"
	"math/big"
)

// Equal implements exact predicate and expected-field equality. Null is equal
// only to null; comparing a present value with null is a defined false result.
func Equal(a, b Value) (bool, error) {
	if a.Type() == NullType || b.Type() == NullType {
		return a.Type() == b.Type(), nil
	}
	if numeric(a.Type()) && numeric(b.Type()) {
		return number(a).Cmp(number(b)) == 0, nil
	}
	if a.Type() != b.Type() {
		return false, invalid("invalid_filter")
	}
	// Every stored payload is immutable and comparable. Blob strings are already
	// canonical encodings of their bytes; JSON strings deliberately are not.
	return a.value == b.value, nil
}

// Compare returns -1, 0, or 1 in ascending protocol order. Null precedes all
// present values. JSON, blob and media have equality but no ordering in v1.
func Compare(a, b Value) (int, error) {
	if a.Type() == NullType {
		if b.Type() == NullType {
			return 0, nil
		}
		return -1, nil
	}
	if b.Type() == NullType {
		return 1, nil
	}
	if numeric(a.Type()) && numeric(b.Type()) {
		return number(a).Cmp(number(b)), nil
	}
	if a.Type() != b.Type() {
		return 0, invalid("invalid_filter")
	}
	switch a.Type() {
	case TextType, DateTimeType:
		return cmp.Compare(a.value.(string), b.value.(string)), nil
	case BooleanType:
		if a.value == b.value {
			return 0, nil
		}
		if a.value.(bool) {
			return 1, nil
		}
		return -1, nil
	case ReferenceType:
		x, y := a.value.(Reference), b.value.(Reference)
		if n := cmp.Compare(x.MetaID, y.MetaID); n != 0 {
			return n, nil
		}
		if n := cmp.Compare(x.DatasetID, y.DatasetID); n != 0 {
			return n, nil
		}
		return cmp.Compare(x.ObjectID, y.ObjectID), nil
	}
	return 0, invalid("invalid_filter")
}

func numeric(t Type) bool { return t == IntegerType || t == FloatType || t == FixedType }

// number is the exact rational a numeric value denotes, which is what lets the
// three numeric types compare across each other without any of them being
// rounded into the others. A fixed value is a decimal literal, so SetString is
// exact; the canonical grammar has already excluded everything SetString would
// otherwise accept, including rationals and exponents.
func number(v Value) *big.Rat {
	switch v.Type() {
	case IntegerType:
		return new(big.Rat).SetInt64(v.value.(int64))
	case FixedType:
		r, _ := new(big.Rat).SetString(v.value.(string))
		return r
	}
	return new(big.Rat).SetFloat64(v.value.(float64))
}
