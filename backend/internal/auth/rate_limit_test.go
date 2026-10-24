package auth

import "testing"

func TestLoginClientAndBucketHash(t *testing.T) {
	for input, want := range map[string]string{
		"192.0.2.10:1234":       "192.0.2.10",
		"[2001:db8::1]:443":     "2001:db8::1",
		"[::ffff:192.0.2.1]:80": "192.0.2.1",
	} {
		got, err := loginClient(input)
		if err != nil || got != want {
			t.Fatalf("loginClient(%q)=%q,%v want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"", "192.0.2.1", "host.invalid:80", "192.0.2.1:not-port"} {
		if _, err := loginClient(input); err == nil {
			t.Fatalf("loginClient accepted %q", input)
		}
	}
	var key [32]byte
	key[0] = 1
	username := rateLimitHash(key, "username", "admin")
	if username == rateLimitHash(key, "client", "admin") || username == rateLimitHash(key, "username", "Admin") {
		t.Fatal("rate-limit hashes did not separate key type or normalized value")
	}
}
