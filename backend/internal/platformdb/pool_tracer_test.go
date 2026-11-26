package platformdb

import "testing"

func TestRuntimePoolConfigInstallsQueryTracer(t *testing.T) {
	cfg, err := runtimePoolConfig("postgres://runtime:secret@127.0.0.1:1/db?sslmode=disable")
	if err != nil || cfg == nil || cfg.ConnConfig.Tracer == nil {
		t.Fatalf("platform runtime pool has no tracer: %v", err)
	}
}
