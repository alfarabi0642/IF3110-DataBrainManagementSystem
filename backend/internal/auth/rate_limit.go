package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"net/netip"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/jackc/pgx/v5"
)

const (
	loginWindow         = 15 * time.Minute
	loginWindowCapacity = 100000
	usernameLimit       = 10
	clientLimit         = 60
)

type RateLimitError struct {
	RetryAfter int
}

func (e *RateLimitError) Error() string { return "login rate limit exceeded" }

func loginClient(remoteAddr string) (string, error) {
	address, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		return "", sessionFailure(problem.InvalidRequest)
	}
	return address.Addr().Unmap().String(), nil
}

func rateLimitHash(key [32]byte, kind, value string) [32]byte {
	hash := hmac.New(sha256.New, key[:])
	_, _ = hash.Write([]byte(kind))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(value))
	var result [32]byte
	copy(result[:], hash.Sum(nil))
	return result
}

// checkLoginRateLimit atomically consumes both the normalized username and
// direct client buckets. Forwarding headers are deliberately ignored until a
// trusted proxy list is configured by the composition root.
func (s *Service) checkLoginRateLimit(ctx context.Context, username, remoteAddr string) error {
	client, err := loginClient(remoteAddr)
	if err != nil {
		return err
	}
	key := s.secrets.RateLimitKey()
	usernameHash := rateLimitHash(key, "username", username)
	clientHash := rateLimitHash(key, "client", client)

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lease, err := platformdb.Acquire(ctx, s.pool)
	if err != nil {
		return sessionFailure(problem.PlatformUnavailable)
	}
	defer lease.Release()
	tx, err := lease.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return sessionFailure(problem.PlatformUnavailable)
	}
	defer rollbackAuth(tx)
	// The table lock keeps the two counters and the global capacity decision
	// indivisible. Password hashing happens only after this short transaction.
	if _, err := tx.Exec(ctx, "LOCK TABLE databrain.login_windows IN SHARE ROW EXCLUSIVE MODE"); err != nil {
		return sessionFailure(problem.PlatformUnavailable)
	}
	var now time.Time
	if err := tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
		return sessionFailure(problem.PlatformUnavailable)
	}
	windowStart := time.Unix(now.Unix()/int64(loginWindow/time.Second)*int64(loginWindow/time.Second), 0).UTC()
	// Use the database timestamp, not the process clock, for Retry-After.
	remaining := windowStart.Add(loginWindow).Sub(now)
	retry := int((remaining + time.Second - 1) / time.Second)
	if retry < 1 {
		retry = 1
	}

	type bucket struct {
		kind  string
		hash  [32]byte
		limit int
	}
	buckets := []bucket{{"username", usernameHash, usernameLimit}, {"client", clientHash, clientLimit}}
	missing := 0
	for _, bucket := range buckets {
		var attempts int
		err := tx.QueryRow(ctx, `SELECT attempts FROM databrain.login_windows
		    WHERE key_type=$1 AND key_hash=$2 AND window_start=$3`, bucket.kind, bucket.hash[:], windowStart).Scan(&attempts)
		if errors.Is(err, pgx.ErrNoRows) {
			missing++
			continue
		}
		if err != nil {
			return sessionFailure(problem.PlatformUnavailable)
		}
		if attempts >= bucket.limit {
			return &RateLimitError{RetryAfter: retry}
		}
	}
	if missing > 0 {
		var active int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM databrain.login_windows
		    WHERE window_start >= $1`, now.Add(-30*time.Minute)).Scan(&active); err != nil {
			return sessionFailure(problem.PlatformUnavailable)
		}
		if active+missing > loginWindowCapacity {
			return &RateLimitError{RetryAfter: retry}
		}
	}
	for _, bucket := range buckets {
		if _, err := tx.Exec(ctx, `INSERT INTO databrain.login_windows(key_type,key_hash,window_start,attempts)
		    VALUES ($1,$2,$3,1) ON CONFLICT (key_type,key_hash,window_start)
		    DO UPDATE SET attempts=databrain.login_windows.attempts+1`, bucket.kind, bucket.hash[:], windowStart); err != nil {
			return sessionFailure(problem.PlatformUnavailable)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return sessionFailure(problem.PlatformUnavailable)
	}
	return nil
}
