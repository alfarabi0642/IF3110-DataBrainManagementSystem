package config

import (
	"encoding/base64"
	"errors"
	"os"
)

// LoadSecretKey membaca APP_SECRET_KEY untuk auth dan target. Nilai harus base64 standar kanonis
// dengan padding dari tepat 32 byte. Tidak ada default di konfigurasi Go; pesan error tidak memuat
// nilainya.
func LoadSecretKey() ([]byte, error) {
	raw := os.Getenv("APP_SECRET_KEY")
	key, err := base64.StdEncoding.Strict().DecodeString(raw)
	if err != nil || len(key) != 32 || base64.StdEncoding.EncodeToString(key) != raw {
		return nil, errors.New("APP_SECRET_KEY must be standard padded base64 of 32 random bytes")
	}
	return key, nil
}
