package release

import "testing"

// A pseudo-version begins with something that looks like a real one, so the
// distinction has to be made on shape rather than on prefix.
func TestPseudoVersionsAreNotMistakenForReleases(t *testing.T) {
	for version, pseudo := range map[string]bool{
		"v0.1.0":                             false,
		"v1.2.3":                             false,
		"v0.1.0-rc.1":                        false,
		"v2.0.0-beta":                        false,
		"v0.0.0-20260919132919-d40db46d3bf7": true,
		"v0.0.0-20260919132919-d40db46d3bf7+dirty": true,
		"v0.1.1-0.20260919132919-d40db46d3bf7":     true,
	} {
		if pseudoVersion(version) != pseudo {
			t.Fatal("misjudged", version, "expected pseudo:", pseudo)
		}
	}
}

func TestATestBuildIsNeverAReleaseButAlwaysNamesItself(t *testing.T) {
	if Released() {
		t.Fatal("a build from a working tree reported itself as a release")
	}
	version := Version()
	if version == "" || version == "(devel)" {
		t.Fatal("the binary cannot say what it was built from:", version)
	}
}
