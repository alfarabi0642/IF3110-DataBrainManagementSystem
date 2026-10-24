package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/secret"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	SessionCookieName    = "databrain_sid"
	ProductionCookieName = "__Host-databrain_sid"

	anonymousLifetime     = 10 * time.Minute
	authenticatedIdle     = 30 * time.Minute
	authenticatedLifetime = 8 * time.Hour
	rotationInterval      = 15 * time.Minute
	aliasGrace            = 120 * time.Second
)

type SessionKind string

const (
	AnonymousSession     SessionKind = "anonymous"
	AuthenticatedSession SessionKind = "authenticated"
)

type Clock func() time.Time

type Policy struct {
	AppEnv            string
	AppOrigin         string
	TrustedProxyCIDRs string
}

type Session struct {
	ID                uuid.UUID
	Kind              SessionKind
	Principal         access.Principal
	ReauthRequired    bool
	AbsoluteExpiresAt time.Time
	CSRF              string
}

type Service struct {
	pool           *pgxpool.Pool
	secrets        *secret.Secrets
	clock          Clock
	appOrigin      string
	cookieName     string
	secure         bool
	trustedProxies []netip.Prefix
}

var (
	ErrAlreadyAuthenticated = &problem.Error{Code: problem.AlreadyAuthenticated}
	ErrReauthRequired       = errors.New("session requires reauthentication")
)

func NewService(pool *pgxpool.Pool, secrets *secret.Secrets, policy Policy, clock Clock) (*Service, error) {
	if pool == nil || secrets == nil {
		return nil, errors.New("auth: platform pool and secrets are required")
	}
	origin, err := url.Parse(policy.AppOrigin)
	if err != nil || origin.User != nil || origin.Hostname() == "" ||
		(origin.Scheme != "http" && origin.Scheme != "https") || policy.AppOrigin != origin.Scheme+"://"+origin.Host {
		return nil, errors.New("auth: invalid application origin")
	}
	if policy.AppEnv != "development" && policy.AppEnv != "test" && policy.AppEnv != "production" {
		return nil, errors.New("auth: invalid application environment")
	}
	secure := origin.Scheme == "https"
	if !secure {
		ip := net.ParseIP(origin.Hostname())
		if policy.AppEnv == "production" || origin.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return nil, errors.New("auth: insecure cookies require a development/test loopback origin")
		}
	}
	if clock == nil {
		clock = time.Now
	}
	trustedProxies, err := parseTrustedProxies(policy.TrustedProxyCIDRs)
	if err != nil {
		return nil, err
	}
	name := SessionCookieName
	if policy.AppEnv == "production" {
		name = ProductionCookieName
	}
	return &Service{pool: pool, secrets: secrets, clock: clock, appOrigin: policy.AppOrigin, cookieName: name, secure: secure, trustedProxies: trustedProxies}, nil
}

func parseTrustedProxies(raw string) ([]netip.Prefix, error) {
	if raw == "" {
		return nil, nil
	}
	values := strings.Split(raw, ",")
	result := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(value))
		if err != nil {
			return nil, errors.New("auth: invalid trusted proxy prefix")
		}
		result = append(result, prefix.Masked())
	}
	return result, nil
}

func sessionFailure(code problem.Code) error { return &problem.Error{Code: code} }

