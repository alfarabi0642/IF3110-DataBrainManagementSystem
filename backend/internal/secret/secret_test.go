package secret_test

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"testing"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/secret"
	"github.com/google/uuid"
)

var resourceID = uuid.MustParse("11111111-2222-4333-8444-555555555555")

func newSecrets(t *testing.T, root []byte) *secret.Secrets {
	t.Helper()
	s, err := secret.New(root)
	if err != nil {
		t.Fatal("valid synthetic root key rejected")
	}
	return s
}

func TestRoundTripAndNonce(t *testing.T) {
	root := bytes.Repeat([]byte{0x42}, 32)
	s := newSecrets(t, root)
	reloaded := newSecrets(t, root)
	for _, tc := range []struct {
		name string
		seal func(uuid.UUID, []byte) ([]byte, error)
		open func(uuid.UUID, []byte) ([]byte, error)
	}{
		{"session", s.SealSession, reloaded.OpenSession},
		{"target", s.SealTarget, reloaded.OpenTarget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, plaintext := range [][]byte{nil, []byte("  kata sandi 猫\r\n"), bytes.Repeat([]byte{0xff, 0}, 16)} {
				before := bytes.Clone(plaintext)
				first, err := tc.seal(resourceID, plaintext)
				if err != nil || len(first) != 1+12+len(plaintext)+16 || first[0] != 1 {
					t.Fatal("seal did not produce a v1 envelope with a 12-byte nonce and 16-byte tag")
				}
				second, err := tc.seal(resourceID, plaintext)
				if err != nil || bytes.Equal(first[1:13], second[1:13]) {
					t.Fatal("sealing reused a nonce")
				}
				saved := bytes.Clone(first)
				got, err := tc.open(resourceID, first)
				if err != nil || !bytes.Equal(got, plaintext) {
					t.Fatal("sealed value did not roundtrip with the same root after reinitialization")
				}
				if !bytes.Equal(plaintext, before) || !bytes.Equal(first, saved) {
					t.Fatal("sealing or opening modified caller input")
				}
			}
		})
	}
}

func TestV1Interoperability(t *testing.T) {
	root := bytes.Repeat([]byte{0x42}, 32)
	s := newSecrets(t, root)
	for _, tc := range []struct {
		name, info, aad string
		seal            func(uuid.UUID, []byte) ([]byte, error)
		open            func(uuid.UUID, []byte) ([]byte, error)
	}{
		{"session", "databrain/session-token/v1", "session:11111111-2222-4333-8444-555555555555:v1", s.SealSession, s.OpenSession},
		{"target", "databrain/target-secret/v1", "target:11111111-2222-4333-8444-555555555555:v1", s.SealTarget, s.OpenTarget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Decode the public format with ordinary AES-GCM, independently of
			// the helper's random-nonce envelope implementation.
			key, err := hkdf.Key(sha256.New, root, nil, tc.info, 32)
			if err != nil {
				t.Fatal("HKDF setup failed")
			}
			block, err := aes.NewCipher(key)
			if err != nil {
				t.Fatal("AES setup failed")
			}
			gcm, err := cipher.NewGCM(block)
			if err != nil {
				t.Fatal("GCM setup failed")
			}
			plaintext := []byte("synthetic sensitive value")
			envelope, err := tc.seal(resourceID, plaintext)
			if err != nil {
				t.Fatal("seal failed")
			}
			got, err := gcm.Open(nil, envelope[1:13], envelope[13:], []byte(tc.aad))
			if err != nil || !bytes.Equal(got, plaintext) {
				t.Fatal("v1 envelope is incompatible with the specified key, AAD or wire format")
			}
			nonce := bytes.Repeat([]byte{0x11}, 12) // Fixed only for this synthetic compatibility fixture.
			fixture := gcm.Seal(append([]byte{1}, nonce...), nonce, plaintext, []byte(tc.aad))
			got, err = tc.open(resourceID, fixture)
			if err != nil || !bytes.Equal(got, plaintext) {
				t.Fatal("helper could not read a standard v1 fixture")
			}
		})
	}
}

