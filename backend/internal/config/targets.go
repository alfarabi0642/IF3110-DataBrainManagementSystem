package config

import (
	"errors"
	"net/netip"
	"os"
	"strings"
)

type TargetSettings struct {
	AppEnv       string
	AllowedHosts []string
	CAFile       string
}

// LoadTargetSettings diperlukan saat Provider target dipasang.
func LoadTargetSettings(appEnv string) (TargetSettings, error) {
	cfg := TargetSettings{AppEnv: appEnv, CAFile: os.Getenv("TARGET_CA_FILE")}
	if appEnv != "development" && appEnv != "test" && appEnv != "production" {
		return TargetSettings{}, errors.New("APP_ENV must be development, test, or production")
	}
	for _, raw := range strings.Split(os.Getenv("TARGET_ALLOWED_HOSTS"), ",") {
		host, err := NormalizeTargetHost(raw)
		if err != nil {
			return TargetSettings{}, errors.New("TARGET_ALLOWED_HOSTS must contain exact DNS hosts or IP addresses")
		}
		cfg.AllowedHosts = append(cfg.AllowedHosts, host)
	}
	return cfg, nil
}

// NormalizeTargetHost menerima satu host DNS atau IP, tanpa URL, DSN, atau socket.
func NormalizeTargetHost(raw string) (string, error) {
	host := strings.TrimSpace(raw)
	if ip, err := netip.ParseAddr(host); err == nil && ip.Zone() == "" {
		return ip.Unmap().String(), nil
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if len(host) < 1 || len(host) > 253 {
		return "", errors.New("invalid target host")
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) < 1 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", errors.New("invalid target host")
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return "", errors.New("invalid target host")
			}
		}
	}
	return host, nil
}
