// Package secret menyediakan format rahasia v1 untuk auth dan target. Penerbitan session, hash
// password, dan policy CSRF ditangani paket masing-masing.
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

// Secrets menyimpan kunci terpisah untuk token session, password target, CSRF, dan rate limit. Buat
// melalui New; root key tidak disimpan.
type Secrets struct {
	session   cipher.AEAD
	target    cipher.AEAD
	csrf      [32]byte
	rateLimit [32]byte
}

// New menurunkan empat kunci 32 byte memakai HKDF-SHA256 tanpa salt dan info v1 tetap.
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
	// Primitive standar ini menambahkan nonce acak 12 byte di awal dan tag autentikasi 16 byte di akhir.
	// Pemanggil tidak memasok atau memakai ulang nonce.
	return cipher.NewGCMWithRandomNonce(block)
}

// SealSession mengikat token ke sessionID melalui AAD session:<UUID>:v1.
func (s *Secrets) SealSession(sessionID uuid.UUID, token []byte) ([]byte, error) {
	return seal(s.session, "session", sessionID, token)
}

func (s *Secrets) OpenSession(sessionID uuid.UUID, envelope []byte) ([]byte, error) {
	return open(s.session, "session", sessionID, envelope)
}

// SealTarget mengikat password ke targetID melalui AAD target:<UUID>:v1.
func (s *Secrets) SealTarget(targetID uuid.UUID, password []byte) ([]byte, error) {
	return seal(s.target, "target", targetID, password)
}

func (s *Secrets) OpenTarget(targetID uuid.UUID, envelope []byte) ([]byte, error) {
	return open(s.target, "target", targetID, envelope)
}

// CSRFKey mengembalikan salinan kunci untuk HMAC-SHA256 CSRF.
func (s *Secrets) CSRFKey() [32]byte { return s.csrf }

// RateLimitKey mengembalikan salinan kunci untuk bucket login HMAC-SHA256.
func (s *Secrets) RateLimitKey() [32]byte { return s.rateLimit }

func seal(aead cipher.AEAD, scope string, id uuid.UUID, plaintext []byte) ([]byte, error) {
	if id == uuid.Nil {
		return nil, ErrInvalidID
	}
	// Format envelope: byte versi || nonce || ciphertext || tag autentikasi.
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
