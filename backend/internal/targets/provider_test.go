package targets

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"strings"
	"syscall"
	"testing"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/config"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func checkCode(t *testing.T, err error, code problem.Code) {
	t.Helper()
	var p *problem.Error
	if !errors.As(err, &p) || p.Code != code || p.Message != "" || strings.Contains(err.Error(), "SENSITIVE") {
		t.Fatalf("expected safe code %s", code)
	}
}

func TestTargetValidationAndConnectionPolicy(t *testing.T) {
	t.Setenv("PGHOST", "forbidden.example")
	t.Setenv("PGUSER", "forbidden-user")
	t.Setenv("PGPASSWORD", "SENSITIVE")
	t.Setenv("PGOPTIONS", "-c search_path=forbidden")
	t.Setenv("PGSSLMODE", "prefer")
	base, err := pgx.ParseConfig("host=localhost port=5432 dbname=unused user=unused password=unused sslmode=disable")
	if err != nil {
		t.Fatal("could not initialize synthetic driver config")
	}
	p := &Provider{base: base, settings: config.TargetSettings{AppEnv: "test"}, allowed: map[string]bool{"localhost": true, "127.0.0.1": true}}
	valid := CreateInput{Name: "  Example 猫  ", Host: "LOCALHOST.", Port: 5432, DatabaseName: "Case Sensitive'数据库", Username: "user options='x", Password: "  SENSITIVE猫  ", SSLMode: "verify-full"}
	got, err := p.validate(valid)
	if err != nil || got.Name != "Example 猫" || got.Host != "localhost" || got.Password != valid.Password || got.DatabaseName != valid.DatabaseName {
		t.Fatal("normalization changed credentials or case-sensitive database name")
	}
	cfg := p.connectionConfig(got, "databrain-probe", true)
	if cfg.User != got.Username || cfg.Password != got.Password || cfg.Database != got.DatabaseName || cfg.Host != got.Host ||
		cfg.TLSConfig == nil || cfg.TLSConfig.InsecureSkipVerify || cfg.TLSConfig.ServerName != "localhost" || cfg.TLSConfig.MinVersion != tls.VersionTLS12 || len(cfg.Fallbacks) != 0 ||
		cfg.RuntimeParams["search_path"] != "public" || cfg.RuntimeParams["default_transaction_read_only"] != "on" || cfg.RuntimeParams["statement_timeout"] != "3000" || cfg.RuntimeParams["options"] != "" ||
		cfg.DefaultQueryExecMode == pgx.QueryExecModeSimpleProtocol || strings.Contains(cfg.ConnString(), "SENSITIVE") {
		t.Fatal("connection fields, TLS, protocol or runtime policy were overridden or disclosed")
	}
	for _, mutate := range []func(*CreateInput){
		func(in *CreateInput) { in.Host = "http://SENSITIVE@localhost" },
		func(in *CreateInput) { in.Host = "not-allowed.example" },
		func(in *CreateInput) { in.Port = 0 },
		func(in *CreateInput) { in.Port = 65536 },
		func(in *CreateInput) { in.Name = "  " },
		func(in *CreateInput) { in.Name = strings.Repeat("猫", 81) },
		func(in *CreateInput) { in.DatabaseName = strings.Repeat("猫", 22) },
		func(in *CreateInput) { in.Username = "" },
		func(in *CreateInput) { in.Username = "SENSITIVE\x00" },
		func(in *CreateInput) { in.Password = "" },
		func(in *CreateInput) { in.Password = strings.Repeat("猫", 1025) },
		func(in *CreateInput) { in.Password = string([]byte{0xff}) },
		func(in *CreateInput) { in.SSLMode = "prefer" },
	} {
		input := valid
		mutate(&input)
		_, err := p.validate(input)
		checkCode(t, err, problem.ValidationFailed)
	}
	p.settings.AppEnv = "production"
	valid.SSLMode = "disable"
	_, err = p.validate(valid)
	checkCode(t, err, problem.ValidationFailed)
}

func TestSafeProbeReasons(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want ProbeReason
	}{
		{&pgconn.PgError{Code: "28P01", Message: "SENSITIVE"}, AuthenticationFailed},
		{&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, ConnectionRefused},
		{context.DeadlineExceeded, Timeout},
		{errors.New("TLS handshake SENSITIVE"), TLSFailed},
		{errors.New("SENSITIVE"), Other},
	} {
		if got := connectReason(context.Background(), tc.err); got != tc.want {
			t.Fatalf("wrong safe reason; want %s", tc.want)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if connectReason(ctx, errors.New("SENSITIVE")) != Timeout {
		t.Fatal("cancellation did not return a safe timeout reason")
	}
}

func TestSupportedServerVersion(t *testing.T) {
	for version, want := range map[int]bool{160012: false, 170011: true, 180006: true, 190000: false} {
		if got := supportedServerVersion(version); got != want {
			t.Fatalf("supportedServerVersion(%d)=%v want %v", version, got, want)
		}
	}
}
