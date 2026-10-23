package auth

import (
	"net/http"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
)

func (s *Service) Cookie(token string, expires time.Time) *http.Cookie {
	remaining := int(expires.Sub(s.clock()).Seconds())
	if remaining < 1 {
		remaining = 1
	}
	return &http.Cookie{Name: s.cookieName, Value: token, Path: "/", HttpOnly: true,
		Secure: s.secure, SameSite: http.SameSiteLaxMode, Expires: expires, MaxAge: remaining}
}

func (s *Service) ClearCookie() *http.Cookie {
	return &http.Cookie{Name: s.cookieName, Path: "/", HttpOnly: true, Secure: s.secure,
		SameSite: http.SameSiteLaxMode, Expires: time.Unix(1, 0), MaxAge: -1}
}

func (s *Service) cookieValue(r *http.Request) (string, error) {
	cookies := r.CookiesNamed(s.cookieName)
	if len(cookies) != 1 || cookies[0].Value == "" {
		return "", sessionFailure(problem.SessionInvalid)
	}
	return cookies[0].Value, nil
}
