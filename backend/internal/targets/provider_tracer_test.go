package targets

import (
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestTargetConnectionConfigInstallsQueryTracer(t *testing.T) {
	base, err := pgx.ParseConfig("host=localhost port=5432 dbname=unused user=unused password=unused sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	provider := &Provider{base: base}
	cfg := provider.connectionConfig(CreateInput{Host: "127.0.0.1", Port: 5432, DatabaseName: "db",
		Username: "role", Password: "secret"}, "databrain-probe", true)
	if cfg.Tracer == nil {
		t.Fatal("target connection has no tracer")
	}
}
