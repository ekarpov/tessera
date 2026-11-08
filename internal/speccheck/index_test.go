package speccheck_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func root(parts ...string) string {
	return filepath.Join(append([]string{"..", ".."}, parts...)...)
}

func read(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

var findingHeading = regexp.MustCompile(`(?m)^### Finding (\d+):`)
var candidateHeading = regexp.MustCompile(`(?m)^#{2,3} (\d+)\. `)

// The index is a promise that a consumer can find an answer in one lookup
// rather than by reading two documents organised by argument. A finding added
// without an entry breaks that promise quietly: the index still answers
// confidently, and it is now answering about a smaller contract than exists.
//
// This checks that a question is listed, not that the answer beside it is
// still right. Nothing mechanical can check the second, and claiming
// otherwise would be the failure this whole package exists to catch.
func TestEveryFindingAndCandidateIsInTheIndex(t *testing.T) {
	index := read(t, root("docs", "asked-and-answered.md"))

	for _, match := range findingHeading.FindAllStringSubmatch(read(t, root("docs", "tessera-spec-review.md")), -1) {
		if !regexp.MustCompile(`\bFinding ` + match[1] + `\b`).MatchString(index) {
			t.Errorf("finding %s has no entry in docs/asked-and-answered.md", match[1])
		}
	}

	// Two candidate numbers are deliberately not entries of their own. 10 was
	// withdrawn and its number retired rather than reused, so that the record
	// of what it was does not read wrong; 11 and 12 arrived from the
	// implementation and are findings 16 and 15, indexed under those.
	elsewhere := map[string]string{"10": "withdrawn, number retired", "11": "finding 16", "12": "finding 15"}
	for _, match := range candidateHeading.FindAllStringSubmatch(read(t, root("docs", "extension-candidates.md")), -1) {
		if _, known := elsewhere[match[1]]; known {
			continue
		}
		if !regexp.MustCompile(`\bCandidate ` + match[1] + `\b`).MatchString(index) {
			t.Errorf("extension candidate %s has no entry in docs/asked-and-answered.md", match[1])
		}
	}
}

var markdownLink = regexp.MustCompile(`\]\(([^)]+)\)`)

// A link to a document that moved is the same defect as a stale rule, and it
// happened here: a renamed guide left a Go comment pointing at a file that had
// not existed for weeks, under a name this repository had deliberately stopped
// using. Nobody reading either file could see it.
//
// Every relative link in every Markdown file, both directions of the repository
// — the specifications link into the documents and back.
func TestEveryDocumentLinkResolves(t *testing.T) {
	for _, dir := range []string{".", "docs", "specs", "deploy"} {
		entries, err := os.ReadDir(root(dir))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".md") {
				continue
			}
			source := filepath.Join(root(dir), entry.Name())
			for _, match := range markdownLink.FindAllStringSubmatch(read(t, source), -1) {
				target := match[1]
				if strings.HasPrefix(target, "http") || strings.HasPrefix(target, "#") || strings.HasPrefix(target, "mailto:") {
					continue
				}
				path, fragment, _ := strings.Cut(target, "#")
				if path == "" {
					continue
				}
				resolved := filepath.Join(filepath.Dir(source), path)
				if _, err := os.Stat(resolved); err != nil {
					t.Errorf("%s/%s links to %s, which does not exist", dir, entry.Name(), path)
					continue
				}
				// A fragment that names no heading lands the reader at the top
				// of the document, which for a six-hundred-line review is the
				// same as not linking at all — and it cannot be seen by reading
				// either file. Only Markdown targets carry headings to check.
				if fragment == "" || !strings.HasSuffix(path, ".md") {
					continue
				}
				if !headings(t, resolved)[fragment] {
					t.Errorf("%s/%s links to %s#%s, and that document has no such heading", dir, entry.Name(), path, fragment)
				}
			}
		}
	}
}

var heading = regexp.MustCompile(`(?m)^#{1,6} +(.+?)\s*$`)
var notInAnchor = regexp.MustCompile(`[^a-z0-9 -]`)

// headings returns the anchors a Markdown document offers, by the rule GitHub
// applies: lower case, punctuation dropped, spaces to hyphens.
func headings(t *testing.T, path string) map[string]bool {
	t.Helper()
	found := map[string]bool{}
	for _, match := range heading.FindAllStringSubmatch(read(t, path), -1) {
		found[strings.ReplaceAll(notInAnchor.ReplaceAllString(strings.ToLower(match[1]), ""), " ", "-")] = true
	}
	return found
}
