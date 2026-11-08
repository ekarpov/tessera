package protocol

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type Type string

const (
	NullType      Type = "null"
	IntegerType   Type = "integer"
	FloatType     Type = "float"
	TextType      Type = "text"
	BooleanType   Type = "boolean"
	DateTimeType  Type = "date_time"
	MediaType     Type = "media"
	JSONType      Type = "json"
	BlobType      Type = "blob"
	ReferenceType Type = "reference"
	// FixedType is an exact decimal, carried as a canonical string so it never
	// passes through a binary floating-point reader.
	//
	// It is not spelled `decimal` because a frozen vector case uses that name
	// as its example of a type no server knows, and keeping the name would
	// leave the case passing for a different reason than it was written for.
	// `fixed` names the representation rather than a use: fixed-point against
	// float's floating point. `money` would be a vocabulary, which this
	// protocol does not carry.
	FixedType Type = "fixed"
)

func (t Type) ValidFieldType() bool {
	switch t {
	case IntegerType, FloatType, FixedType, TextType, BooleanType, DateTimeType, MediaType, JSONType, BlobType, ReferenceType:
		return true
	}
	return false
}

type Reference struct {
	DatasetID string `json:"dataset_id"`
	ObjectID  string `json:"object_id"`
	MetaID    string `json:"meta_id"`
}

func (r Reference) Validate() error {
	for kind, id := range map[IdentifierKind]string{DatasetIdentifier: r.DatasetID, ObjectIdentifier: r.ObjectID, MetaIdentifier: r.MetaID} {
		if err := ValidateIdentifier(kind, id); err != nil {
			return err
		}
	}
	return nil
}

// Value is an immutable tagged value. Its zero value is null. Numbers are stored
// in their actual integer/float representation, never in a generic JSON double.
type Value struct {
	kind  Type
	value any
}

func (v Value) Type() Type {
	if v.kind == "" {
		return NullType
	}
	return v.kind
}

// Payload returns int64, float64, string, bool, Reference, or nil. Blob payloads
// are canonical base64 strings; JSON payloads are the original JSON text.
func (v Value) Payload() any { return v.value }

// NewValue validates a payload through the wire contract. Prefer int64 for
// integer payloads. It never applies conversion to a field's declared type.
func NewValue(kind Type, payload any) (Value, error) {
	if s, ok := payload.(string); ok && !utf8.ValidString(s) {
		return Value{}, invalid("invalid_field_value")
	}
	if r, ok := payload.(Reference); ok {
		if err := r.Validate(); err != nil {
			return Value{}, err
		}
	}
	wire := map[string]any{"type": kind}
	if kind != NullType {
		wire["value"] = payload
	} else if payload != nil {
		return Value{}, invalid("invalid_field_value")
	}
	data, err := json.Marshal(wire)
	if err != nil {
		return Value{}, invalid("invalid_field_value")
	}
	var v Value
	err = json.Unmarshal(data, &v)
	return v, err
}

func (v Value) MarshalJSON() ([]byte, error) {
	if v.Type() == NullType {
		return []byte(`{"type":"null"}`), nil
	}
	return json.Marshal(struct {
		Type  Type `json:"type"`
		Value any  `json:"value"`
	}{v.Type(), v.value})
}

