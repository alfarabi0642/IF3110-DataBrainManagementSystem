package auth

import (
	"net/netip"
	"testing"
)

func TestLoginClientAndBucketHash(t *testing.T) {
	for input, want := range map[string]string{
		"192.0.2.10:1234":       "192.0.2.10",
		"[2001:db8::1]:443":     "2001:db8::1",
		"[::ffff:192.0.2.1]:80": "192.0.2.1",
	} {
		got, err := loginClient(input, "", nil)
		if err != nil || got != want {
			t.Fatalf("loginClient(%q)=%q,%v want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"", "192.0.2.1", "host.invalid:80", "192.0.2.1:not-port"} {
		if _, err := loginClient(input, "", nil); err == nil {
			t.Fatalf("loginClient accepted %q", input)
		}
	}
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("192.0.2.0/24")}
	for _, test := range []struct {
		remote, forwarded, want string
	}{
		{"198.51.100.1:443", "203.0.113.9", "198.51.100.1"},
		{"10.0.0.2:443", "203.0.113.9, 192.0.2.7", "203.0.113.9"},
		{"10.0.0.2:443", "192.0.2.7, 10.0.0.3", "192.0.2.7"},
	} {
		got, err := loginClient(test.remote, test.forwarded, trusted)
		if err != nil || got != test.want {
			t.Fatalf("loginClient(%q,%q)=%q,%v want %q", test.remote, test.forwarded, got, err, test.want)
		}
	}
	if _, err := loginClient("10.0.0.2:443", "not-an-ip", trusted); err == nil {
		t.Fatal("invalid forwarding chain accepted from a trusted proxy")
	}
	var key [32]byte
	key[0] = 1
	username := rateLimitHash(key, "username", "admin")
	if username == rateLimitHash(key, "client", "admin") || username == rateLimitHash(key, "username", "Admin") {
		t.Fatal("rate-limit hashes did not separate key type or normalized value")
	}
}
