package protocol

import (
	"encoding/json"
	"reflect"
	"strings"
)

// Limits are advertised by a server, not inferred by clients from its engine.
type Limits struct {
	MaxMaterializedBytes int `json:"max_materialized_bytes,omitempty"`
	// MaxStoredBytes is the dataset's storage quota: the stored bytes past
	// which a write is refused 507 dataset_full. Zero, and absent on the
	// wire, is no quota. It is not the materialization bound, which is how
	// much one operation may hold in memory and is always declared.
	MaxStoredBytes              int `json:"max_stored_bytes,omitempty"`
	MaxAllowedConcreteTypes     int `json:"max_allowed_concrete_types"`
	MaxSelectionPerConcreteType int `json:"max_selection_per_concrete_type"`
	MaxSortTerms                int `json:"max_sort_terms"`
	MaxProjectedFields          int `json:"max_projected_fields"`
	MaxOverallLimit             int `json:"max_overall_limit"`
	MaxPageSize                 int `json:"max_page_size"`
	MaxDistinctReferences       int `json:"max_distinct_references"`
	// MaxAggregateGroups bounds the distinct groups one aggregation may
	// return. Exceeding it refuses the whole request; a prefix returned as if
	// complete is the answer this bound exists to prevent.
	MaxAggregateGroups int `json:"max_aggregate_groups"`
	// MaxTransactionOperations bounds the operations in one transaction. It
	// was enforced before it was declared, and a caller exceeding it got a
	// bare invalid_field naming neither the bound nor the number — a limit a
	// client could only find by being refused.
	MaxTransactionOperations int `json:"max_transaction_operations"`
	MaxPredicateDepth        int `json:"max_predicate_depth"`
	MaxPredicateWidth        int `json:"max_predicate_width"`
	// MaxFixedDigits bounds the significant digits of a `fixed` value, sign
	// and decimal point excluded. It is a value bound rather than a field
	// bound: a field's own `scale` bounds its fraction digits, and this bounds
	// how large the whole number may be whatever the field declares.
	MaxFixedDigits     int `json:"max_fixed_digits"`
	MaxTextBytes       int `json:"max_text_bytes"`
	MaxInlineBlobBytes int `json:"max_inline_blob_bytes"`
	MaxRequestBytes    int `json:"max_request_bytes"`
	MaxScannedRows     int `json:"max_scanned_rows"`
	MaxRoles           int `json:"max_roles"`
	MaxRoleLength      int `json:"max_role_length"`
	// IdempotencyRetentionSeconds is how long a claimed key stays answerable,
	// measured from the claim. It MUST be at least
	// MinimumIdempotencyRetentionSeconds: a window shorter than that leaves a
	// client unable to reconcile a lost response, which is the one thing the
	// key exists to make possible.
	IdempotencyRetentionSeconds int `json:"idempotency_retention_seconds"`
}

// DevelopmentLimits matches the discovery example. It is an implementation
// choice, not a protocol-wide minimum or default for other implementations.
// MinimumIdempotencyRetentionSeconds is the floor under any advertised
// idempotency retention window: twenty-four hours.
//
// A bound that can be set low enough to void the guarantee it bounds is not a
// bound. Retention was required to be "at least the advertised value" and the
// advertised value was unconstrained, so a server could advertise sixty
// seconds and remain conformant while making reconciliation impossible — and a
// client meeting that has nothing to do but show somebody who lost their
// connection a dead end.
//
// A day rather than an hour because the client that needs this most is on a
// phone, and the gap between losing a response and being able to ask about it
// is a night's sleep.
const MinimumIdempotencyRetentionSeconds = 86400

// DefaultAggregateGroups is the group bound in force where a dataset declares
// none. It was a constant in the evaluator before it was a declared limit, and
// the value has not changed: moving it was about a client being able to read
// it before planning a query, not about what it should be.
const DefaultAggregateGroups = 32

// DefaultTransactionOperations is the operation bound where a dataset declares
// none, and the value that was enforced before it was declared.
const DefaultTransactionOperations = 256