func TestOpenRejectsWrongKeyResourceAndContext(t *testing.T) {
	s := newSecrets(t, bytes.Repeat([]byte{0x42}, 32))
	other := newSecrets(t, bytes.Repeat([]byte{0x43}, 32))
	session, err := s.SealSession(resourceID, []byte("synthetic session token"))
	if err != nil {
		t.Fatal("seal session failed")
	}
	target, err := s.SealTarget(resourceID, []byte("synthetic target password"))
	if err != nil {
		t.Fatal("seal target failed")
	}
	for _, tc := range []struct {
		name     string
		open     func(uuid.UUID, []byte) ([]byte, error)
		id       uuid.UUID
		envelope []byte
	}{
		{"wrong session key", other.OpenSession, resourceID, session},
		{"wrong target key", other.OpenTarget, resourceID, target},
		{"wrong session UUID", s.OpenSession, uuid.New(), session},
		{"wrong target UUID", s.OpenTarget, uuid.New(), target},
		{"session as target", s.OpenTarget, resourceID, session},
		{"target as session", s.OpenSession, resourceID, target},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := tc.open(tc.id, tc.envelope); got != nil || err != secret.ErrOpen {
				t.Fatal("wrong key/resource/context accepted or error disclosed input")
			}
		})
	}
}

func TestOpenRejectsMalformedOrTamperedEnvelope(t *testing.T) {
	s := newSecrets(t, bytes.Repeat([]byte{0x42}, 32))
	for _, tc := range []struct {
		name string
		seal func(uuid.UUID, []byte) ([]byte, error)
		open func(uuid.UUID, []byte) ([]byte, error)
	}{
		{"session", s.SealSession, s.OpenSession},
		{"target", s.SealTarget, s.OpenTarget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			envelope, err := tc.seal(resourceID, []byte("synthetic sensitive value"))
			if err != nil {
				t.Fatal("seal failed")
			}
			for i := range envelope {
				if got, err := tc.open(resourceID, envelope[:i]); got != nil || err != secret.ErrOpen {
					t.Fatal("truncated envelope accepted or disclosed")
				}
				tampered := bytes.Clone(envelope)
				tampered[i] ^= 0xff // Covers version, nonce, ciphertext and tag.
				if got, err := tc.open(resourceID, tampered); got != nil || err != secret.ErrOpen {
					t.Fatal("tampered envelope accepted or disclosed")
				}
			}
			if got, err := tc.open(resourceID, append(bytes.Clone(envelope), 0)); got != nil || err != secret.ErrOpen {
				t.Fatal("appended envelope bytes accepted or disclosed")
			}
		})
	}
}

func TestInvalidRootKeyAndResourceID(t *testing.T) {
	for _, size := range []int{0, 16, 24, 31, 33, 64} {
		if s, err := secret.New(bytes.Repeat([]byte{0x42}, size)); s != nil || err != secret.ErrInvalidKey {
			t.Fatal("invalid AES-256 root key accepted or disclosed")
		}
	}
	s := newSecrets(t, bytes.Repeat([]byte{0x42}, 32))
	for _, operation := range []func(uuid.UUID, []byte) ([]byte, error){s.SealSession, s.OpenSession, s.SealTarget, s.OpenTarget} {
		if got, err := operation(uuid.Nil, []byte("synthetic sensitive value")); got != nil || err != secret.ErrInvalidID {
			t.Fatal("nil UUID accepted or disclosed")
		}
	}
}

func TestAuthSubkeysAreSeparatedAndCopied(t *testing.T) {
	root := bytes.Repeat([]byte{0x42}, 32)
	s := newSecrets(t, root)
	csrf, rate := s.CSRFKey(), s.RateLimitKey()
	for _, tc := range []struct {
		info string
		got  [32]byte
	}{
		{"databrain/csrf/v1", csrf},
		{"databrain/rate-limit/v1", rate},
	} {
		want, err := hkdf.Key(sha256.New, root, nil, tc.info, 32)
		if err != nil || !bytes.Equal(tc.got[:], want) || bytes.Equal(tc.got[:], root) {
			t.Fatal("auth subkey does not match its specified derivation")
		}
	}
	if csrf == rate {
		t.Fatal("CSRF and rate-limit keys must be separate")
	}
	csrf[0] ^= 0xff
	rate[0] ^= 0xff
	clear(root)
	if s.CSRFKey() == csrf || s.RateLimitKey() == rate {
		t.Fatal("consumer modified a retained key")
	}
	if s.CSRFKey() != newSecrets(t, bytes.Repeat([]byte{0x42}, 32)).CSRFKey() {
		t.Fatal("helper retained caller root memory")
	}
}
