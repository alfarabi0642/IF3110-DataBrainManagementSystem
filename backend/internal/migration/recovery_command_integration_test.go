package migration

import (
	"bytes"
	"context"
	"encoding/base64"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestRecoveryStartupAndCommandPostgres(t *testing.T) {
	f, s := newAttemptFixture(t)
	ctx := context.Background()
	d := attemptDraft(t, f, f.a, `CREATE TABLE public.startup_must_not_run(id int)`)
	r := mustReserve(t, s, f, d, ReserveInput{1, uuid.New()})
	d2 := attemptDraft(t, f, f.b, d.SQL)
	r2 := mustReserve(t, s, f, d2, ReserveInput{1, uuid.New()})
	if ok, err := s.Claim(ctx, r2); err != nil || !ok {
		t.Fatal("cannot initialize running startup orphan")
	}
	cfg := f.target.Config().Copy()
	cfg.Database = f.db.Pool.Config().ConnConfig.Database
	owner, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal("cannot inspect startup/command fixture")
	}
	defer closeDraftTestConn(owner)
	runtimeURL := f.db.Pool.Config().ConnString()
	module, _ := filepath.Abs("../..")
	dir := t.TempDir()
	api, manage := filepath.Join(dir, "api.exe"), filepath.Join(dir, "manage.exe")
	for target, pkg := range map[string]string{api: "./cmd/api", manage: "./cmd/manage"} {
		build := exec.Command("go", "build", "-o", target, pkg)
		build.Dir = module
		if err := build.Run(); err != nil {
			t.Fatal("cannot build actual startup/maintenance commands")
		}
	}
	if err := f.db.Close(ctx); err != nil {
		t.Fatal("cannot stop fixture's old process")
	}
	port, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("cannot reserve startup test address")
	}
	address := port.Addr().String()
	_ = port.Close()
	env := append(os.Environ(), "APP_ENV=test", "PLATFORM_DATABASE_URL="+runtimeURL,
		"HTTP_ADDR="+address, "APP_ORIGIN=http://"+address, "TARGET_ALLOWED_HOSTS=127.0.0.1",
		"APP_SECRET_KEY="+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x84}, 32)))
	server := exec.Command(api)
	server.Dir, server.Env = module, env
	if err := server.Start(); err != nil {
		t.Fatal("cannot start actual API process")
	}
	waited := false
	defer func() {
		if !waited {
			_ = server.Process.Kill()
			_ = server.Wait()
		}
	}()
	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()
	deadline := time.Now().Add(10 * time.Second)
	for {
		response, err := client.Get("http://" + address + "/healthz")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("API never became ready after recovery")
		}
		time.Sleep(20 * time.Millisecond)
	}
	var unknown int
	if err := owner.QueryRow(ctx, `SELECT count(*) FROM databrain.migration_attempts WHERE state='unknown' AND first_unknown_at IS NOT NULL`).Scan(&unknown); err != nil || unknown != 2 {
		t.Fatal("API accepted traffic before recovering both orphan states")
	}
	secondCtx, stopSecond := context.WithTimeout(ctx, 10*time.Second)
	defer stopSecond()
	secondPort, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("cannot reserve second server address")
	}
	secondAddress := secondPort.Addr().String()
	_ = secondPort.Close()
	second := exec.CommandContext(secondCtx, api)
	second.Dir = module
	second.Env = append(append([]string{}, env...), "HTTP_ADDR="+secondAddress)
	if output, err := second.CombinedOutput(); err == nil || secondCtx.Err() != nil || !strings.Contains(string(output), "another server holds the platform process lock") {
		t.Fatal("second API process was not rejected by the process lock")
	}
	evidence := filepath.Join(dir, "evidence.txt")
	const private = "PRIVATE_OPERATOR_EVIDENCE\r\nTest reservation and target inspection confirm no SQL started.\n"
	if err := os.WriteFile(evidence, []byte(private), 0600); err != nil {
		t.Fatal(err)
	}
	command := func() (int, string) {
		cmd := exec.Command(manage, "resolve-attempt", "--attempt", r.Receipt.ID.String(), "--outcome", "failed", "--operator", "PRIVATE_OPERATOR", "--evidence-file", evidence)
		cmd.Dir, cmd.Env = module, env
		output, err := cmd.CombinedOutput()
		if err == nil {
			return 0, string(output)
		}
		if exit, ok := err.(*exec.ExitError); ok {
			return exit.ExitCode(), string(output)
		}
		t.Fatal("maintenance command could not execute")
		return -1, ""
	}
	if code, out := command(); code != 1 || !strings.Contains(out, "process lock") || strings.Contains(out, "PRIVATE") {
		t.Fatal("maintenance did not refuse a live server safely")
	}
	_ = server.Process.Kill()
	_ = server.Wait()
	waited = true
	if code, out := command(); code != 0 || !strings.Contains(out, "resolved as failed") || strings.Contains(out, "PRIVATE") {
		t.Fatal("maintenance command did not record evidence safely")
	}
	if code, out := command(); code != 1 || strings.Contains(out, "PRIVATE") {
		t.Fatal("maintenance command accepted a terminal attempt or exposed evidence")
	}
	var state, saved string
	var idle, absent bool
	if err := owner.QueryRow(ctx, `SELECT a.state,a.resolution_reason,s.active_attempt_id IS NULL FROM databrain.migration_attempts a JOIN databrain.migration_streams s ON s.target_id=a.target_id WHERE a.id=$1`, r.Receipt.ID).Scan(&state, &saved, &idle); err != nil || state != "failed" || saved != private || !idle {
		t.Fatal("command outcome/evidence/stream was not persisted")
	}
	if err := f.target.QueryRow(ctx, `SELECT to_regclass('public.startup_must_not_run') IS NULL`).Scan(&absent); err != nil || !absent {
		t.Fatal("startup or CLI replayed target SQL")
	}
	// State blokir permanen yang tidak konsisten harus menggagalkan startup sebelum HTTP dibuka.
	runnerSQL(t, owner, `UPDATE databrain.migration_streams SET active_attempt_id=NULL WHERE target_id=$1`, f.b)
	startup, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	broken := exec.CommandContext(startup, api)
	broken.Dir, broken.Env = module, env
	output, err := broken.CombinedOutput()
	if err == nil || startup.Err() != nil || !strings.Contains(string(output), "migration startup recovery failed") || strings.Contains(string(output), "PRIVATE") {
		t.Fatal("failed recovery did not stop startup safely")
	}
	if c, err := net.DialTimeout("tcp", address, time.Second); err == nil {
		_ = c.Close()
		t.Fatal("failed recovery opened an HTTP listener")
	}
}
