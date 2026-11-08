package recordstate

import (
	"math"
	"math/big"
	"math/rand"
	"testing"

	p "github.com/ekarpov/tessera/pkg/protocol"
)

func sumFloats(t *testing.T, values []float64) float64 {
	t.Helper()
	a := &accumulator{}
	for _, v := range values {
		value, err := p.NewValue(p.FloatType, v)
		if err != nil {
			t.Fatal(err)
		}
		if err = a.add(value); err != nil {
			t.Fatal(err)
		}
	}
	return a.floatTotal()
}

// exact computes the correctly rounded sum independently, through rational
// arithmetic, so the expansion is checked against real mathematics rather than
// against another implementation of itself.
func exact(values []float64) float64 {
	total := new(big.Rat)
	for _, v := range values {
		r := new(big.Rat)
		r.SetFloat64(v)
		total.Add(total, r)
	}
	f, _ := total.Float64()
	if f == 0 {
		return 0
	}
	return f
}

// Row order must not change a float sum. A running float64 accumulator rounds
// at every step and gets order-dependent answers; this rounds once.
func TestFloatSumsAreRoundedOnceAndOrderIndependent(t *testing.T) {
	cases := [][]float64{
		{9007199254740992, 1, -9007199254740992},
		{9007199254740992, 1},
		{9007199254740994, 1},
		{math.MaxFloat64, math.MaxFloat64, -math.MaxFloat64},
		{math.SmallestNonzeroFloat64, 0},
		{1e308, 1e308, -1e308},
		{0.1, 0.2, 0.3, -0.6},
		{1, 1e100, 1, -1e100},
	}
	for _, values := range cases {
		want := exact(values)
		for _, order := range permutations(values) {
			got := sumFloats(t, order)
			if got != want || math.Signbit(got) != math.Signbit(want) {
				t.Fatalf("order changed the sum: %v gave %v, exact is %v", order, got, want)
			}
		}
	}
	// The named ties-to-even cases, spelled out.
	if got := sumFloats(t, []float64{9007199254740992, 1}); got != 9007199254740992 {
		t.Fatal("ties-to-even rounded away from even", got)
	}
	if got := sumFloats(t, []float64{9007199254740994, 1}); got != 9007199254740996 {
		t.Fatal("ties-to-even rounded the wrong way", got)
	}
	// Equal opposite values are positive zero, not negative zero.
	zero := sumFloats(t, []float64{-1, 1})
	if zero != 0 || math.Signbit(zero) {
		t.Fatal("an exact zero was not positive zero")
	}
	// A subnormal survives.
	if got := sumFloats(t, []float64{math.SmallestNonzeroFloat64, 0}); got != math.SmallestNonzeroFloat64 {
		t.Fatal("a subnormal was lost", got)
	}
}

// Randomised against rational arithmetic, because a hand-picked set of cases
// is exactly where a summation bug hides.
func TestFloatSumsMatchExactArithmetic(t *testing.T) {
	random := rand.New(rand.NewSource(1))
	for trial := 0; trial < 300; trial++ {
		values := make([]float64, 1+random.Intn(12))
		for i := range values {
			values[i] = (random.Float64() - 0.5) * math.Pow(2, float64(random.Intn(120)-60))
		}
		if got, want := sumFloats(t, values), exact(values); got != want {
			t.Fatalf("expansion disagreed with exact arithmetic: %v vs %v for %v", got, want, values)
		}
	}
}

// Cancellation brings a total back into range, and the specification says that
// total is in range. A running int64 would have failed at the second value.
func TestIntegerSumsSurviveCancellation(t *testing.T) {
	sum := func(values ...int64) (json string, err error) {
		a := &accumulator{}
		for _, v := range values {
			value, e := p.NewValue(p.IntegerType, v)
			if e != nil {
				t.Fatal(e)
			}
			if e = a.add(value); e != nil {
				t.Fatal(e)
			}
		}
		entry, e := a.statistics(len(values), false, group{}, p.DevelopmentLimits())
		return string(entry.Sum), e
	}
	for _, order := range [][]int64{
		{math.MaxInt64, 1, -1}, {1, math.MaxInt64, -1}, {-1, 1, math.MaxInt64}, {1, -1, math.MaxInt64},
	} {
		got, err := sum(order...)
		if err != nil || got != "9223372036854775807" {
			t.Fatalf("cancellation failed for %v: %s %v", order, got, err)
		}
	}
	for _, order := range [][]int64{
		{math.MinInt64, -1, 1}, {-1, math.MinInt64, 1}, {1, -1, math.MinInt64},
	} {
		got, err := sum(order...)
		if err != nil || got != "-9223372036854775808" {
			t.Fatalf("negative cancellation failed for %v: %s %v", order, got, err)
		}
	}
	// The endpoints alone are fine; leaving the range is not.
	if got, err := sum(math.MaxInt64); err != nil || got != "9223372036854775807" {
		t.Fatal("the maximum alone was refused", got, err)
	}
	if got, err := sum(math.MinInt64); err != nil || got != "-9223372036854775808" {
		t.Fatal("the minimum alone was refused", got, err)
	}
	for _, overflowing := range [][]int64{
		{math.MaxInt64, 1}, {math.MaxInt64, math.MaxInt64}, {math.MinInt64, -1},
	} {
		_, err := sum(overflowing...)
		if p.ErrorCode(err) != "query_result_limit_exceeded" {
			t.Fatal("an overflowing total was not refused", overflowing, err)
		}
		var envelope *p.Error
		if !asError(err, &envelope) || envelope.Details["reason"] != "sum_overflow" {
			t.Fatal("the refusal did not name sum_overflow", overflowing)
		}
	}
}

