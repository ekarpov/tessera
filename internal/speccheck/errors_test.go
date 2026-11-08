// Package speccheck_test compares the specifications with the code that
// implements them, in the places where the two are mechanically comparable.
//
// It exists because of finding 22. Two requirements in one document
// contradicted each other for as long as nobody read them together, and what
// let that survive was a conformance case asserting the half both readings
// satisfied. A requirement nothing can fail is not yet a requirement, and
// prose that nothing compares to code drifts from it silently.
//
// This checks three bindings out of two hundred-odd requirements, and saying
// so is part of the point: a check that implied coverage would repeat the
// mistake it exists to catch. What it does have is exactness — each binding
// fails loudly and names what moved.
package speccheck_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	p "github.com/ekarpov/tessera/pkg/protocol"
)

// documented is a code's status as a specification states it. A code may carry
// more than one where a specification says so on purpose.
type documented map[string][]int

var codePattern = regexp.MustCompile("`([a-z_]+)`")

// errorTable reads the `HTTP | Code` table out of a specification. Both
// specifications write one; the protocol's carries several codes and their
// prose in one cell, the content protocol's one code per row with its meaning
// beside it. Only the second column is read, because the third holds
// capability names in backticks and would otherwise be mistaken for codes.
//
// Which means the code column must name codes and nothing else. A row that
// backticks a limit, a member or a route breaks this parse, and the failure
// reads as a missing error code rather than as a formatting problem — so the
// message is worth recognising: if a name this reports has never been an
// error code, the table put something else in the wrong column.
func errorTable(t *testing.T, path string) documented {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "specs", path))
	if err != nil {
		t.Fatal(err)
	}
	found := documented{}
	inside := false
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "| HTTP |") {
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
		if len(cells) < 2 {
			break
		}
		status, err := strconv.Atoi(strings.TrimSpace(cells[0]))
		if err != nil {
			continue // the --- separator row
		}
		for _, match := range codePattern.FindAllStringSubmatch(cells[1], -1) {
			found[match[1]] = append(found[match[1]], status)
		}
	}
	if len(found) == 0 {
		t.Fatal("no error table found in", path)
	}
	return found
}

// Every code a specification names carries the status the code gives it, and
// every code the code defines is named by a specification. Both directions,
// because each catches something the other cannot: a status that moved, and a
// code that was added or removed on one side only.
func TestTheErrorTablesAgreeWithTheCode(t *testing.T) {
	tables := map[string]documented{
		"tessera-protocol-v1.md": errorTable(t, "tessera-protocol-v1.md"),
		"tessera-content-v1.md":  errorTable(t, "tessera-content-v1.md"),
	}
	together := map[string]bool{}
	for spec, table := range tables {
		for code, statuses := range table {
			together[code] = true
			actual := p.ErrorStatus(code)
			carried := false
			for _, status := range statuses {
				if status == actual {
					carried = true
				}
			}
			if !carried {
				t.Errorf("%s documents %s as %v; the code answers %d", spec, code, statuses, actual)
			}
			// More than one status is allowed where a specification means it —
			// query_result_limit_exceeded is documented at 413 and 422,
			// because a request too large to accept is reported at the
			// transport boundary. Anything else with two is a table that has
			// been edited in one row and not the other.
			if len(statuses) > 1 && code != "query_result_limit_exceeded" {
				t.Errorf("%s documents %s under more than one status: %v", spec, code, statuses)
			}
		}
	}
	for _, code := range p.ErrorCodes() {
		if !together[code] {
			t.Errorf("the code defines %s (%d) and no specification names it", code, p.ErrorStatus(code))
		}
	}
	for code := range together {
		if p.ErrorStatus(code) == 500 && code != "internal_error" {
			t.Errorf("a specification names %s and the code does not define it", code)
		}
	}
}

// A code a specification uses in its prose is one its own table has to list.
//
// The union of the two tables agreeing with the code is not enough: a code can
// be documented in one specification, used by the surface the other describes,
// and missing from that one's table — which is a reader of that document being
// told a code exists that their protocol never mentions, or worse, being told
// the set is smaller than it is.
//
// This is deliberately per document rather than across both, because the
// question is what a reader of one document is told.
func TestEachSpecificationTableNamesTheCodesItsProseUses(t *testing.T) {
	known := map[string]bool{}
	for _, code := range p.ErrorCodes() {
		known[code] = true
	}
	for _, spec := range []string{"tessera-protocol-v1.md", "tessera-content-v1.md"} {
		table := errorTable(t, spec)
		raw, err := os.ReadFile(filepath.Join("..", "..", "specs", spec))
		if err != nil {
			t.Fatal(err)
		}
		reported := map[string]bool{}
		for _, match := range codePattern.FindAllStringSubmatch(string(raw), -1) {
			code := match[1]
			if !known[code] || len(table[code]) > 0 || reported[code] {
				continue
			}
			reported[code] = true
			t.Errorf("%s uses %s in its prose and its own table does not list it", spec, code)
		}
	}
}
