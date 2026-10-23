package auth

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestSessionTokenCodec(t *testing.T) {
	raw, encoded, err := newToken()
	if err != nil || len(raw) != 32 || len(encoded) != 43 {
		t.Fatalf("token generation failed: bytes=%d encoded=%d error=%v", len(raw), len(encoded), err)
	}
	decoded, err := decodeToken(encoded)
	if err != nil || !bytes.Equal(decoded, raw) {
		t.Fatal("canonical token did not round trip")
	}
	for _, value := range []string{"", encoded + "=", encoded[:42], "!" + encoded[1:]} {
		if _, err := decodeToken(value); err == nil {
			t.Fatalf("invalid token accepted: %q", value)
		}
	}
}

func TestCSRFTokenIsStableAndFamilyBound(t *testing.T) {
	var key [32]byte
	copy(key[:], bytes.Repeat([]byte{0x42}, 32))
	firstID, secondID := uuid.New(), uuid.New()
	first := csrfToken(key, firstID)
	if len(first) != 43 || first != csrfToken(key, firstID) || first == csrfToken(key, secondID) {
		t.Fatal("CSRF token is not canonical, stable, and family-bound")
	}
	if !validCSRF(first, first) || validCSRF(first, csrfToken(key, secondID)) || validCSRF(first, "short") {
		t.Fatal("CSRF comparison accepted the wrong token")
	}
}

func TestSessionCookieAttributes(t *testing.T) {
	now := time.Date(2026, 10, 23, 8, 0, 0, 0, time.UTC)
	service := &Service{clock: func() time.Time { return now }, cookieName: ProductionCookieName, secure: true}
	cookie := service.Cookie("token", now.Add(10*time.Minute))
	if cookie.Name != ProductionCookieName || cookie.Path != "/" || !cookie.HttpOnly || !cookie.Secure ||
		cookie.SameSite != http.SameSiteLaxMode || cookie.MaxAge != 600 {
		t.Fatal("session cookie attributes differ from policy")
	}
	cleared := service.ClearCookie()
	if cleared.MaxAge != -1 || cleared.Value != "" || cleared.Name != cookie.Name || cleared.Path != cookie.Path ||
		cleared.Secure != cookie.Secure || cleared.SameSite != cookie.SameSite {
		t.Fatal("clear cookie does not match the session cookie")
	}
}

func TestMiddlewareRequiresService(t *testing.T) {
	if middleware, err := NewMiddleware(nil); middleware != nil || err == nil {
		t.Fatal("nil session service accepted")
	}
}

func TestCookieValueRejectsMissingAndDuplicateCookies(t *testing.T) {
	service := &Service{cookieName: SessionCookieName}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	if _, err := service.cookieValue(request); err == nil {
		t.Fatal("missing session cookie accepted")
	}
	request.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "first"})
	request.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "second"})
	if _, err := service.cookieValue(request); err == nil {
		t.Fatal("duplicate session cookies accepted")
	}
}