func asError(err error, target **p.Error) bool {
	e, ok := err.(*p.Error)
	if ok {
		*target = e
	}
	return ok
}

func permutations(values []float64) [][]float64 {
	if len(values) <= 1 {
		return [][]float64{append([]float64(nil), values...)}
	}
	var result [][]float64
	for i := range values {
		rest := make([]float64, 0, len(values)-1)
		rest = append(rest, values[:i]...)
		rest = append(rest, values[i+1:]...)
		for _, tail := range permutations(rest) {
			result = append(result, append([]float64{values[i]}, tail...))
		}
	}
	return result
}

// Limits that never went through JSON get the group bound too. A composition
// building a Limits by hand, or one carried from a dataset made before the
// bound had a name, would otherwise refuse every grouped aggregation at a
// bound of zero — and refuse it with the message for a query that asked for
// too much, which is the opposite of what happened.
func TestAStoreBuiltWithoutTheGroupBoundStillGroups(t *testing.T) {
	limits := p.DevelopmentLimits()
	limits.MaxAggregateGroups = 0
	s, err := New(limits)
	if err != nil {
		t.Fatal(err)
	}
	if s.limits.MaxAggregateGroups != p.DefaultAggregateGroups {
		t.Fatal("a store built without the bound carries", s.limits.MaxAggregateGroups)
	}
}

// The case the type exists for: money summed exactly, with no float on either
// side of the wire.
//
// **Only one of these rows catches an accumulator that goes through a double,
// and it is not the famous one.** Seeding that defect leaves `0.1 + 0.1 + 0.1`
// answering `0.3`, because the total is rendered at the widest scale any
// addend carried and rounding to one fraction digit hides the drift. What
// fails is the eighteen-digit row, where the value itself is beyond binary64's
// significand and no rounding at the end can put it back.
//
// The others are kept for what they do prove — mixed scales, exact
// cancellation, and a total needing canonicalisation — but a reader should not
// take the first row as the guard. It is the demonstration, not the test.
func TestAFixedSumIsExact(t *testing.T) {
	for _, tc := range []struct {
		name     string
		addends  []string
		expected string
	}{
		{"tenths do not drift", []string{"0.1", "0.1", "0.1"}, "0.3"},
		{"mixed scales keep the widest", []string{"1.5", "0.25", "2"}, "3.75"},
		{"beyond binary64 significand", []string{"1.00000000000000001", "1.00000000000000001"}, "2.00000000000000002"},
		{"cancellation reaches zero exactly", []string{"0.1", "-0.1"}, "0"},
		{"a total that needs a canonical form", []string{"1.5", "0.5"}, "2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &accumulator{}
			for _, addend := range tc.addends {
				v, err := p.NewValue(p.FixedType, addend)
				if err != nil {
					t.Fatal(err)
				}
				if err = a.add(v); err != nil {
					t.Fatal(err)
				}
			}
			entry, err := a.statistics(len(tc.addends), false, group{}, p.DevelopmentLimits())
			if err != nil {
				t.Fatal(err)
			}
			// A string on the wire, not a number: a JSON number would be read
			// through a double by most clients and the exactness would die
			// there rather than here.
			if string(entry.Sum) != `"`+tc.expected+`"` {
				t.Fatalf("sum was %s, expected %q", entry.Sum, tc.expected)
			}
		})
	}
}

// A total wider than the declared bound is refused rather than truncated or
// returned in a spelling nobody can parse back.
func TestAFixedSumBeyondTheDigitBoundIsRefused(t *testing.T) {
	limits := p.DevelopmentLimits()
	limits.MaxFixedDigits = 4
	a := &accumulator{}
	for _, addend := range []string{"9999", "9999"} {
		v, _ := p.NewValue(p.FixedType, addend)
		if err := a.add(v); err != nil {
			t.Fatal(err)
		}
	}
	_, err := a.statistics(2, false, group{}, limits)
	if p.ErrorCode(err) != "query_result_limit_exceeded" {
		t.Fatal("a sum past the digit bound was not refused:", err)
	}
}