func newToken() ([]byte, string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, "", sessionFailure(problem.InternalError)
	}
	return raw, base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeToken(value string) ([]byte, error) {
	if len(value) != 43 || strings.Contains(value, "=") {
		return nil, sessionFailure(problem.SessionInvalid)
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != value {
		return nil, sessionFailure(problem.SessionInvalid)
	}
	return raw, nil
}

func tokenHash(raw []byte) [32]byte { return sha256.Sum256(raw) }

func (s *Service) csrf(sessionID uuid.UUID) string {
	return csrfToken(s.secrets.CSRFKey(), sessionID)
}

func (s *Service) CreateAnonymous(ctx context.Context) (Session, string, error) {
	return s.create(ctx, AnonymousSession, uuid.Nil, 0)
}

func (s *Service) create(ctx context.Context, kind SessionKind, userID uuid.UUID, securityVersion int64) (Session, string, error) {
	if kind != AnonymousSession && (kind != AuthenticatedSession || userID == uuid.Nil || securityVersion < 1) {
		return Session{}, "", sessionFailure(problem.InvalidRequest)
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lease, err := platformdb.Acquire(bounded, s.pool)
	if err != nil {
		return Session{}, "", sessionFailure(problem.PlatformUnavailable)
	}
	defer lease.Release()
	tx, err := lease.Begin(bounded)
	if err != nil {
		return Session{}, "", sessionFailure(problem.PlatformUnavailable)
	}
	defer rollbackAuth(tx)
	state, encoded, err := s.insertSession(bounded, tx, kind, userID, securityVersion)
	if err != nil {
		return Session{}, "", err
	}
	if err := tx.Commit(bounded); err != nil {
		return Session{}, "", sessionFailure(problem.PlatformUnavailable)
	}
	return state, encoded, nil
}

// insertSession creates a family inside the caller's transaction. Login uses
// it after revoking the anonymous/reauth family so neither state is published
// unless both changes commit.
func (s *Service) insertSession(ctx context.Context, tx pgx.Tx, kind SessionKind, userID uuid.UUID, securityVersion int64) (Session, string, error) {
	raw, encoded, err := newToken()
	if err != nil {
		return Session{}, "", err
	}
	id := uuid.New()
	sealed, err := s.secrets.SealSession(id, raw)
	if err != nil {
		return Session{}, "", sessionFailure(problem.InternalError)
	}
	var now time.Time
	if err := tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
		return Session{}, "", sessionFailure(problem.PlatformUnavailable)
	}
	lifetime := anonymousLifetime
	var dbUser any
	var dbVersion any
	if kind == AuthenticatedSession {
		lifetime, dbUser, dbVersion = authenticatedLifetime, userID, securityVersion
	}
	expires := now.Add(lifetime)
	hash := tokenHash(raw)
	if _, err := tx.Exec(ctx, `INSERT INTO databrain.sessions
	    (id,kind,user_id,security_version,current_token_hash,current_token_sealed,
	     created_at,last_seen_at,last_rotated_at,absolute_expires_at)
	    VALUES ($1,$2,$3,$4,$5,$6,$7,$7,$7,$8)`, id, kind, dbUser, dbVersion, hash[:], sealed, now, expires); err != nil {
		return Session{}, "", sessionFailure(problem.PlatformUnavailable)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO databrain.session_tokens(token_hash,session_id,valid_until)
	    VALUES ($1,$2,$3)`, hash[:], id, expires); err != nil {
		return Session{}, "", sessionFailure(problem.PlatformUnavailable)
	}
	state := Session{ID: id, Kind: kind, AbsoluteExpiresAt: expires.UTC(), CSRF: s.csrf(id)}
	if kind == AuthenticatedSession {
		state.Principal = access.Principal{UserID: userID, SessionID: id, SecurityVersion: securityVersion}
	}
	return state, encoded, nil
}

type sessionRow struct {
	id                                       uuid.UUID
	kind                                     SessionKind
	userID                                   pgtype.UUID
	sessionVersion, userVersion              pgtype.Int8
	currentHash, currentSealed               []byte
	lastSeen, lastRotated, absolute, validTo time.Time
	revoked                                  pgtype.Timestamptz
	reauth, userActive                       bool
	now                                      time.Time
}

func scanSession(row pgx.Row) (sessionRow, error) {
	var value sessionRow
	err := row.Scan(&value.id, &value.kind, &value.userID, &value.sessionVersion, &value.currentHash,
		&value.currentSealed, &value.lastSeen, &value.lastRotated, &value.absolute, &value.revoked,
		&value.reauth, &value.validTo, &value.userActive, &value.userVersion, &value.now)
	return value, err
}

func loadSession(ctx context.Context, tx pgx.Tx, hash [32]byte) (sessionRow, error) {
	// The lock query may wait across rotation/expiry. Read token, user and clock
	// again in a fresh READ COMMITTED snapshot after acquiring the family lock.
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT s.id FROM databrain.session_tokens st
	    JOIN databrain.sessions s ON s.id=st.session_id WHERE st.token_hash=$1 FOR UPDATE OF s`, hash[:]).Scan(&id); err != nil {
		return sessionRow{}, err
	}
	return scanSession(tx.QueryRow(ctx, `SELECT s.id,s.kind,s.user_id,s.security_version,
	    s.current_token_hash,s.current_token_sealed,s.last_seen_at,s.last_rotated_at,s.absolute_expires_at,
	    s.revoked_at,s.reauth_required,st.valid_until,coalesce(u.is_active,false),u.security_version,clock_timestamp()
	    FROM databrain.sessions s JOIN databrain.session_tokens st ON st.session_id=s.id
	    LEFT JOIN databrain.users u ON u.id=s.user_id WHERE s.id=$1 AND st.token_hash=$2`, id, hash[:]))
}

func (s *Service) sessionState(value sessionRow, allowReauth bool) (Session, error) {
	if value.revoked.Valid || !value.now.Before(value.absolute) || !value.now.Before(value.validTo) ||
		(value.kind != AnonymousSession && value.kind != AuthenticatedSession) {
		return Session{}, sessionFailure(problem.SessionInvalid)
	}
	if value.kind == AuthenticatedSession && (!value.userID.Valid || !value.sessionVersion.Valid ||
		!value.userVersion.Valid || !value.userActive || value.sessionVersion.Int64 != value.userVersion.Int64 ||
		!value.now.Before(value.lastSeen.Add(authenticatedIdle))) {
		return Session{}, sessionFailure(problem.SessionInvalid)
	}
	state := Session{ID: value.id, Kind: value.kind, ReauthRequired: value.reauth,
		AbsoluteExpiresAt: value.absolute.UTC(), CSRF: s.csrf(value.id)}
	if value.kind == AuthenticatedSession {
		state.Principal = access.Principal{UserID: uuid.UUID(value.userID.Bytes), SessionID: value.id,
			SecurityVersion: value.sessionVersion.Int64}
		if value.reauth && !allowReauth {
			return state, ErrReauthRequired
		}
	}
	return state, nil
}

// rotateSession requires the family row lock and never commits its caller's transaction.
func (s *Service) rotateSession(ctx context.Context, tx pgx.Tx, value sessionRow) (string, error) {
	raw, encoded, err := newToken()
	if err != nil {
		return "", err
	}
	hash := tokenHash(raw)
	sealed, err := s.secrets.SealSession(value.id, raw)
	if err != nil {
		return "", sessionFailure(problem.InternalError)
	}
	aliasUntil := value.now.Add(aliasGrace)
	if aliasUntil.After(value.absolute) {
		aliasUntil = value.absolute
	}
	if _, err := tx.Exec(ctx, `UPDATE databrain.session_tokens SET valid_until=LEAST(valid_until,$2)
	    WHERE token_hash=$1`, value.currentHash, aliasUntil); err != nil {
		return "", sessionFailure(problem.PlatformUnavailable)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO databrain.session_tokens(token_hash,session_id,valid_until)
	    VALUES ($1,$2,$3)`, hash[:], value.id, value.absolute); err != nil {
		return "", sessionFailure(problem.PlatformUnavailable)
	}
	if _, err := tx.Exec(ctx, `UPDATE databrain.sessions SET current_token_hash=$2,
	    current_token_sealed=$3,last_rotated_at=$4 WHERE id=$1`, value.id, hash[:], sealed, value.now); err != nil {
		return "", sessionFailure(problem.PlatformUnavailable)
	}
	return encoded, nil
}

// Validate checks a cookie token, serializes rotation on the family row, and
// returns a replacement token only for rotation or alias repair.
func (s *Service) Validate(ctx context.Context, encoded string, allowReauth bool) (Session, string, error) {
	raw, err := decodeToken(encoded)
	if err != nil {
		return Session{}, "", err
	}
	hash := tokenHash(raw)
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lease, err := platformdb.Acquire(bounded, s.pool)
	if err != nil {
		return Session{}, "", sessionFailure(problem.PlatformUnavailable)
	}
	defer lease.Release()
	tx, err := lease.Begin(bounded)
	if err != nil {
		return Session{}, "", sessionFailure(problem.PlatformUnavailable)
	}
	defer rollbackAuth(tx)
	value, err := loadSession(bounded, tx, hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, "", sessionFailure(problem.SessionInvalid)
	}
	if err != nil {
		return Session{}, "", sessionFailure(problem.PlatformUnavailable)
	}
	state, stateErr := s.sessionState(value, allowReauth)
	if stateErr != nil && !errors.Is(stateErr, ErrReauthRequired) {
		return Session{}, "", stateErr
	}

	replacement := ""
	current := bytes.Equal(hash[:], value.currentHash)
	if !current {
		canonical, err := s.secrets.OpenSession(value.id, value.currentSealed)
		canonicalHash := tokenHash(canonical)
		if err != nil || len(canonical) != 32 || !bytes.Equal(canonicalHash[:], value.currentHash) {
			return Session{}, "", sessionFailure(problem.PlatformUnavailable)
		}
		replacement = base64.RawURLEncoding.EncodeToString(canonical)
	} else if !value.now.Before(value.lastRotated.Add(rotationInterval)) {
		replacement, err = s.rotateSession(bounded, tx, value)
		if err != nil {
			return Session{}, "", err
		}
	}
	if value.kind == AuthenticatedSession {
		if _, err := tx.Exec(bounded, "UPDATE databrain.sessions SET last_seen_at=$2 WHERE id=$1", value.id, value.now); err != nil {
			return Session{}, "", sessionFailure(problem.PlatformUnavailable)
		}
	}
	if err := tx.Commit(bounded); err != nil {
		return Session{}, "", sessionFailure(problem.PlatformUnavailable)
	}
	return state, replacement, stateErr
}

func rollbackAuth(tx pgx.Tx) {
	cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = tx.Rollback(cleanup)
}
