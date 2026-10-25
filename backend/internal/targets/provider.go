// Package targets menyimpan konfigurasi target dan membuka koneksi khusus. Pemanggil wajib
// mengotorisasi akses sebelum memakai Provider langsung.
package targets

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"os"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/config"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/secret"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type ProbeReason string

const (
	AuthenticationFailed    ProbeReason = "authentication_failed"
	ConnectionRefused       ProbeReason = "connection_refused"
	Timeout                 ProbeReason = "timeout"
	TLSFailed               ProbeReason = "tls_failed"
	UnsupportedVersion      ProbeReason = "unsupported_version"
	UnsafeRole              ProbeReason = "unsafe_role"
	PlatformTargetForbidden ProbeReason = "platform_target_forbidden"
	Other                   ProbeReason = "other"
)

type ProbeResult struct {
	OK            bool
	Reason        ProbeReason
	ServerVersion int
	Latency       time.Duration
}

type Provider struct {
	pool      *pgxpool.Pool
	secrets   *secret.Secrets
	settings  config.TargetSettings
	allowed   map[string]bool
	roots     *x509.CertPool
	base      *pgx.ConnConfig
	platform  *pgx.ConnConfig
	explorer  chan struct{}
	migration chan struct{}
}

// NewProvider memvalidasi policy tanpa membuka target atau mengambil process lock. Satu Provider per
// server berbagi pool platform yang ada.
func NewProvider(pool *pgxpool.Pool, secrets *secret.Secrets, settings config.TargetSettings) (*Provider, error) {
	if pool == nil || secrets == nil || len(settings.AllowedHosts) == 0 ||
		(settings.AppEnv != "development" && settings.AppEnv != "test" && settings.AppEnv != "production") {
		return nil, failure(problem.ValidationFailed)
	}
	p := &Provider{pool: pool, secrets: secrets, settings: settings, allowed: make(map[string]bool),
		platform: pool.Config().ConnConfig, explorer: make(chan struct{}, 4), migration: make(chan struct{}, 2)}
	for _, host := range settings.AllowedHosts {
		normalized, err := config.NormalizeTargetHost(host)
		if err != nil {
			return nil, failure(problem.ValidationFailed)
		}
		p.allowed[normalized] = true
	}
	if settings.CAFile != "" {
		pem, err := os.ReadFile(settings.CAFile)
		p.roots = x509.NewCertPool()
		if err != nil || !p.roots.AppendCertsFromPEM(pem) {
			return nil, failure(problem.ValidationFailed)
		}
	}
	// Parse string tetap untuk inisialisasi internal pgx. Field target diisi langsung, tanpa interpolasi
	// DSN.
	base, err := pgx.ParseConfig("host=localhost port=5432 dbname=unused user=unused password=unused sslmode=disable connect_timeout=5")
	if err != nil {
		return nil, failure(problem.ValidationFailed)
	}
	p.base = base
	return p, nil
}

// connectionConfig menimpa konfigurasi libpq dari environment/service file, termasuk opsi runtime,
// credential, fallback TLS, dan role.
func (p *Provider) connectionConfig(in CreateInput, application string, readOnly bool) *pgx.ConnConfig {
	cfg := p.base.Copy()
	cfg.Host, cfg.Port, cfg.Database, cfg.User, cfg.Password = in.Host, uint16(in.Port), in.DatabaseName, in.Username, in.Password
	cfg.Fallbacks, cfg.TLSConfig, cfg.ValidateConnect = nil, nil, nil
	cfg.SSLNegotiation = "postgres"
	if in.SSLMode == "verify-full" {
		cfg.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, ServerName: in.Host, RootCAs: p.roots}
	}
	cfg.ConnectTimeout = 5 * time.Second
	cfg.DialFunc = (&net.Dialer{Timeout: 5 * time.Second}).DialContext
	cfg.LookupFunc = net.DefaultResolver.LookupHost
	cfg.RuntimeParams = map[string]string{
		"application_name": application, "search_path": "public", "TimeZone": "UTC",
		"statement_timeout": "30000", "lock_timeout": "3000", "default_transaction_read_only": "off",
	}
	if readOnly {
		cfg.RuntimeParams["statement_timeout"], cfg.RuntimeParams["lock_timeout"] = "3000", "1000"
		cfg.RuntimeParams["default_transaction_read_only"] = "on"
	}
	return cfg
}

func (p *Provider) forbidden(ctx context.Context, in CreateInput) (bool, error) {
	if in.Username == p.platform.User {
		return true, nil
	}
	if in.DatabaseName != p.platform.Database || in.Port != int(p.platform.Port) {
		return false, nil
	}
	platformHost, err := config.NormalizeTargetHost(p.platform.Host)
	if err != nil {
		return true, nil // Pemisahan dari endpoint platform tidak dapat dipastikan.
	}
	if in.Host == platformHost {
		return true, nil
	}
	left, err := net.DefaultResolver.LookupHost(ctx, in.Host)
	if err != nil {
		return false, err
	}
	right, err := net.DefaultResolver.LookupHost(ctx, platformHost)
	if err != nil {
		return false, err
	}
	for _, a := range left {
		for _, b := range right {
			if net.ParseIP(a).Equal(net.ParseIP(b)) {
				return true, nil
			}
		}
	}
	return false, nil
}

