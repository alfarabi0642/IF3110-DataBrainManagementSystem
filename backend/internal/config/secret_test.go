package config

import (
	"bytes"
	"encoding/base64"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestLoadSecretKey(t *testing.T) {
	want := bytes.Repeat([]byte{0xfb}, 32)
	t.Setenv("APP_SECRET_KEY", base64.StdEncoding.EncodeToString(want))
	got, err := LoadSecretKey()
	if err != nil || !bytes.Equal(got, want) {
		t.Fatal("valid root key rejected or changed")
	}
}

func TestLoadSecretKeyRejectsInvalid(t *testing.T) {
	valid := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xfb}, 32))
	tests := []struct{ name, raw string }{
		{"empty", ""},
		{"placeholder", "CHANGE_ME_BASE64_OF_32_RANDOM_BYTES"},
		{"invalid encoding", "SENSITIVE!"},
		{"unpadded", strings.TrimSuffix(valid, "=")},
		{"URL alphabet", base64.URLEncoding.EncodeToString(bytes.Repeat([]byte{0xfb}, 32))},
		{"leading whitespace", " " + valid},
		{"newline", valid[:20] + "\r\n" + valid[20:]},
		{"nonzero padding bits", valid[:42] + "t="},
	}
	for _, size := range []int{0, 16, 24, 31, 33, 64} {
		tests = append(tests, struct{ name, raw string }{
			name: "invalid decoded length " + strconv.Itoa(size),
			raw:  base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xfb}, size)),
		})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("APP_SECRET_KEY", tt.raw)
			key, err := LoadSecretKey()
			if key != nil || err == nil || err.Error() != "APP_SECRET_KEY must be standard padded base64 of 32 random bytes" {
				t.Fatal("invalid key accepted or configuration error disclosed input")
			}
		})
	}
}

func TestMissingSecretKeyHasNoDefault(t *testing.T) {
	t.Setenv("APP_SECRET_KEY", "")
	if err := os.Unsetenv("APP_SECRET_KEY"); err != nil {
		t.Fatal("could not unset test environment")
	}
	if key, err := LoadSecretKey(); err == nil || key != nil {
		t.Fatal("missing root key must fail without a default")
	}
	// Konfigurasi dasar HTTP/platform dan migration dapat dibaca tanpa kunci; auth dan target
	// memerlukannya saat dipasang.
	if _, err := load(func(string) (string, bool) { return "", false }); err != nil {
		t.Fatal("HTTP config unexpectedly requires a secret key")
	}
}
