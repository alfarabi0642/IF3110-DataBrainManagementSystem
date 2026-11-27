package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeRiverClient struct {
	stopped chan struct{}
	forced  chan struct{}

	mu    sync.Mutex
	calls int
	err   error
}

func (f *fakeRiverClient) Stopped() <-chan struct{} { return f.stopped }

func (f *fakeRiverClient) StopAndCancel(context.Context) error {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	select {
	case <-f.forced:
	default:
		close(f.forced)
	}
	return f.err
}

func TestDrainWorkerReturnsAfterSoftStop(t *testing.T) {
	client := &fakeRiverClient{stopped: make(chan struct{}), forced: make(chan struct{})}
	close(client.stopped)
	if err := drainWorker(client, time.Second); err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.calls != 0 {
		t.Fatal("drain forced a stop after the client had already stopped")
	}
}

func TestDrainWorkerForcesStopAfterDeadline(t *testing.T) {
	client := &fakeRiverClient{stopped: make(chan struct{}), forced: make(chan struct{})}
	started := time.Now()
	if err := drainWorker(client, 20*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 20*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("drain ignored its deadline: %s", elapsed)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.calls != 1 {
		t.Fatal("drain did not force the client to stop")
	}
}

func TestDrainWorkerKeepsForcedStopError(t *testing.T) {
	want := errors.New("SENSITIVE")
	client := &fakeRiverClient{stopped: make(chan struct{}), forced: make(chan struct{}), err: want}
	if err := drainWorker(client, time.Millisecond); !errors.Is(err, want) {
		t.Fatalf("forced stop error was lost: %v", err)
	}
}

func TestRunRejectsInvalidWorkerConfigBeforeDatabase(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKER_CONCURRENCY", "0")
	var logs bytes.Buffer
	if err := run(slog.New(slog.NewTextHandler(&logs, nil))); err == nil {
		t.Fatal("invalid worker limits did not reject startup")
	}
	if !strings.Contains(logs.String(), "WORKER_CONCURRENCY") {
		t.Fatal("startup did not identify the invalid worker limit")
	}
}

func TestRunRejectsInvalidPlatformConfigWithoutSecrets(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("PLATFORM_DATABASE_URL", "postgres://runtime:SENSITIVE@localhost:0/platform")
	var logs bytes.Buffer
	if err := run(slog.New(slog.NewTextHandler(&logs, nil))); err == nil {
		t.Fatal("invalid platform URL did not reject startup")
	}
	if !strings.Contains(logs.String(), "PLATFORM_DATABASE_URL") || strings.Contains(logs.String(), "SENSITIVE") {
		t.Fatal("platform configuration failure was missing or disclosed its value")
	}
}
