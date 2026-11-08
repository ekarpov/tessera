package conformance

import (
	"encoding/json"
	"fmt"
	"io"

	p "github.com/ekarpov/tessera/pkg/protocol"
)

// CheckVersionMetadata checks the version fields of a discovery JSON document.
// Unknown response fields are ignored. Dates already in the past remain valid:
// a planned sunset does not prove that retirement conditions have been met.
func CheckVersionMetadata(document []byte) error {
	var fields map[string]json.RawMessage
	if json.Unmarshal(document, &fields) != nil || fields == nil {
		return fmt.Errorf("invalid discovery document")
	}
	return checkVersionMetadata(fields)
}

func checkVersionMetadata(fields map[string]json.RawMessage) error {
	var versions []string
	if json.Unmarshal(fields["protocol_versions"], &versions) != nil {
		return fmt.Errorf("invalid protocol_versions")
	}
	if err := p.ValidateProtocolVersions(versions); err != nil {
		return err
	}
	raw, present := fields["deprecated_versions"]
	if !present {
		return nil
	}
	var entries []json.RawMessage
	if json.Unmarshal(raw, &entries) != nil || entries == nil {
		return fmt.Errorf("deprecated_versions must be an array")
	}
	deprecated := make([]p.DeprecatedVersion, 0, len(entries))
	for _, entry := range entries {
		// Decode required strings separately: null must not become a zero value.
		var fields map[string]json.RawMessage
		if json.Unmarshal(entry, &fields) != nil || fields == nil {
			return fmt.Errorf("invalid deprecated_versions entry")
		}
		var d p.DeprecatedVersion
		if json.Unmarshal(fields["version"], &d.Version) != nil || json.Unmarshal(fields["sunset"], &d.Sunset) != nil {
			return fmt.Errorf("invalid deprecated_versions entry")
		}
		deprecated = append(deprecated, d)
	}
	return p.ValidateDeprecatedVersions(versions, deprecated)
}

// CheckVersionSelection runs portable version-selection vectors against a client
// implementation. Supply specs/vectors/version-selection.json from the release
// being qualified. Error cases must return an error and no selected version.
func CheckVersionSelection(selectVersion func([]string, []string) (string, error), vectors io.Reader) error {
	var cases []struct {
		Name       string   `json:"name"`
		Advertised []string `json:"advertised"`
		Supported  []string `json:"supported"`
		Selected   string   `json:"selected"`
		Refused    bool     `json:"refused"`
	}
	if selectVersion == nil || vectors == nil {
		return fmt.Errorf("version selection needs an implementation and vectors")
	}
	if json.NewDecoder(vectors).Decode(&cases) != nil || len(cases) == 0 {
		return fmt.Errorf("invalid version selection vectors")
	}
	for i, c := range cases {
		got, err := selectVersion(c.Advertised, c.Supported)
		if (err != nil) != c.Refused || got != c.Selected {
			return fmt.Errorf("version selection case %d failed", i)
		}
	}
	return nil
}
