package config

import (
	"errors"
	"net/url"
	"os"
	"strings"
)

// LoadMigratorDatabaseURL hanya dipakai oleh manage, bukan server HTTP.
func LoadMigratorDatabaseURL() (string, error) {
	appEnv, ok := os.LookupEnv("APP_ENV")
	if !ok {
		appEnv = "development"
	}
	raw, err := migratorDatabaseURL(os.Getenv("MIGRATOR_DATABASE_URL"), appEnv)
	if err != nil {
		return "", err
	}
	u, _ := url.Parse(raw)
	return databasePassword(u, raw, "MIGRATOR_DATABASE_PASSWORD"), nil
}

func migratorDatabaseURL(raw, appEnv string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u == nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") ||
		u.Hostname() == "" || u.User == nil || u.User.Username() == "" ||
		u.Path == "" || u.Path == "/" || u.Fragment != "" ||
		(u.Port() != "" && !validPort(u.Port())) {
		return "", errors.New("MIGRATOR_DATABASE_URL must be a PostgreSQL URL with host, database and migrator user")
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", errors.New("MIGRATOR_DATABASE_URL has invalid options")
	}
	for key, values := range query {
		if len(values) != 1 || strings.HasPrefix(key, "x-") {
			return "", errors.New("MIGRATOR_DATABASE_URL has unsupported or repeated options")
		}
	}
	switch appEnv {
	case "development", "test":
	case "production":
		if query.Get("sslmode") != "verify-full" {
			return "", errors.New("MIGRATOR_DATABASE_URL requires sslmode=verify-full in production")
		}
	default:
		return "", errors.New("APP_ENV must be development, test, or production")
	}
	// Batasi koneksi serta penantian metadata, advisory lock, dan DDL pada koneksi khusus yang sama.
	// Migration multi-statement berjalan sebagai satu kesatuan.
	query.Set("connect_timeout", "5")
	query.Set("statement_timeout", "10000")
	query.Set("lock_timeout", "3000")
	query.Set("search_path", "pg_catalog,public")
	u.RawQuery = query.Encode()
	return u.String(), nil
}
