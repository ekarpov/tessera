package standalone

import "testing"

func TestTrustedProxiesAreAddressesOrRanges(t *testing.T) {
	proxies, err := ParseProxies([]string{"10.0.0.0/8", "192.0.2.7", "::ffff:192.0.2.8", "fd00::/8"})
	if err != nil || len(proxies) != 4 || proxies[1].Bits() != 32 || proxies[2].String() != "192.0.2.8/32" {
		t.Fatal("valid proxies were misread:", proxies, err)
	}
	for _, bad := range []string{"caddy", "10.0.0.0/33", ""} {
		if _, err = ParseProxies([]string{bad}); err == nil {
			t.Fatal("a proxy that is not an address or range was accepted:", bad)
		}
	}
}
