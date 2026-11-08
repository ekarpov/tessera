package standalone

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBackupSettingsBootstrapStrictDecoding(t *testing.T) {
	const valid = `{"version":1,"deployment":"01234567890123456789012345678901","every_hours":0,"keep":2,"revision":"deployment.scope.revision","exported_at":"2026-10-17T10:20:30.123Z"}`
	if got, err := decodeBackupSettingsBootstrap([]byte(valid)); err != nil || got.EveryHours != 0 || got.ExportedAt.Nanosecond() != 123000000 {
		t.Fatal("valid scalar export rejected", err)
	}
	cases := map[string]string{
		"unknown":            strings.Replace(valid, `"version":1`, `"extra":1,"version":1`, 1),
		"duplicate":          strings.Replace(valid, `"keep":2`, `"keep":2,"keep":3`, 1),
		"timestamp":          strings.Replace(valid, "2026-10-17T10:20:30.123Z", "yesterday", 1),
		"timestamp object":   strings.Replace(valid, `"2026-10-17T10:20:30.123Z"`, `{}`, 1),
		"fractional integer": strings.Replace(valid, `"keep":2`, `"keep":2.5`, 1),
		"trailing value":     valid + `{}`,
		"oversize":           valid + strings.Repeat(" ", 16384),
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(valid), &fields); err != nil {
		t.Fatal(err)
	}
	for field, value := range fields {
		delete(fields, field)
		raw, _ := json.Marshal(fields)
		cases["missing "+field] = string(raw)
		fields[field] = json.RawMessage("null")
		raw, _ = json.Marshal(fields)
		cases["null "+field] = string(raw)
		fields[field] = value
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeBackupSettingsBootstrap([]byte(raw)); err == nil {
				t.Fatal("invalid bootstrap accepted")
			}
		})
	}
}
