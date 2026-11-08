package protocol

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// Every limit can be named in an override, whatever the base it merges onto
// happens to encode. max_stored_bytes is absent from the development limits,
// and a configuration setting a quota was refused as naming an unknown limit.
func TestEveryLimitCanBeOverridden(t *testing.T) {
	ty := reflect.TypeOf(Limits{})
	for i := 0; i < ty.NumField(); i++ {
		name, _, _ := strings.Cut(ty.Field(i).Tag.Get("json"), ",")
		value := 1 << 20
		if name == "idempotency_retention_seconds" {
			value = MinimumIdempotencyRetentionSeconds * 2
		}
		raw, _ := json.Marshal(map[string]int{name: value})
		merged, err := MergeLimits(DevelopmentLimits(), raw)
		if err != nil {
			t.Errorf("%s could not be overridden: %v", name, err)
			continue
		}
		if err = merged.Validate(); err != nil {
			t.Errorf("%s=%d merged into invalid limits: %v", name, value, err)
		}
		if got := reflect.ValueOf(merged).Field(i).Int(); got != int64(value) {
			t.Errorf("%s merged as %d, want %d", name, got, value)
		}
	}
	if _, err := MergeLimits(DevelopmentLimits(), json.RawMessage(`{"max_unknown_bytes":1}`)); err == nil {
		t.Fatal("an unknown limit was accepted")
	}
}