var (
	integerText = regexp.MustCompile(`^-?[0-9]+$`)
	jsonNumber  = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)
	dateText    = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{3}Z$`)
	// fixedText is the canonical form and the only accepted one: an optional
	// sign, an integer part without leading zeros, and — when there is a
	// fraction at all — at least one digit and no trailing zero. No exponent,
	// no plus, and no `-0`, which the sign group forbids by requiring a
	// non-zero digit somewhere after a minus.
	fixedText = regexp.MustCompile(`^(0|-?[1-9][0-9]*)(\.[0-9]*[1-9])?$|^-?0\.[0-9]*[1-9]$`)
)

func (v *Value) UnmarshalJSON(data []byte) error {
	if err := checkJSON(data, true); err != nil {
		return err
	}
	var wire struct {
		Type  Type            `json:"type"`
		Value json.RawMessage `json:"value"`
	}
	if json.Unmarshal(data, &wire) != nil {
		return invalid("invalid_field_value")
	}
	if wire.Type == NullType {
		if len(wire.Value) != 0 {
			return invalid("invalid_field_value")
		}
		*v = Value{}
		return nil
	}
	if !wire.Type.ValidFieldType() || len(wire.Value) == 0 || bytes.Equal(bytes.TrimSpace(wire.Value), []byte("null")) {
		return invalid("invalid_field_value")
	}
	var payload any
	switch wire.Type {
	case IntegerType:
		// SetString parses decimal exponents exactly. Validate grammar first to
		// exclude rational notation, hexadecimal, and other big.Rat extensions.
		s := string(wire.Value)
		if !jsonNumber.MatchString(s) {
			return invalid("invalid_field_value")
		}
		n, ok := exactInteger(s)
		if !ok {
			return invalid("invalid_field_value")
		}
		payload = n
	case FloatType:
		if !jsonNumber.Match(wire.Value) {
			return invalid("invalid_field_value")
		}
		n, err := strconv.ParseFloat(string(wire.Value), 64)
		if err != nil || math.IsInf(n, 0) || math.IsNaN(n) {
			return invalid("invalid_field_value")
		}
		payload = n
	case BooleanType:
		var b bool
		if json.Unmarshal(wire.Value, &b) != nil {
			return invalid("invalid_field_value")
		}
		payload = b
	case ReferenceType:
		var r Reference
		if json.Unmarshal(wire.Value, &r) != nil {
			return invalid("invalid_field_value")
		}
		if err := r.Validate(); err != nil {
			return invalid("invalid_field_value")
		}
		payload = r
	default:
		var s string
		if json.Unmarshal(wire.Value, &s) != nil {
			return invalid("invalid_field_value")
		}
		switch wire.Type {
		case MediaType:
			if _, _, err := ParseContentReference(s); err != nil {
				return invalid("invalid_field_value")
			}
		case DateTimeType:
			if !dateText.MatchString(s) {
				return invalid("invalid_field_value")
			}
			if _, err := time.Parse("2006-01-02T15:04:05.000Z", s); err != nil {
				return invalid("invalid_field_value")
			}
		case JSONType:
			if err := checkJSON([]byte(s), false); err != nil {
				return invalid("invalid_field_value")
			}
		case BlobType:
			decoded, err := base64.StdEncoding.Strict().DecodeString(s)
			if err != nil || base64.StdEncoding.EncodeToString(decoded) != s {
				return invalid("invalid_field_value")
			}
		case FixedType:
			// Canonical only. A server that normalised `1.50` to `1.5` would
			// return a value the client did not send, and this protocol
			// compares raw values in `expect` and folds them into record
			// versions — so a silently rewritten value is a mismatch a client
			// cannot explain. Refusing is the loud half of that choice.
			if !fixedText.MatchString(s) {
				return invalid("invalid_field_value")
			}
		}
		payload = s
	}
	*v = Value{wire.Type, payload}
	return nil
}

// exactInteger avoids allocating exponent-sized big integers for hostile input.
// A decimal lexeme is reduced to at most 19 significant integral digits first.
func exactInteger(s string) (int64, bool) {
	negative := len(s) > 0 && s[0] == '-'
	if negative {
		s = s[1:]
	}
	exponent := int64(0)
	var exponentError error
	for i, c := range s {
		if c == 'e' || c == 'E' {
			exponent, exponentError = strconv.ParseInt(s[i+1:], 10, 64)
			s = s[:i]
			break
		}
	}
	digits := make([]byte, 0, len(s))
	frac := int64(0)
	dot := false
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			dot = true
			continue
		}
		digits = append(digits, s[i])
		if dot {
			frac++
		}
	}
	for len(digits) > 0 && digits[0] == '0' {
		digits = digits[1:]
	}
	if len(digits) == 0 {
		return 0, true
	}
	if exponentError != nil {
		return 0, false
	}
	// Any exponent outside this range cannot bring a bounded input into int64.
	if exponent > int64(len(s))+19 || exponent < -int64(len(s))-19 {
		return 0, false
	}
	scale := exponent - frac
	for scale < 0 && len(digits) > 0 && digits[len(digits)-1] == '0' {
		digits = digits[:len(digits)-1]
		scale++
	}
	if scale < 0 || int64(len(digits))+scale > 19 {
		return 0, false
	}
	for ; scale > 0; scale-- {
		digits = append(digits, '0')
	}
	if negative {
		digits = append([]byte{'-'}, digits...)
	}
	n, err := strconv.ParseInt(string(digits), 10, 64)
	return n, err == nil
}

// Validate enforces deployment-specific payload bounds after syntax decoding.
func (v Value) Validate(limits Limits) error {
	switch v.Type() {
	case TextType:
		if len(v.value.(string)) > limits.MaxTextBytes {
			return invalid("invalid_field_value")
		}
	case BlobType:
		decoded, _ := base64.StdEncoding.DecodeString(v.value.(string))
		if len(decoded) > limits.MaxInlineBlobBytes {
			return invalid("invalid_field_value")
		}
	case FixedType:
		if FixedDigits(v.value.(string)) > limits.MaxFixedDigits {
			return invalid("invalid_field_value")
		}
	}
	return nil
}

// FixedDigits counts the significant digits of a canonical fixed value, sign
// and decimal point excluded. The canonical form has no leading zero in the
// integer part and no trailing zero in the fraction, so every digit it carries
// is one the value needs — except the single `0` of a value less than one,
// which is a placeholder rather than a digit and is not counted.
func FixedDigits(canonical string) int {
	digits := 0
	leading := true
	for _, r := range canonical {
		switch {
		case r == '-' || r == '.':
			continue
		case r == '0' && leading:
			continue
		default:
			leading = false
			digits++
		}
	}
	return digits
}

// CanonicalFixed reduces a decimal literal to the one spelling this protocol
// accepts: no trailing zero in the fraction, no empty fraction, and one zero
// for the value zero however it was signed or padded.
//
// It exists for results the server computes rather than for values it is
// given. A value that arrives non-canonical is refused, because rewriting what
// a caller sent would make `expect` and record versions disagree with it; a
// sum this store computed has no sender to disagree with.
func CanonicalFixed(literal string) string {
	if point := strings.IndexByte(literal, '.'); point >= 0 {
		literal = strings.TrimRight(literal, "0")
		literal = strings.TrimSuffix(literal, ".")
	}
	if literal == "" || literal == "-" || literal == "-0" {
		return "0"
	}
	return literal
}

// FixedScale is the number of fraction digits a canonical fixed value carries.
func FixedScale(canonical string) int {
	if point := strings.IndexByte(canonical, '.'); point >= 0 {
		return len(canonical) - point - 1
	}
	return 0
}

// Convert implements only the specification's declared-field conversion table.
func (v Value) Convert(to Type, limits Limits) (Value, error) {
	if !to.ValidFieldType() {
		return Value{}, invalid("invalid_type")
	}
	if v.Type() == NullType {
		return v, nil
	}
	if v.Type() == to {
		return v, v.Validate(limits)
	}
	var payload any
	switch {
	case v.Type() == IntegerType && to == FloatType:
		i := v.value.(int64)
		f := float64(i)
		if new(big.Rat).SetInt64(i).Cmp(new(big.Rat).SetFloat64(f)) != 0 {
			return Value{}, invalid("invalid_field_value")
		}
		payload = f
	case v.Type() == IntegerType && to == FixedType:
		// Always exact: an integer is a decimal with no fraction, and the
		// canonical form of one is its own decimal spelling.
		payload = strconv.FormatInt(v.value.(int64), 10)
	case v.Type() == FixedType && to == IntegerType:
		s := v.value.(string)
		if strings.ContainsRune(s, '.') {
			return Value{}, invalid("invalid_field_value")
		}
		i, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return Value{}, invalid("invalid_field_value")
		}
		payload = i
	case v.Type() == FixedType && to == FloatType:
		// Permitted only where it loses nothing, which is the rule integer to
		// float already follows. Most decimals are not binary64 values, and a
		// conversion that rounded here would be this protocol deciding that an
		// exact number may quietly stop being one.
		s := v.value.(string)
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsInf(f, 0) {
			return Value{}, invalid("invalid_field_value")
		}
		exact, _ := new(big.Rat).SetString(s)
		if exact.Cmp(new(big.Rat).SetFloat64(f)) != 0 {
			return Value{}, invalid("invalid_field_value")
		}
		payload = f
	case v.Type() == FloatType && to == FixedType:
		// Refused outright rather than conditionally. A binary64 has an exact
		// decimal expansion, so "when it is exact" would admit almost every
		// value and produce the fifty-odd digits of 0.1 — technically exact,
		// and not what any caller asking for a decimal meant.
		return Value{}, invalid("invalid_field_value")
	case v.Type() == FloatType && to == IntegerType:
		f := v.value.(float64)
		if math.Trunc(f) != f || f < -0x1p63 || f >= 0x1p63 {
			return Value{}, invalid("invalid_field_value")
		}
		payload = int64(f)
	case v.Type() == TextType:
		s := v.value.(string)
		switch to {
		case IntegerType:
			if !integerText.MatchString(s) {
				return Value{}, invalid("invalid_field_value")
			}
			i, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				return Value{}, invalid("invalid_field_value")
			}
			payload = i
		case FloatType:
			if !jsonNumber.MatchString(s) {
				return Value{}, invalid("invalid_field_value")
			}
			f, err := strconv.ParseFloat(s, 64)
			if err != nil || math.IsInf(f, 0) {
				return Value{}, invalid("invalid_field_value")
			}
			payload = f
		case FixedType:
			// The same canonical grammar the wire requires. Text is not a
			// looser door into a type whose whole point is one spelling per
			// value.
			if !fixedText.MatchString(s) {
				return Value{}, invalid("invalid_field_value")
			}
			payload = s
		case BooleanType:
			if s != "true" && s != "false" {
				return Value{}, invalid("invalid_field_value")
			}
			payload = s == "true"
		case DateTimeType, MediaType, JSONType, BlobType:
			payload = s
		default:
			return Value{}, invalid("invalid_field_value")
		}
	default:
		return Value{}, invalid("invalid_field_value")
	}
	converted, err := NewValue(to, payload)
	if err != nil {
		return Value{}, err
	}
	return converted, converted.Validate(limits)
}
