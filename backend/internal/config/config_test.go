package config

import (
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	defaults := Config{HTTPAddr: "127.0.0.1:8080", AppEnv: "development", AppOrigin: "http://localhost:8080"}
	tests := []struct {
		name string
		env  map[string]string
		want Config
	}{
		{name: "defaults", want: defaults},
		{name: "development IPv6", env: map[string]string{"HTTP_ADDR": "[::1]:9000", "APP_ORIGIN": "http://[::1]:9000"}, want: Config{HTTPAddr: "[::1]:9000", AppEnv: "development", AppOrigin: "http://[::1]:9000"}},
		{name: "test IPv4 loopback", env: map[string]string{"APP_ENV": "test", "APP_ORIGIN": "http://127.0.0.1:8080"}, want: Config{HTTPAddr: defaults.HTTPAddr, AppEnv: "test", AppOrigin: "http://127.0.0.1:8080"}},
		{name: "production HTTPS", env: map[string]string{"APP_ENV": "production", "APP_ORIGIN": "https://databrain.example", "HTTP_ADDR": ":8080"}, want: Config{HTTPAddr: ":8080", AppEnv: "production", AppOrigin: "https://databrain.example"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := load(func(key string) (string, bool) { v, ok := tt.env[key]; return v, ok })
			if err != nil || got != tt.want {
				t.Fatalf("load() = %+v, %v; want %+v", got, err, tt.want)
			}
		})
	}
}

func TestLoadRejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name  string
		env   map[string]string
		field string
	}{
		{"unknown environment", map[string]string{"APP_ENV": "staging"}, "APP_ENV"},
		{"empty environment", map[string]string{"APP_ENV": ""}, "APP_ENV"},
		{"empty address", map[string]string{"HTTP_ADDR": ""}, "HTTP_ADDR"},
		{"address missing port", map[string]string{"HTTP_ADDR": "localhost"}, "HTTP_ADDR"},
		{"address URL", map[string]string{"HTTP_ADDR": "http://localhost:8080"}, "HTTP_ADDR"},
		{"address whitespace", map[string]string{"HTTP_ADDR": "local host:8080"}, "HTTP_ADDR"},
		{"address zero port", map[string]string{"HTTP_ADDR": "localhost:0"}, "HTTP_ADDR"},
		{"address port overflow", map[string]string{"HTTP_ADDR": ":65536"}, "HTTP_ADDR"},
		{"address signed port", map[string]string{"HTTP_ADDR": ":+8080"}, "HTTP_ADDR"},
		{"empty origin", map[string]string{"APP_ORIGIN": ""}, "APP_ORIGIN"},
		{"origin missing scheme", map[string]string{"APP_ORIGIN": "localhost:8080"}, "APP_ORIGIN"},
		{"origin unsupported scheme", map[string]string{"APP_ORIGIN": "ftp://localhost"}, "APP_ORIGIN"},
		{"origin missing host", map[string]string{"APP_ORIGIN": "https://:443"}, "APP_ORIGIN"},
		{"origin credentials", map[string]string{"APP_ORIGIN": "https://user:SENSITIVE@example.com"}, "APP_ORIGIN"},
		{"origin path", map[string]string{"APP_ORIGIN": "https://example.com/login"}, "APP_ORIGIN"},
		{"origin trailing slash", map[string]string{"APP_ORIGIN": "https://example.com/"}, "APP_ORIGIN"},
		{"origin query", map[string]string{"APP_ORIGIN": "https://example.com?secret=SENSITIVE"}, "APP_ORIGIN"},
		{"origin empty query", map[string]string{"APP_ORIGIN": "https://example.com?"}, "APP_ORIGIN"},
		{"origin fragment", map[string]string{"APP_ORIGIN": "https://example.com#SENSITIVE"}, "APP_ORIGIN"},
		{"origin empty fragment", map[string]string{"APP_ORIGIN": "https://example.com#"}, "APP_ORIGIN"},
		{"origin invalid port", map[string]string{"APP_ORIGIN": "https://example.com:abc"}, "APP_ORIGIN"},
		{"origin zero port", map[string]string{"APP_ORIGIN": "https://example.com:0"}, "APP_ORIGIN"},
		{"origin port overflow", map[string]string{"APP_ORIGIN": "https://example.com:65536"}, "APP_ORIGIN"},
		{"origin empty port", map[string]string{"APP_ORIGIN": "https://example.com:"}, "APP_ORIGIN"},
		{"origin HTTP nonloopback", map[string]string{"APP_ORIGIN": "http://example.com"}, "APP_ORIGIN"},
		{"origin loopback suffix", map[string]string{"APP_ORIGIN": "http://localhost.example.com"}, "APP_ORIGIN"},
		{"production missing origin", map[string]string{"APP_ENV": "production"}, "APP_ORIGIN"},
		{"production HTTP loopback", map[string]string{"APP_ENV": "production", "APP_ORIGIN": "http://localhost:8080"}, "APP_ORIGIN"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := load(func(key string) (string, bool) { v, ok := tt.env[key]; return v, ok })
			if err == nil || cfg != (Config{}) {
				t.Fatalf("invalid config accepted: %+v, %v", cfg, err)
			}
			if !strings.Contains(err.Error(), tt.field) || strings.Contains(err.Error(), "SENSITIVE") {
				t.Fatalf("error must identify field without disclosing its value: %v", err)
			}
		})
	}
}
