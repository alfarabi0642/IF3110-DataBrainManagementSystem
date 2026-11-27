package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/config"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
)

func TestStartupRejectsInvalidAssetConfig(t *testing.T) {
	t.Setenv("APP_ENV", "development")
	t.Setenv("APP_ORIGIN", "http://localhost:8080")
	t.Setenv("HTTP_ADDR", "127.0.0.1:8080")
	t.Setenv("SPA_DIR", "")
	var logs bytes.Buffer
	if err := run(slog.New(slog.NewTextHandler(&logs, nil))); err == nil {
		t.Fatal("invalid asset configuration did not reject startup")
	}
	if !strings.Contains(logs.String(), "SPA_DIR") {
		t.Fatal("startup did not identify the invalid asset configuration")
	}
}

func TestProcessLockLossCancelsActiveRequests(t *testing.T) {
	for _, duringDrain := range []bool{false, true} {
		t.Run(map[bool]string{false: "serving", true: "draining"}[duringDrain], func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			lost := make(chan struct{})
			started, canceled, draining := make(chan struct{}), make(chan struct{}), make(chan struct{})
			server := newServer(config.Config{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(started)
				<-r.Context().Done()
				close(canceled)
			}))
			defer server.Close()
			server.RegisterOnShutdown(func() { close(draining) })
			stopped := make(chan error, 1)
			go func() { stopped <- serveHTTP(ctx, server, listener, lost) }()
			clientDone := make(chan struct{})
			go func() {
				defer close(clientDone)
				client := &http.Client{Timeout: 5 * time.Second}
				res, err := client.Get("http://" + listener.Addr().String())
				if err == nil {
					res.Body.Close()
				}
			}()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("request did not start")
			}
			if duringDrain {
				cancel()
				select {
				case <-draining:
				case <-time.After(5 * time.Second):
					t.Fatal("server did not begin draining")
				}
			}
			close(lost)
			select {
			case <-canceled:
			case <-time.After(5 * time.Second):
				t.Fatal("lock loss did not cancel the active request")
			}
			select {
			case err := <-stopped:
				if !errors.Is(err, platformdb.ErrLockLost) {
					t.Fatal("lock loss did not fail the server")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("lock loss did not stop the server")
			}
			conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
			if err == nil {
				conn.Close()
				t.Fatal("listener still accepts connections after lock loss")
			}
			<-clientDone
		})
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
	go func() { stopped <- serveHTTP(ctx, server, listener, nil) }()
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

func TestSessionCleanupStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	called := make(chan struct{}, 1)
	go func() {
		defer close(done)
		runSessionCleanup(ctx, time.Millisecond, func(context.Context) error {
			select {
			case called <- struct{}{}:
			default:
			}
			return nil
		}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("session cleanup did not run")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("session cleanup did not stop")
	}
}
