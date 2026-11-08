package speccheck_test

import (
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var exampleURL = regexp.MustCompile(`https?://[^\s"'` + "`" + `)<>]+`)

// Every URL in a specification example is `https`, or its host is loopback.
//
// This exists because of how the rule it checks was nearly lost. The content
// protocol issues a URI it calls a credential in its own words, constrains its
// lifetime, its scope, whether it may be logged and what revocation does to
// it — and said nothing about its scheme. **The only statement that it must be
// encrypted was that the examples happened to use `https`.** A reader copies
// the example or does not, and nothing fails either way.
//
// So the examples are bound. A new one carrying a plaintext URL fails here,
// which is the difference between a pattern and a requirement.
//
// Fenced blocks only, deliberately. Prose carries plaintext URLs on purpose —
// `http://192.168.1.10` appears in both specifications as the thing a client
// must refuse — and a check that could not tell a counter-example from an
// example would have to be switched off the first time somebody wrote one.
func TestSpecificationExamplesCarryNoPlaintextURLs(t *testing.T) {
	entries, err := os.ReadDir(root("specs"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		document := read(t, filepath.Join(root("specs"), entry.Name()))
		for index, block := range strings.Split(document, "```") {
			if index%2 == 0 {
				continue // outside a fence
			}
			for _, found := range exampleURL.FindAllString(block, -1) {
				parsed, e := url.Parse(found)
				if e != nil {
					continue
				}
				if strings.EqualFold(parsed.Scheme, "https") || loopback(parsed.Hostname()) {
					continue
				}
				t.Errorf("specs/%s shows %s in an example, which a conformant client must refuse", entry.Name(), found)
			}
		}
	}
}

// loopback answers for the address rather than its spelling, matching the rule
// the specifications state.
func loopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}
