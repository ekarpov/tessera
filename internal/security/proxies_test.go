package security_test

import (
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

// register asks to register a client as if from peer, carrying the given
// X-Forwarded-For, and reports whether the rate limit let it through.
func (x *tenancy) registerFrom(t *testing.T, peer, forwarded string) bool {
	t.Helper()
	r := httptest.NewRequest("POST", "/oauth/register",
		strings.NewReader(`{"client_name":"Probe","redirect_uris":["https://probe.example/callback"]}`))
	r.RemoteAddr = peer + ":40000"
	if forwarded != "" {
		r.Header.Set("X-Forwarded-For", forwarded)
	}
	w := httptest.NewRecorder()
	x.runtime.Handler.ServeHTTP(w, r)
	if w.Code != 201 && w.Code != 429 {
		t.Fatal("unexpected registration answer:", w.Code, w.Body.String())
	}
	return w.Code == 201
}

// Behind a reverse proxy every request arrives from the proxy. Counting per
// connection made the per-address limits one budget for the whole server,
// which anybody could spend. A trusted proxy's X-Forwarded-For names the
// caller; nobody else's is believed.
func TestPerAddressLimitsCountTheCallerBehindATrustedProxy(t *testing.T) {
	x := tenants(t)
	const proxy = "10.0.0.2"
	spend := func(peer, forwarded string) {
		for i := 0; i < 20; i++ {
			if !x.registerFrom(t, peer, forwarded) {
				t.Fatal("refused before its own budget was spent:", forwarded, i)
			}
		}
		if x.registerFrom(t, peer, forwarded) {
			t.Fatal("the per-address limit did not hold for", forwarded)
		}
	}

	// Not trusted: the header is ignored, so naming another address does not
	// buy a fresh budget.
	spend(proxy, "198.51.100.1")
	if x.registerFrom(t, proxy, "198.51.100.2") {
		t.Fatal("a forged X-Forwarded-For from an untrusted peer was believed")
	}

	x.runtime.Authority.SetTrustedProxies([]netip.Prefix{netip.MustParsePrefix("10.0.0.0/24")})
	spend(proxy, "198.51.100.10")
	// Another caller behind the same proxy has a budget of their own.
	if !x.registerFrom(t, proxy, "198.51.100.11") {
		t.Fatal("one caller behind the proxy spent another's budget")
	}
	// Whatever the caller wrote to the left of the proxy's entry is theirs,
	// and not an address to be counted against.
	if x.registerFrom(t, proxy, "203.0.113.99, 198.51.100.10") {
		t.Fatal("an entry the caller forged to the left of the proxy's was believed")
	}
	// A chain of trusted proxies is walked to the first untrusted hop.
	if x.registerFrom(t, proxy, "198.51.100.10, 10.0.0.7") {
		t.Fatal("a trusted hop was taken for the caller")
	}
	// A request that arrives from a proxy with no header counts as the proxy.
	// (This one's own budget went on the untrusted requests above.)
	if !x.registerFrom(t, "10.0.0.3", "") {
		t.Fatal("the proxy's own budget was spent by its callers")
	}
}

// There is no server-wide budget left for a few addresses to spend: each
// caller is limited, and the next caller is not (finding 31). The processors
// are protected by bounding concurrent password derivations instead.
func TestAFewAddressesCannotSpendEverybodysBudget(t *testing.T) {
	x := tenants(t)
	for i := 1; i <= 5; i++ {
		attacker := "198.51.100." + string(rune('0'+i))
		for x.registerFrom(t, attacker, "") {
		}
	}
	if !x.registerFrom(t, "192.0.2.50", "") {
		t.Fatal("five addresses spending their own budgets closed registration for everybody")
	}
}

// An IPv6 subscriber is given a /64, so its limits are counted per /64:
// counted per address, one caller could use a fresh address every request.
func TestAnIPv6CallerIsCountedByItsNetwork(t *testing.T) {
	x := tenants(t)
	for i := 0; i < 20; i++ {
		if !x.registerFrom(t, "[2001:db8:1:2::"+string(rune('a'+i%6))+"]", "") {
			t.Fatal("refused before the network's budget was spent", i)
		}
	}
	if x.registerFrom(t, "[2001:db8:1:2:ffff::1]", "") {
		t.Fatal("another address in the same /64 got a fresh budget")
	}
	if !x.registerFrom(t, "[2001:db8:1:3::1]", "") {
		t.Fatal("a different /64 was counted with the first")
	}
}
