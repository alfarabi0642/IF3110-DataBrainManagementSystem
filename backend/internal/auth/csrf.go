package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"

	"github.com/google/uuid"
)

func csrfToken(key [32]byte, sessionID uuid.UUID) string {
	mac := hmac.New(sha256.New, key[:])
	_, _ = mac.Write([]byte("csrf:" + sessionID.String()))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func validCSRF(expected, submitted string) bool {
	if len(expected) != 43 || len(submitted) != 43 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(expected), []byte(submitted)) == 1
}
