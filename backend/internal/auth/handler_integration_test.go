package auth

import (
	"errors"
	"testing"
)

func TestLoginRateLimitDatabase(t *testing.T) {
	service, ctx := sessionFixture(t)
	for range usernameLimit {
		if err := service.checkLoginRateLimit(ctx, "limited_user", "192.0.2.20:4321", ""); err != nil {
			t.Fatal(err)
		}
	}
	var limited *RateLimitError
	if err := service.checkLoginRateLimit(ctx, "limited_user", "192.0.2.20:4321", ""); !errors.As(err, &limited) || limited.RetryAfter < 1 || limited.RetryAfter > 900 {
		t.Fatalf("limit error=%v retry=%+v", err, limited)
	}
	var rows, usernameAttempts, clientAttempts int
	if err := service.pool.QueryRow(ctx, `SELECT count(*),
	    coalesce(max(attempts) FILTER (WHERE key_type='username'),0),
	    coalesce(max(attempts) FILTER (WHERE key_type='client'),0)
	    FROM databrain.login_windows`).Scan(&rows, &usernameAttempts, &clientAttempts); err != nil ||
		rows != 2 || usernameAttempts != usernameLimit || clientAttempts != usernameLimit {
		t.Fatalf("buckets=%d username=%d client=%d error=%v", rows, usernameAttempts, clientAttempts, err)
	}
}
