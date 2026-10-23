package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

const (
	passwordHashPrefix = "$argon2id$v=19$m=65536,t=3,p=2$"
	passwordHashLength = len(passwordHashPrefix) + 22 + 1 + 43
	passwordHashWait   = 2 * time.Second

	// This public dummy fixture is never used to create an account.
	dummyPasswordHash = passwordHashPrefix + "RGF0YUJyYWluRHVtbXlWMQ$mfe1mmxSlxTPDUyj41pvXKo1DpF5T3Zfvxc1vhVsyAw"
)

var (
	ErrInvalidPassword     = errors.New("password must be valid UTF-8 and contain 12 to 128 bytes")
	ErrInvalidPasswordHash = errors.New("invalid or unsupported password hash")
	ErrPasswordHashBusy    = errors.New("password hashing capacity unavailable")

	// All callers share the process-wide limit, including verification and dummy work.
	passwordHashSlots = make(chan struct{}, 2)
)

// ValidatePassword checks bytes without trimming, normalizing, or truncating input.
func ValidatePassword(password string) error {
	if len(password) < 12 || len(password) > 128 || !utf8.ValidString(password) {
		return ErrInvalidPassword
	}
	return nil
}

// HashPassword uses the fixed M1 Argon2id policy and a fresh random salt.
func HashPassword(ctx context.Context, password string) (string, error) {
	if err := ValidatePassword(password); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := derivePasswordKey(ctx, password, salt)
	if err != nil {
		return "", err
	}
	return passwordHashPrefix + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(key), nil
}

// VerifyPassword distinguishes a mismatch (false, nil) from invalid data or capacity errors.
func VerifyPassword(ctx context.Context, password, encoded string) (bool, error) {
	if err := ValidatePassword(password); err != nil {
		return false, err
	}
	salt, expected, err := parsePasswordHash(encoded)
	if err != nil {
		return false, err
	}
	key, err := derivePasswordKey(ctx, password, salt)
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(key, expected) == 1, nil
}

// CheckDummyPassword performs normal verification work for missing/inactive users.
// A nil error means the work completed; the login caller must still reject credentials.
func CheckDummyPassword(ctx context.Context, password string) error {
	_, err := VerifyPassword(ctx, password, dummyPasswordHash)
	return err
}

func parsePasswordHash(encoded string) ([]byte, []byte, error) {
	// Only the fixed M1 format is supported; reject costs before any Argon2 allocation.
	if len(encoded) != passwordHashLength || !strings.HasPrefix(encoded, passwordHashPrefix) || strings.ContainsAny(encoded, "\r\n") {
		return nil, nil, ErrInvalidPasswordHash
	}
	saltText, keyText, ok := strings.Cut(encoded[len(passwordHashPrefix):], "$")
	if !ok || len(saltText) != 22 || len(keyText) != 43 {
		return nil, nil, ErrInvalidPasswordHash
	}
	encoding := base64.RawStdEncoding.Strict()
	salt, err := encoding.DecodeString(saltText)
	if err != nil || len(salt) != 16 {
		return nil, nil, ErrInvalidPasswordHash
	}
	key, err := encoding.DecodeString(keyText)
	if err != nil || len(key) != 32 {
		return nil, nil, ErrInvalidPasswordHash
	}
	return salt, key, nil
}

func derivePasswordKey(ctx context.Context, password string, salt []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	timer := time.NewTimer(passwordHashWait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, ErrPasswordHashBusy
	case passwordHashSlots <- struct{}{}:
	}
	defer func() { <-passwordHashSlots }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// IDKey cannot be interrupted; keep the slot until it returns even if canceled.
	key := argon2.IDKey([]byte(password), salt, 3, 65536, 2, 32)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return key, nil
}
