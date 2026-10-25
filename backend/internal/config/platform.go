package config

import (
	"errors"
	"net/url"
	"os"
	"strings"
)

// LoadPlatformDatabaseURL hanya membaca DSN runtime, tanpa fallback ke migrator. Pesan error tidak
// memuat connection string.
func LoadPlatformDatabaseURL(appEnv string) (string, error) {
	raw := os.Getenv("PLATFORM_DATABASE_URL")
	u, err := url.Parse(raw)
	if err != nil || u == nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") ||
		u.Hostname() == "" || u.User == nil || u.User.Username() == "" ||
		u.Path == "" || u.Path == "/" || u.Fragment != "" ||
		(u.Port() != "" && !validPort(u.Port())) {
		return "", errors.New("PLATFORM_DATABASE_URL must be a PostgreSQL URL with host, database and runtime user")
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", errors.New("PLATFORM_DATABASE_URL has invalid options")
	}
	for key, values := range query {
		if len(values) != 1 || strings.HasPrefix(key, "pool_") || strings.HasPrefix(key, "x-") {
			return "", errors.New("PLATFORM_DATABASE_URL has unsupported or repeated options")
		}
	}
	if appEnv == "production" && query.Get("sslmode") != "verify-full" {
		return "", errors.New("PLATFORM_DATABASE_URL requires sslmode=verify-full in production")
	}
	return databasePassword(u, raw, "PLATFORM_DATABASE_PASSWORD"), nil
}

// databasePassword menyisipkan password mentah tanpa menganggapnya bagian dari URI. Variabel yang
// tidak diisi mempertahankan password pada URL untuk kompatibilitas.
func databasePassword(u *url.URL, raw, key string) string {
	if password, ok := os.LookupEnv(key); ok {
		u.User = url.UserPassword(u.User.Username(), password)
		return u.String()
	}
	return raw
}
