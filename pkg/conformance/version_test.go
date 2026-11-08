package conformance_test

import (
	"encoding/json"
	"github.com/ekarpov/tessera/pkg/conformance"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"os"
	"strings"
	"testing"
)

func TestVersionDiscoveryVectors(t *testing.T) {
	raw, err := os.ReadFile("../../specs/vectors/version-discovery.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name     string
		Document json.RawMessage
		Valid    bool
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			if err := conformance.CheckVersionMetadata(c.Document); (err == nil) != c.Valid {
				t.Fatal("unexpected validation result", err)
			}
		})
	}
}

func TestVersionSelectionVectors(t *testing.T) {
	raw, err := os.ReadFile("../../specs/vectors/version-selection.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := conformance.CheckVersionSelection(p.SelectProtocolVersion, strings.NewReader(string(raw))); err != nil {
		t.Fatal(err)
	}
	// A compiling implementation defect: choose the first shared array member.
	first := func(a, b []string) (string, error) {
		for _, x := range a {
			for _, y := range b {
				if x == y {
					return x, nil
				}
			}
		}
		return "", p.ErrNoCommonProtocolVersion
	}
	if err := conformance.CheckVersionSelection(first, strings.NewReader(string(raw))); err == nil {
		t.Fatal("missed seeded array-order selection")
	}
}
