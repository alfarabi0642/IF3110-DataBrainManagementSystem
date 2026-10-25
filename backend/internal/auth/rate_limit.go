package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"net/netip"
	"strings"
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

func loginClient(remoteAddr, forwardedFor string, trusted []netip.Prefix) (string, error) {
	address, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		return "", sessionFailure(problem.InvalidRequest)
	}
	client := address.Addr().Unmap()
	if !containsIP(trusted, client) || strings.TrimSpace(forwardedFor) == "" {
		return client.String(), nil
	}
	chain := strings.Split(forwardedFor, ",")
	for i := len(chain) - 1; i >= 0; i-- {
		raw := strings.TrimSpace(chain[i])
		ip, err := netip.ParseAddr(raw)
		if err != nil || ip.Zone() != "" {
			return "", sessionFailure(problem.InvalidRequest)
		}
		client = ip.Unmap()
		if !containsIP(trusted, client) {
			return client.String(), nil
		}
	}
	return client.String(), nil
}

func containsIP(prefixes []netip.Prefix, ip netip.Addr) bool {
	for _, prefix := range prefixes {
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
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

// checkLoginRateLimit memakai bucket username ternormalisasi dan client secara atomik. Header proxy
// hanya dipercaya dari jaringan proxy terkonfigurasi.
func (s *Service) checkLoginRateLimit(ctx context.Context, username, remoteAddr, forwardedFor string) error {
	client, err := loginClient(remoteAddr, forwardedFor, s.trustedProxies)
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
	// Lock tabel menyatukan kedua counter dan keputusan kapasitas global. Hash password baru dihitung
	// setelah transaksi singkat ini selesai.
	if _, err := tx.Exec(ctx, "LOCK TABLE databrain.login_windows IN SHARE ROW EXCLUSIVE MODE"); err != nil {
		return sessionFailure(problem.PlatformUnavailable)
	}
	var now time.Time
	if err := tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
		return sessionFailure(problem.PlatformUnavailable)
	}
	windowStart := time.Unix(now.Unix()/int64(loginWindow/time.Second)*int64(loginWindow/time.Second), 0).UTC()
	// Retry-After memakai timestamp database agar konsisten antar request.
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