func DevelopmentLimits() Limits {
	return Limits{
		MaxMaterializedBytes:        64 << 20,
		MaxAllowedConcreteTypes:     128,
		MaxSelectionPerConcreteType: 100,
		MaxSortTerms:                16,
		MaxProjectedFields:          256,
		MaxOverallLimit:             10000,
		MaxPageSize:                 1000,
		MaxDistinctReferences:       256,
		MaxAggregateGroups:          DefaultAggregateGroups,
		MaxTransactionOperations:    DefaultTransactionOperations,
		MaxPredicateDepth:           32,
		MaxPredicateWidth:           256,
		// Comfortably past binary64's 17 significant digits and past
		// Foundation.Decimal's 38, so a value that defeats either is one this
		// protocol permits rather than one it happens to exclude. A bound
		// stopping short of the traps would make their vector cases
		// unwritable.
		MaxFixedDigits:              64,
		MaxTextBytes:                1048576,
		MaxInlineBlobBytes:          262144,
		MaxRequestBytes:             4194304,
		MaxScannedRows:              200000,
		MaxRoles:                    16,
		MaxRoleLength:               64,
		IdempotencyRetentionSeconds: 86400,
	}
}

// UnmarshalJSON applies the group bound's default when it is absent.
//
// A dataset stored before `max_aggregate_groups` had a name decodes it as
// zero, and zero is not a bound of nothing — it is a row written before the
// bound existed. Reading it as a bound would refuse every grouped aggregation
// on every dataset this deployment already holds.
//
// It lives here rather than at each decode site because there are two of
// those today and the next one will not remember.
func (l *Limits) UnmarshalJSON(data []byte) error {
	type plain Limits
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*l = Limits(decoded)
	if l.MaxAggregateGroups <= 0 {
		l.MaxAggregateGroups = DefaultAggregateGroups
	}
	if l.MaxTransactionOperations <= 0 {
		l.MaxTransactionOperations = DefaultTransactionOperations
	}
	return nil
}

// Validate refuses limits a server could not honestly advertise: a bound of
// zero or less means nothing can be done, and a retention window under the
// floor voids the reconciliation it exists for.
func (l Limits) Validate() error {
	for _, v := range []int{l.MaxMaterializedBytes, l.MaxAllowedConcreteTypes, l.MaxSelectionPerConcreteType, l.MaxSortTerms,
		l.MaxProjectedFields, l.MaxOverallLimit, l.MaxPageSize, l.MaxDistinctReferences, l.MaxAggregateGroups,
		l.MaxPredicateDepth, l.MaxPredicateWidth, l.MaxFixedDigits, l.MaxTransactionOperations, l.MaxRequestBytes, l.MaxScannedRows, l.MaxRoles, l.MaxRoleLength} {
		if v <= 0 {
			return invalid("invalid_field")
		}
	}
	if l.MaxTextBytes < 0 || l.MaxInlineBlobBytes < 0 || l.MaxStoredBytes < 0 || l.IdempotencyRetentionSeconds < MinimumIdempotencyRetentionSeconds {
		return invalid("invalid_field")
	}
	return nil
}

// limitNames are every member a limits object may name, read from the struct
// rather than from one encoding of it: an optional member that encoding omits
// when unset, such as max_stored_bytes, is still one an override may set.
var limitNames = func() map[string]bool {
	names := map[string]bool{}
	t := reflect.TypeOf(Limits{})
	for i := 0; i < t.NumField(); i++ {
		if name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ","); name != "" && name != "-" {
			names[name] = true
		}
	}
	return names
}()

// MergeLimits applies a partial limits object over base: members present in
// raw replace base's, absent ones keep it, and an unknown member is refused.
//
// Decoding raw straight into a Limits would not do this. Its decoder starts
// from zero, so every member the override left out would become a bound of
// nothing — a configuration raising one limit would silently zero the rest.
func MergeLimits(base Limits, raw json.RawMessage) (Limits, error) {
	if len(raw) == 0 {
		return base, nil
	}
	var override map[string]json.RawMessage
	if err := json.Unmarshal(raw, &override); err != nil {
		return Limits{}, invalid("invalid_field")
	}
	encoded, err := json.Marshal(base)
	if err != nil {
		return Limits{}, err
	}
	merged := map[string]json.RawMessage{}
	if err = json.Unmarshal(encoded, &merged); err != nil {
		return Limits{}, err
	}
	for name, value := range override {
		if !limitNames[name] {
			return Limits{}, invalid("invalid_field")
		}
		merged[name] = value
	}
	combined, err := json.Marshal(merged)
	if err != nil {
		return Limits{}, err
	}
	var result Limits
	if err = json.Unmarshal(combined, &result); err != nil {
		return Limits{}, invalid("invalid_field")
	}
	return result, result.Validate()
}
