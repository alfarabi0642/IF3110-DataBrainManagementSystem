package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestTargetSettings(t *testing.T) {
	t.Setenv("TARGET_ALLOWED_HOSTS", " LocalHost.,127.0.0.1,2001:0db8::1,::ffff:127.0.0.1 ")
	t.Setenv("TARGET_CA_FILE", "operator-ca.pem")
	got, err := LoadTargetSettings("production")
	if err != nil || got.AppEnv != "production" || got.CAFile != "operator-ca.pem" ||
		!reflect.DeepEqual(got.AllowedHosts, []string{"localhost", "127.0.0.1", "2001:db8::1", "127.0.0.1"}) {
		t.Fatal("target settings did not normalize exact allowed hosts")
	}
	for _, raw := range []string{"", "localhost,", "*.example.com", "http://SENSITIVE@localhost", "localhost:5432", "/tmp/socket", "[::1]", "fe80::1%eth0", "a..b", "-bad", "bad-", strings.Repeat("a", 64) + ".com"} {
		t.Setenv("TARGET_ALLOWED_HOSTS", raw)
		if _, err := LoadTargetSettings("test"); err == nil || strings.Contains(err.Error(), "SENSITIVE") {
			t.Fatal("invalid allowed host accepted or disclosed")
		}
	}
	t.Setenv("TARGET_ALLOWED_HOSTS", "localhost")
	if _, err := LoadTargetSettings("staging"); err == nil {
		t.Fatal("unknown environment accepted")
	}
	if _, err := NormalizeTargetHost("host\x00name"); err == nil {
		t.Fatal("NUL in host accepted")
	}
}
