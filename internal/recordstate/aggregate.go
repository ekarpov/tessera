package recordstate

import (
	"encoding/json"
	"math"
	"math/big"
	"sort"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// exact scans one concrete type. Aggregation and distinct references are
// exact-type only, unlike count, which agrees with a descendant-scoped query.
// Matching applies the caller's view first, so grouping and limits are computed
// inside it rather than over rows already gathered.
// ordered says whether the caller depends on the scan order. An aggregate
// sorts its groups by name and does not; distinct references are sorted with
// a stable sort, so for them the scan order is still observable.
func (t *transaction) exact(typeID string, predicate *p.Predicate, ordered bool) ([]store.ObjectKey, error) {
	keys, err := t.matching(typeID, predicate, ordered)
	if err != nil {
		return nil, err
	}
	rows := make([]store.ObjectKey, 0, len(keys))
	for _, key := range keys {
		if key.TypeID == typeID {
			rows = append(rows, key)
		}
	}
	return rows, nil
}

func (t *transaction) fieldType(typeID, fieldID string) (p.Type, error) {
	meta, err := t.effective(typeID)
	if err != nil {
		return "", err
	}
	for _, f := range meta.Fields {
		if f.ID == fieldID {
			return f.Type, nil
		}
	}
	return "", store.Refuse("invalid_field")
}

// accumulator sums exactly and rounds at most once, because the specification
// makes both observable. Row order must not change the answer or cause a
// premature failure: a total that leaves the range and comes back, as
// int64 max plus one minus one does, is in range and must be reported.
//
// So integers accumulate in a big.Int rather than an int64, and floats in an
// exact expansion rather than a running float64. A running accumulator of
// either kind gets order-dependent answers, which is the defect this shape
// exists to prevent rather than an optimisation it forgoes.
type accumulator struct {
	kind                   p.Type
	count                  int
	integer                big.Int
	float                  *big.Float
	exact                  *big.Rat
	maxScale               int
	minimum, maximum       p.Value
	hasMinimum, hasMaximum bool
}

func (a *accumulator) add(v p.Value) error {
	switch v.Type() {
	case p.FixedType:
		// A sum of decimals is a decimal: every addend has a finite scale, so
		// the total does too and a rational holds it exactly. No rounding is
		// possible here and none is performed, which is the whole reason this
		// type exists rather than being carried as a float.
		term, ok := new(big.Rat).SetString(v.Payload().(string))
		if !ok {
			return store.Refuse("invalid_field_value")
		}
		if a.kind != "" && a.kind != p.FixedType {
			return store.Refuse("invalid_field")
		}
		a.kind = p.FixedType
		if a.exact == nil {
			a.exact = new(big.Rat)
		}
		a.exact.Add(a.exact, term)
		// The total's scale is the widest any addend carried, which is what
		// makes rendering it exact rather than rounded.
		if scale := p.FixedScale(v.Payload().(string)); scale > a.maxScale {
			a.maxScale = scale
		}
	case p.IntegerType:
		n := v.Payload().(int64)
		if a.kind == p.FloatType {
			a.addFloat(float64(n))
		} else {
			a.kind = p.IntegerType
			a.integer.Add(&a.integer, big.NewInt(n))
		}
	case p.FloatType:
		if a.kind == p.IntegerType {
			// A mixed field should not occur, since values are converted to
			// the declared type on write. Fall back to float rather than
			// silently dropping the integers already counted.
			exact, _ := new(big.Float).SetInt(&a.integer).Float64()
			a.integer.SetInt64(0)
			a.addFloat(exact)
		}
		a.kind = p.FloatType
		a.addFloat(v.Payload().(float64))
	default:
		return store.Refuse("invalid_field")
	}
	a.count++
	if !a.hasMinimum {
		a.minimum, a.maximum, a.hasMinimum, a.hasMaximum = v, v, true, true
		return nil
	}
	// Compare exactly rather than through float64, so an integer extreme
	// beyond 2^53 is not reported as its rounded neighbour.
	if n, err := p.Compare(v, a.minimum); err != nil {
		return err
	} else if n < 0 {
		a.minimum = v
	}
	if n, err := p.Compare(v, a.maximum); err != nil {
		return err
	} else if n > 0 {
		a.maximum = v
	}
	return nil
}

// floatPrecision is wide enough that adding binary64 values is exact. Their
// exponents span about 2100 powers of two, so a significand this wide cannot
// drop a bit however far apart two addends are, and the exponent range of a
// big.Float is far beyond binary64's — which is what lets a total leave the
// representable range and come back, as two maxima and a negation do.
const floatPrecision = 2400

// addFloat accumulates exactly. A running float64 rounds at every step and so
// gets order-dependent answers, and a compensated expansion still overflows
// internally near the top of the range; neither is allowed to decide this.
func (a *accumulator) addFloat(x float64) {
	if a.float == nil {
		a.float = new(big.Float).SetPrec(floatPrecision).SetMode(big.ToNearestEven)
	}
	a.float.Add(a.float, new(big.Float).SetPrec(floatPrecision).SetFloat64(x))
}

// floatTotal rounds the exact total once, to nearest with ties to even.
func (a *accumulator) floatTotal() float64 {
	if a.float == nil {
		return 0
	}
	total, _ := a.float.Float64()
	// An exact zero is positive zero: the total of equal opposite values
	// would otherwise be reported as -0 for some orders and +0 for others.
	if total == 0 {
		return 0
	}
	return total
}

var int64Low = big.NewInt(math.MinInt64)
var int64High = big.NewInt(math.MaxInt64)

func number(v p.Value) (json.RawMessage, error) {
	switch v.Type() {
	case p.IntegerType:
		return json.Marshal(v.Payload().(int64))
	case p.FloatType:
		return json.Marshal(v.Payload().(float64))
	}
	return nil, store.Refuse("internal_error")
}

// group identifies one group of a grouped answer: a text label, a reference
// identity, or neither, which is the null group.
type group struct {
	label     *string
	reference *p.Reference
}

func (a *accumulator) statistics(rows int, grouped bool, g group, limits p.Limits) (p.Aggregate, error) {
	entry := p.Aggregate{Grouped: grouped, Group: g.label, GroupReference: g.reference, RowCount: rows, ValueCount: a.count}
	// No row carried a value, so the statistics are null rather than zero.
	if a.count == 0 {
		return entry, nil
	}
	var err error
	if a.kind == p.FixedType {
		canonical := p.CanonicalFixed(a.exact.FloatString(a.maxScale))
		if p.FixedDigits(canonical) > limits.MaxFixedDigits {
			return entry, store.RefuseWith("query_result_limit_exceeded", "sum_overflow", nil)
		}
		if entry.Sum, err = json.Marshal(canonical); err != nil {
			return entry, err
		}
		if entry.Minimum, err = json.Marshal(a.minimum.Payload()); err != nil {
			return entry, err
		}
		entry.Maximum, err = json.Marshal(a.maximum.Payload())
		return entry, err
	}
	if a.kind == p.IntegerType {
		// Exact within the signed 64-bit range, refused outside it, whatever
		// width the implementation could have managed.
		if a.integer.Cmp(int64Low) < 0 || a.integer.Cmp(int64High) > 0 {
			return entry, store.RefuseWith("query_result_limit_exceeded", "sum_overflow", nil)
		}
		entry.Sum, err = json.Marshal(a.integer.Int64())
	} else {
		total := a.floatTotal()
		if math.IsInf(total, 0) || math.IsNaN(total) {
			return entry, store.RefuseWith("query_result_limit_exceeded", "sum_overflow", nil)
		}
		entry.Sum, err = json.Marshal(total)
	}
	if err != nil {
		return entry, err
	}
	if entry.Minimum, err = number(a.minimum); err != nil {
		return entry, err
	}
	entry.Maximum, err = number(a.maximum)
	return entry, err
}

// groupKey turns a grouped field's value into a key that identifies its group
// and sorts in the order the protocol specifies.
//
// The separator is a NUL, which no identifier may contain, so the three parts
// of a reference compare part by part rather than running together: without it
// a meta_id ending where the next begins would order two genuinely different
// objects as one range. The kind prefixes keep a text group and a reference
// group apart even where their spellings would collide, and put the null group
// first in every answer — a fixed position a client can rely on rather than
// one that depends on what the data happened to contain.
func groupKey(v p.Value) (string, group, error) {
	switch v.Type() {
	case p.NullType:
		// A null group is a group. Dropping it would report a total that
		// silently excludes rows the same filter counted.
		return "0null", group{}, nil
	case p.TextType:
		text := v.Payload().(string)
		return "1text:" + text, group{label: &text}, nil
	case p.ReferenceType:
		reference, ok := v.Payload().(p.Reference)
		if !ok || reference.Validate() != nil {
			return "", group{}, store.Refuse("invalid_field_value")
		}
		return "2reference:" + reference.MetaID + "\x00" + reference.DatasetID + "\x00" + reference.ObjectID,
			group{reference: &reference}, nil
	}
	return "", group{}, store.Refuse("invalid_field")
}

// Aggregate counts matching rows and summarises one numeric field over them.
// field may be empty for a count alone; groupBy may be empty for one ungrouped
// answer, which is always returned even when nothing matched.
func (t *transaction) Aggregate(typeID, field, groupBy string, predicate *p.Predicate) ([]p.Aggregate, error) {
	if err := t.check(t.auth.Grants.Read); err != nil {
		return nil, err
	}
	if field != "" {
		kind, err := t.fieldType(typeID, field)
		if err != nil {
			return nil, err
		}
		if kind != p.IntegerType && kind != p.FloatType && kind != p.FixedType {
			return nil, store.Refuse("invalid_field")
		}
	}
	if groupBy != "" {
		kind, err := t.fieldType(typeID, groupBy)
		if err != nil {
			return nil, err
		}
		// Text and reference, and nothing else. Grouping is equality on an
		// opaque value, and both of these are that: the protocol already
		// defines equality and a total order over references, so this needs no
		// new comparison and consults nothing about what a reference means.
		// A number or a timestamp would be a bucket, and a bucket is policy.
		if kind != p.TextType && kind != p.ReferenceType {
			return nil, store.Refuse("invalid_field")
		}
	}
	rows, err := t.exact(typeID, predicate, false)
	if err != nil {
		return nil, err
	}
	if groupBy == "" {
		totals := &accumulator{}
		for _, key := range rows {
			if err = t.ctx.Err(); err != nil {
				return nil, err
			}
			if v := t.held(key.ObjectID).fields[field]; field != "" && v.Type() != p.NullType {
				if err = totals.add(v); err != nil {
					return nil, err
				}
			}
		}
		entry, e := totals.statistics(len(rows), false, group{}, t.engine.limits)
		if e != nil {
			return nil, e
		}
		return []p.Aggregate{entry}, nil
	}
	// A null group is a group. Dropping it would report a total that silently
	// excludes rows the same filter counted.
	counts := map[string]int{}
	groups := map[string]*accumulator{}
	identities := map[string]group{}
	order := []string{}
	bound := t.engine.limits.MaxAggregateGroups
	for _, key := range rows {
		if err = t.ctx.Err(); err != nil {
			return nil, err
		}
		fields := t.held(key.ObjectID).fields
		name, identity, e := groupKey(fields[groupBy])
		if e != nil {
			return nil, e
		}
		if _, seen := groups[name]; !seen {
			if len(order) == bound {
				return nil, store.RefuseWith("query_result_limit_exceeded", "group_limit",
					map[string]any{"maximum": bound})
			}
			groups[name], identities[name], order = &accumulator{}, identity, append(order, name)
		}
		counts[name]++
		if v := fields[field]; field != "" && v.Type() != p.NullType {
			if err = groups[name].add(v); err != nil {
				return nil, err
			}
		}
	}
	// Sorting the keys sorts the answer, because each key is built to make
	// lexical order the specified order: null first, then text by code point,
	// then references by meta_id, dataset_id, object_id, each by code point.
	sort.Strings(order)
	result := make([]p.Aggregate, 0, len(order))
	for _, name := range order {
		entry, e := groups[name].statistics(counts[name], true, identities[name], t.engine.limits)
		if e != nil {
			return nil, e
		}
		result = append(result, entry)
	}
	return result, nil
}

// DistinctReferences returns the distinct values of one reference field, sorted
// by qualified identity. Exceeding maximum is refused rather than truncated: a
// short list that looks complete is the failure this bound exists to prevent.
func (t *transaction) DistinctReferences(typeID, field string, maximum int, predicate *p.Predicate) ([]p.Reference, error) {
	if err := t.check(t.auth.Grants.Read); err != nil {
		return nil, err
	}
	if maximum < 1 || maximum > t.engine.limits.MaxDistinctReferences {
		return nil, store.Refuse("invalid_field")
	}
	kind, err := t.fieldType(typeID, field)
	if err != nil {
		return nil, err
	}
	if kind != p.ReferenceType {
		return nil, store.Refuse("invalid_field")
	}
	rows, err := t.exact(typeID, predicate, true)
	if err != nil {
		return nil, err
	}
	seen := map[p.Reference]struct{}{}
	values := []p.Value{}
	for _, key := range rows {
		if err = t.ctx.Err(); err != nil {
			return nil, err
		}
		v := t.held(key.ObjectID).fields[field]
		// A null or otherwise unusable stored value fails rather than being
		// skipped, which would answer "these are the references" while hiding
		// that some rows had none.
		if v.Type() != p.ReferenceType {
			return nil, store.Refuse("invalid_field_value")
		}
		reference, ok := v.Payload().(p.Reference)
		if !ok || reference.Validate() != nil {
			return nil, store.Refuse("invalid_field_value")
		}
		if _, repeated := seen[reference]; repeated {
			continue
		}
		if len(values) == maximum {
			return nil, store.RefuseWith("query_result_limit_exceeded", "distinct_reference_limit",
				map[string]any{"maximum": maximum})
		}
		seen[reference] = struct{}{}
		values = append(values, v)
	}
	var failure error
	sort.SliceStable(values, func(i, j int) bool {
		n, e := p.Compare(values[i], values[j])
		if e != nil {
			failure = e
		}
		return n < 0
	})
	if failure != nil {
		return nil, failure
	}
	result := make([]p.Reference, 0, len(values))
	for _, v := range values {
		result = append(result, v.Payload().(p.Reference))
	}
	return result, nil
}
