package conformance

import (
	"encoding/json"
	"testing"
)

// The one half of the plaintext rule that is mechanically checkable against a
// server: a conformant deployment never advertises an authorization server a
// conformant client would have to refuse. The client-side half — refuse it,
// before fetching rather than while validating what was fetched — is a client
// obligation this suite cannot reach.
//
// The spellings below are the ones a consumer's implementation actually
// produced rather than ones invented here.
//
// **Two of them prove nothing in Go and are kept anyway.** `url.Parse`
// lowercases a scheme, so `HTTPS://` and `HTTP://` arrive already normalised
// and the case-insensitive scheme comparison is never what decides them —
// seeding an exact comparison changes no result here. They stay because this
// suite is portable and the hazard is real elsewhere: a consumer implementing
// the same rule in Swift found that `URL` does not normalise, so `HTTP://` has
// a scheme equal to neither `"http"` nor `"https"` and a guard written as
// *refuse http* admits a plaintext URL outright.
//
// The host cases do prove something here, because Go does **not** lowercase a
// host: seeding an exact comparison fails `loopback name cased`. And the IPv6
// and `127.0.0.0/8` cases fail against literal matching, which is why this
// parses an address rather than comparing spellings.
func TestAdvertisedAuthorizationServersMustBeReachableSafely(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []string
		refused bool
	}{
		{"https", []string{"https://tessera.example.org"}, false},
		{"uppercase https", []string{"HTTPS://tessera.example.org"}, false},
		{"loopback http", []string{"http://127.0.0.1:8081"}, false},
		{"loopback name", []string{"http://localhost:8081"}, false},
		{"loopback name cased", []string{"http://LocalHost:8081"}, false},
		{"loopback ipv6", []string{"http://[::1]:8081"}, false},
		{"loopback ipv6 written out", []string{"http://[0:0:0:0:0:0:0:1]:8081"}, false},
		{"loopback ipv4 mapped", []string{"http://[::ffff:127.0.0.1]:8081"}, false},
		{"other loopback ipv4", []string{"http://127.0.0.2:8081"}, false},
		{"empty", []string{}, false},

		{"private address", []string{"http://192.168.1.10"}, true},
		{"private name", []string{"http://storage.local"}, true},
		{"public host", []string{"http://tessera.example.org"}, true},
		// The spelling that defeats an exact comparison against "http": its
		// scheme equals neither "http" nor "https", so a guard written as
		// "refuse http" admits it.
		{"uppercase scheme", []string{"HTTP://192.168.1.10"}, true},
		// One good entry does not excuse a bad one. A client takes the first
		// it can use, so an unusable entry in any position is an entry the
		// deployment should not have published.
		{"one of each", []string{"https://tessera.example.org", "http://192.168.1.10"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.entries)
			if err != nil {
				t.Fatal(err)
			}
			err = checkAuthorizationServersAreReachableSafely(raw)
			if tc.refused && err == nil {
				t.Fatal("accepted an authorization server a conformant client must refuse")
			}
			if !tc.refused && err != nil {
				t.Fatal("refused a safely reachable authorization server:", err)
			}
		})
	}
}
