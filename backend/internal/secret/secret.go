// Package secret supplies the shared v1 secret format for auth and targets.
// It does not issue sessions, hash passwords, or implement CSRF policy.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"errors"

	"github.com/google/uuid"
)

const version byte = 1

var (
	ErrInvalidKey     = errors.New("secret: root key must contain 32 bytes")
	ErrInitialization = errors.New("secret: initialization failed")
	ErrInvalidID      = errors.New("secret: resource UUID is required")
	ErrOpen           = errors.New("secret: invalid sealed value")
)

// Secrets holds separate keys for session tokens, target passwords, CSRF and
// login rate limiting. Construct it with New; it does not retain the root key.
type Secrets struct {
	session   cipher.AEAD
	target    cipher.AEAD
	csrf      [32]byte
	rateLimit [32]byte
}

// New derives four 32-byte HKDF-SHA256 keys with no salt and fixed v1 info.
func New(rootKey []byte) (*Secrets, error) {
	if len(rootKey) != 32 {
		return nil, ErrInvalidKey
	}
	var keys [4][32]byte
	for i, info := range [...]string{
		"databrain/session-token/v1",
		"databrain/target-secret/v1",
		"databrain/csrf/v1",
		"databrain/rate-limit/v1",
	} {
		key, err := hkdf.Key(sha256.New, rootKey, nil, info, 32)
		if err != nil {
			return nil, ErrInitialization
		}
		copy(keys[i][:], key)
	}
	session, err := newAEAD(keys[0][:])
	if err != nil {
		return nil, ErrInitialization
	}
	target, err := newAEAD(keys[1][:])
	if err != nil {
		return nil, ErrInitialization
	}
	return &Secrets{session: session, target: target, csrf: keys[2], rateLimit: keys[3]}, nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	// This standard primitive prepends a fresh random 12-byte nonce and appends
	// the 16-byte authentication tag. Callers never supply or reuse a nonce.
	return cipher.NewGCMWithRandomNonce(block)
}

// SealSession binds the token to sessionID with AAD session:<UUID>:v1.
func (s *Secrets) SealSession(sessionID uuid.UUID, token []byte) ([]byte, error) {
	return seal(s.session, "session", sessionID, token)
}

func (s *Secrets) OpenSession(sessionID uuid.UUID, envelope []byte) ([]byte, error) {
	return open(s.session, "session", sessionID, envelope)
}

// SealTarget binds the password to targetID with AAD target:<UUID>:v1.
func (s *Secrets) SealTarget(targetID uuid.UUID, password []byte) ([]byte, error) {
	return seal(s.target, "target", targetID, password)
}

func (s *Secrets) OpenTarget(targetID uuid.UUID, envelope []byte) ([]byte, error) {
	return open(s.target, "target", targetID, envelope)
}

// CSRFKey returns a copy for auth's HMAC-SHA256 CSRF implementation.
func (s *Secrets) CSRFKey() [32]byte { return s.csrf }

// RateLimitKey returns a copy for auth's HMAC-SHA256 login bucket keys.
func (s *Secrets) RateLimitKey() [32]byte { return s.rateLimit }

func seal(aead cipher.AEAD, scope string, id uuid.UUID, plaintext []byte) ([]byte, error) {
	if id == uuid.Nil {
		return nil, ErrInvalidID
	}
	// Envelope: version byte || nonce || ciphertext || authentication tag.
	return aead.Seal([]byte{version}, nil, plaintext, []byte(scope+":"+id.String()+":v1")), nil
}

func open(aead cipher.AEAD, scope string, id uuid.UUID, envelope []byte) ([]byte, error) {
	if id == uuid.Nil {
		return nil, ErrInvalidID
	}
	if len(envelope) < 1+aead.Overhead() || envelope[0] != version {
		return nil, ErrOpen
	}
	plaintext, err := aead.Open(nil, nil, envelope[1:], []byte(scope+":"+id.String()+":v1"))
	if err != nil {
		return nil, ErrOpen
	}
	return plaintext, nil
}
