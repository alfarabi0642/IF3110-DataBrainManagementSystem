// Package config memuat konfigurasi aplikasi.
package config

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	HTTPAddr          string
	AppEnv            string
	AppOrigin         string
	TrustedProxyCIDRs string
	SPADir            string
	StaticDir         string
}

func Load() (Config, error) {
	return load(os.LookupEnv)
}

func load(lookup func(string) (string, bool)) (Config, error) {
	value := func(name, fallback string) string {
		if v, ok := lookup(name); ok {
			return v
		}
		return fallback
	}
	cfg := Config{
		HTTPAddr:  value("HTTP_ADDR", "127.0.0.1:8080"),
		AppEnv:    value("APP_ENV", "development"),
		SPADir:    value("SPA_DIR", "../frontend/dist"),
		StaticDir: value("STATIC_DIR", "web/static"),
	}
	if strings.TrimSpace(cfg.SPADir) == "" || strings.TrimSpace(cfg.StaticDir) == "" {
		return Config{}, fmt.Errorf("SPA_DIR and STATIC_DIR must not be empty")
	}
	switch cfg.AppEnv {
	case "development", "test", "production":
	default:
		return Config{}, fmt.Errorf("APP_ENV must be development, test, or production")
	}

	host, port, err := net.SplitHostPort(cfg.HTTPAddr)
	if err != nil || !validPort(port) || strings.ContainsAny(host, " \t\r\n/@?#\\%") {
		return Config{}, fmt.Errorf("HTTP_ADDR must be a host:port address with port 1-65535")
	}

	defaultOrigin := "http://localhost:8080"
	if cfg.AppEnv == "production" {
		defaultOrigin = ""
	}
	cfg.AppOrigin = value("APP_ORIGIN", defaultOrigin)
	origin, err := url.Parse(cfg.AppOrigin)
	if err != nil || origin.Hostname() == "" || origin.User != nil ||
		(origin.Scheme != "http" && origin.Scheme != "https") ||
		cfg.AppOrigin != origin.Scheme+"://"+origin.Host ||
		strings.HasSuffix(origin.Host, ":") || (origin.Port() != "" && !validPort(origin.Port())) {
		return Config{}, fmt.Errorf("APP_ORIGIN must contain only an http(s) scheme and host with an optional port")
	}
	if origin.Scheme == "http" {
		ip := net.ParseIP(origin.Hostname())
		loopback := origin.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())
		if cfg.AppEnv == "production" || !loopback {
			return Config{}, fmt.Errorf("APP_ORIGIN requires HTTPS except on development/test loopback")
		}
	}
	cfg.TrustedProxyCIDRs, err = normalizeProxyCIDRs(value("TRUSTED_PROXY_CIDRS", ""))
	if err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func normalizeProxyCIDRs(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	seen := make(map[netip.Prefix]struct{})
	values := make([]string, 0)
	for _, value := range strings.Split(raw, ",") {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(value))
		if err != nil {
			return "", fmt.Errorf("TRUSTED_PROXY_CIDRS must contain IP prefixes")
		}
		prefix = prefix.Masked()
		if _, ok := seen[prefix]; ok {
			continue
		}
		seen[prefix] = struct{}{}
		values = append(values, prefix.String())
	}
	return strings.Join(values, ","), nil
}

func validPort(port string) bool {
	if port == "" || strings.IndexFunc(port, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return false
	}
	n, err := strconv.Atoi(port)
	return err == nil && n >= 1 && n <= 65535
}
