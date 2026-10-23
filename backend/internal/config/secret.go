package config

import (
	"encoding/base64"
	"errors"
	"os"
)

// LoadSecretKey reads APP_SECRET_KEY when an auth or target consumer needs it.
// The value must be canonical padded standard base64 of exactly 32 bytes.
// There is no default key in any environment, and errors never include its value.
func LoadSecretKey() ([]byte, error) {
	raw := os.Getenv("APP_SECRET_KEY")
	key, err := base64.StdEncoding.Strict().DecodeString(raw)
	if err != nil || len(key) != 32 || base64.StdEncoding.EncodeToString(key) != raw {
		return nil, errors.New("APP_SECRET_KEY must be standard padded base64 of 32 random bytes")
	}
	return key, nil
}
