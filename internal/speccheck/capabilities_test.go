package speccheck_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	p "github.com/ekarpov/tessera/pkg/protocol"
)

func protocolSpec(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "specs", "tessera-protocol-v1.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// namedTable reads the first column of the table whose header row begins with
// header, taking the one name in backticks from each row. Both capability
// tables are written this way.
func namedTable(t *testing.T, document, header string) []string {
	t.Helper()
	names := []string{}
	inside := false
	for _, line := range strings.Split(document, "\n") {
		if strings.HasPrefix(line, header) {
			inside = true
			continue
		}
		if !inside {
			continue
		}
		if !strings.HasPrefix(line, "|") {
			break
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for _, match := range codePattern.FindAllStringSubmatch(cells[0], -1) {
			names = append(names, match[1])
		}
	}
	if len(names) == 0 {
		t.Fatalf("no table found under %q", header)
	}
	sort.Strings(names)
	return names
}

func sorted(names []string) []string {
	out := append([]string{}, names...)
	sort.Strings(out)
	return out
}

// The capability table is the whole list of things a server may decline, and
// a name on one side only is a real failure in either direction: a name the
// code advertises and the table omits is a feature no client knows to ask
// for, and a name the table carries and the code cannot advertise is a
// promise of a choice that does not exist.
//
// This is the binding the administration page needed and did not have. It
// carried its own copy of the list, the three retired names stayed in that
// copy after they became required operations, and the overview page reported
// them to an operator as withheld — three operations the protocol no longer
// lets a deployment decline, displayed as decisions this deployment had made.
func TestTheCapabilityTableIsTheCapabilitiesTheCodeCanAdvertise(t *testing.T) {
	documented := namedTable(t, protocolSpec(t), "| Capability | Meaning |")
	declared := sorted(p.DatasetCapabilities())
	if strings.Join(documented, " ") != strings.Join(declared, " ") {
		t.Errorf("the protocol's capability table names %v; the code can advertise %v", documented, declared)
	}
}

// A retired name is not a capability with a false value, it is a name that no
// longer answers the question. The specification says a server MUST NOT
// advertise the three, so the list the code can advertise must not contain
// them — and the sentence that retires them must name the same three, because
// a fourth retired in code and not in prose leaves a reader believing a
// toggle exists.
var retirement = regexp.MustCompile("(?s)\\*\\*([^*]*?)were on this list and are now required")

func TestTheRetiredNamesAreRetiredInBothPlaces(t *testing.T) {
	for _, name := range p.DatasetCapabilities() {
		for _, retired := range p.RetiredReadCapabilities {
			if name == retired {
				t.Errorf("%s is retired and the code still offers it as a capability", name)
			}
		}
	}
	match := retirement.FindStringSubmatch(protocolSpec(t))
	if match == nil {
		t.Fatal("the protocol no longer says which capabilities were retired")
	}
	named := []string{}
	for _, found := range codePattern.FindAllStringSubmatch(match[1], -1) {
		named = append(named, found[1])
	}
	if strings.Join(sorted(named), " ") != strings.Join(sorted(p.RetiredReadCapabilities), " ") {
		t.Errorf("the protocol retires %v; the code retires %v", sorted(named), sorted(p.RetiredReadCapabilities))
	}
}

// `server_capabilities` is a second, smaller vocabulary, and the prose states
// its whole membership rather than tabulating it. Bound the same way: a
// member added to one side alone is a client told the set is a different size
// than it is.
var serverMembers = regexp.MustCompile("The members defined in v1 are ((?:`[a-z_]+`(?:, | and )?)+)\\.")
var memberName = regexp.MustCompile("`([a-z_]+)`")

func TestTheServerCapabilityVocabularyMatchesTheCode(t *testing.T) {
	match := serverMembers.FindStringSubmatch(protocolSpec(t))
	if match == nil {
		t.Fatal("the protocol no longer states the membership of server_capabilities")
	}
	stated := []string{}
	for _, name := range memberName.FindAllStringSubmatch(match[1], -1) {
		stated = append(stated, name[1])
	}
	if strings.Join(stated, ",") != strings.Join(p.ServerCapabilities, ",") {
		t.Errorf("the protocol defines server_capabilities as %v; the code declares %v", stated, p.ServerCapabilities)
	}
}