func connectReason(ctx context.Context, err error) ProbeReason {
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || pgconn.Timeout(err) {
		return Timeout
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == "28P01" || pgErr.Code == "28000") {
		return AuthenticationFailed
	}
	// Windows memakai kode Winsock WSAECONNREFUSED, berbeda dari konstanta kompatibilitas POSIX
	// syscall.ECONNREFUSED.
	if errors.Is(err, syscall.ECONNREFUSED) || runtime.GOOS == "windows" && errors.Is(err, syscall.Errno(10061)) {
		return ConnectionRefused
	}
	// Teks driver hanya dipakai untuk klasifikasi; jangan dikembalikan, dibungkus, atau dicatat.
	if strings.Contains(strings.ToLower(err.Error()), "tls") {
		return TLSFailed
	}
	return Other
}

func (p *Provider) connect(ctx context.Context, in CreateInput, application string, readOnly bool) (*pgx.Conn, ProbeResult) {
	started := time.Now()
	result := ProbeResult{}
	finish := func(reason ProbeReason) (*pgx.Conn, ProbeResult) {
		result.Reason, result.Latency = reason, time.Since(started)
		return nil, result
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	forbidden, err := p.forbidden(bounded, in)
	if err != nil {
		return finish(connectReason(bounded, err))
	}
	if forbidden {
		return finish(PlatformTargetForbidden)
	}
	conn, err := pgx.ConnectConfig(bounded, p.connectionConfig(in, application, readOnly))
	if err != nil {
		return finish(connectReason(bounded, err))
	}
	var database, role string
	var unsafe bool
	err = conn.QueryRow(bounded, `SELECT current_database(),current_user,current_setting('server_version_num')::int,
	    rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls
	    FROM pg_catalog.pg_roles WHERE rolname=current_user`).Scan(&database, &role, &result.ServerVersion, &unsafe)
	if err != nil {
		_ = conn.Close(bounded)
		return finish(connectReason(bounded, err))
	}
	if database != in.DatabaseName || role != in.Username {
		_ = conn.Close(bounded)
		return finish(Other)
	}
	if !supportedServerVersion(result.ServerVersion) {
		_ = conn.Close(bounded)
		return finish(UnsupportedVersion)
	}
	if unsafe {
		_ = conn.Close(bounded)
		return finish(UnsafeRole)
	}
	result.OK, result.Latency = true, time.Since(started)
	return conn, result
}

func supportedServerVersion(version int) bool {
	major := version / 10000
	return major == 17 || major == 18
}

// Probe membuka koneksi usulan maksimal lima detik lalu menutupnya. Kegagalan hanya memuat alasan yang
// diizinkan dan kode aman.
func (p *Provider) Probe(ctx context.Context, input CreateInput) (ProbeResult, error) {
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	in, err := p.validate(input)
	if err != nil {
		return ProbeResult{}, err
	}
	conn, result := p.connect(bounded, in, "databrain-probe", true)
	if conn == nil {
		return result, failure(problem.TargetConnectionFailed)
	}
	_ = conn.Close(bounded)
	return result, nil
}

// Test memeriksa credential tersimpan termasuk target nonaktif tanpa mutasi. Kegagalan koneksi adalah
// hasil probe; kegagalan lookup platform tetap error.
func (p *Provider) Test(ctx context.Context, targetID uuid.UUID) (ProbeResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	stored, err := p.load(ctx, targetID)
	if err != nil {
		return ProbeResult{}, err
	}
	in, err := p.credentials(stored)
	if err != nil {
		return ProbeResult{}, err
	}
	result, err := p.Probe(ctx, in)
	if err != nil && !isCode(err, problem.TargetConnectionFailed) {
		return result, err
	}
	return result, nil
}

func isCode(err error, code problem.Code) bool {
	var p *problem.Error
	return errors.As(err, &p) && p.Code == code
}

func (p *Provider) OpenExplorer(ctx context.Context, targetID uuid.UUID) (*pgx.Conn, error) {
	return p.open(ctx, targetID, 0, uuid.Nil, true)
}

// OpenMigration memerlukan revision konfigurasi yang disimpan runner. Fungsi ini tidak mengotorisasi,
// mereservasi, mengklaim, atau menjalankan attempt.
func (p *Provider) OpenMigration(ctx context.Context, targetID uuid.UUID, configRevision int64, attemptID uuid.UUID) (*pgx.Conn, error) {
	if configRevision < 1 || attemptID == uuid.Nil {
		return nil, failure(problem.InvalidRequest)
	}
	return p.open(ctx, targetID, configRevision, attemptID, false)
}

func (p *Provider) open(ctx context.Context, targetID uuid.UUID, revision int64, attemptID uuid.UUID, readOnly bool) (*pgx.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if ctx.Err() != nil {
		return nil, failure(problem.TargetUnavailable)
	}
	slots, capacityCode := p.explorer, problem.TargetUnavailable
	if !readOnly {
		slots, capacityCode = p.migration, problem.ExecutionCapacity
	}
	select {
	case slots <- struct{}{}:
	default:
		return nil, failure(capacityCode)
	}
	release := true
	defer func() {
		if release {
			<-slots
		}
	}()
	stored, err := p.load(ctx, targetID)
	if err != nil {
		return nil, err
	}
	if !stored.IsActive {
		return nil, failure(problem.DatabaseNotFound)
	}
	if !readOnly && stored.ConfigRevision != revision {
		return nil, failure(problem.RevisionConflict)
	}
	in, err := p.credentials(stored)
	if err != nil {
		return nil, err
	}
	application := "databrain-explorer"
	if !readOnly {
		application = "databrain:" + attemptID.String()
	}
	conn, _ := p.connect(ctx, in, application, readOnly)
	if conn == nil {
		return nil, failure(problem.TargetUnavailable)
	}
	release = false
	// API mengembalikan *pgx.Conn. Kapasitas baru dilepas setelah semua resource pgx tertutup, termasuk
	// cleanup pembatalan asinkron.
	go func() { <-conn.PgConn().CleanupDone(); <-slots }()
	return conn, nil
}
