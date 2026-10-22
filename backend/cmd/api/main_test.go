package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/config"
)

func TestStartupRejectsInvalidTemplates(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("APP_ENV", "development")
	t.Setenv("APP_ORIGIN", "http://localhost:8080")
	t.Setenv("HTTP_ADDR", "127.0.0.1:8080")
	templates := filepath.Join(dir, "web", "templates")
	if err := os.MkdirAll(templates, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(templates, "page.html"), []byte(`{{SENSITIVE`), 0600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	if err := run(slog.New(slog.NewTextHandler(&logs, nil))); err == nil {
		t.Fatal("invalid template did not reject startup")
	}
	if strings.Contains(logs.String(), "SENSITIVE") || strings.Contains(logs.String(), dir) {
		t.Fatal("template error leaked source/path in logs")
	}
}

func TestServerTimeouts(t *testing.T) {
	server := newServer(config.Config{HTTPAddr: "127.0.0.1:8080"}, http.NotFoundHandler())
	if server.ReadHeaderTimeout != 5*time.Second || server.ReadTimeout != 10*time.Second ||
		server.WriteTimeout != 75*time.Second || server.IdleTimeout != 60*time.Second {
		t.Fatal("HTTP timeouts differ from the operational contract")
	}
}

func TestServerGracefulShutdown(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := newServer(config.Config{}, http.NotFoundHandler())
	defer server.Close()

	started, release, draining := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	server.RegisterOnShutdown(func() { close(draining) })
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-release:
			io.WriteString(w, "finished")
		case <-r.Context().Done():
			t.Error("shutdown canceled an active request before the grace period")
		}
	})
	stopped := make(chan error, 1)
	go func() { stopped <- serveHTTP(ctx, server, listener) }()
	response := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: 5 * time.Second}
		res, err := client.Get("http://" + listener.Addr().String())
		if err == nil {
			defer res.Body.Close()
			var body []byte
			body, err = io.ReadAll(res.Body)
			if err == nil && (res.StatusCode != http.StatusOK || string(body) != "finished") {
				t.Errorf("active request = %d %q", res.StatusCode, body)
			}
		}
		response <- err
	}()
	select {
	case <-started:
	case err := <-response:
		t.Fatalf("request failed before reaching the handler: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case <-draining:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not begin shutdown")
	}
	select {
	case err := <-stopped:
		t.Fatalf("shutdown returned before the active request finished: %v", err)
	default:
	}
	unblock()
	select {
	case err := <-response:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("active request did not finish")
	}
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not finish")
	}
	conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err == nil {
		conn.Close()
		t.Fatal("listener still accepts connections after shutdown")
	}
}
