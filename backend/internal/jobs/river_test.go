package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/config"
)

func TestExecutionJobArgsValidate(t *testing.T) {
	valid := ExecutionJobArgs{Version: PayloadVersion, ExecutionID: uuid.New(), RequestID: uuid.New()}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name string
		args ExecutionJobArgs
		text string
	}{
		{"missing version", ExecutionJobArgs{ExecutionID: uuid.New(), RequestID: uuid.New()}, "version"},
		{"unsupported version", ExecutionJobArgs{Version: PayloadVersion + 1, ExecutionID: uuid.New(), RequestID: uuid.New()}, "version"},
		{"missing execution", ExecutionJobArgs{Version: PayloadVersion, RequestID: uuid.New()}, "execution ID"},
		{"missing request", ExecutionJobArgs{Version: PayloadVersion, ExecutionID: uuid.New()}, "request ID"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.args.Validate()
			if !errors.Is(err, ErrPayload) || !strings.Contains(err.Error(), tt.text) {
				t.Fatalf("invalid payload accepted or misidentified: %v", err)
			}
		})
	}
}

func TestReferenceJobArgsValidate(t *testing.T) {
	if err := (ReferenceArgs{Version: PayloadVersion, ReferenceID: uuid.New()}).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, args := range []ReferenceArgs{
		{ReferenceID: uuid.New()},
		{Version: PayloadVersion + 1, ReferenceID: uuid.New()},
		{Version: PayloadVersion},
	} {
		if err := args.Validate(); !errors.Is(err, ErrPayload) {
			t.Fatalf("invalid reference payload accepted: %v", err)
		}
	}
}

func TestExecutionJobArgsSerializeReferencesOnly(t *testing.T) {
	payload, err := json.Marshal(ExecutionJobArgs{Version: PayloadVersion, ExecutionID: uuid.New(), RequestID: uuid.New()})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 3 || fields["version"] == nil || fields["execution_id"] == nil || fields["request_id"] == nil {
		t.Fatalf("payload exposes fields beyond execution references: %s", payload)
	}
	if strings.Contains(strings.ToLower(string(payload)), "sql") || strings.Contains(string(payload), "SENSITIVE") {
		t.Fatalf("payload contains non-reference data: %s", payload)
	}
}

func TestEnqueuerCannotConsume(t *testing.T) {
	enqueuer, err := NewEnqueuer()
	if err != nil {
		t.Fatal(err)
	}
	if err := enqueuer.client.Start(context.Background()); err == nil {
		t.Fatal("enqueue-only client started a consumer")
	}
}

func TestNewWorkerRejectsInvalidInputs(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	workers := river.NewWorkers()
	river.AddWorker(workers, &ReferenceWorker{})
	valid := config.WorkerSettings{Concurrency: 1, JobTimeout: time.Minute, DrainTimeout: time.Second}
	for _, tt := range []struct {
		name     string
		settings config.WorkerSettings
		workers  *river.Workers
		logger   *slog.Logger
	}{
		{"no workers", valid, nil, logger},
		{"no logger", valid, workers, nil},
		{"zero concurrency", config.WorkerSettings{JobTimeout: time.Minute, DrainTimeout: time.Second}, workers, logger},
		{"excessive concurrency", config.WorkerSettings{Concurrency: 65, JobTimeout: time.Minute, DrainTimeout: time.Second}, workers, logger},
		{"missing job timeout", config.WorkerSettings{Concurrency: 1, DrainTimeout: time.Second}, workers, logger},
		{"missing drain timeout", config.WorkerSettings{Concurrency: 1, JobTimeout: time.Minute}, workers, logger},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if client, err := NewWorker(nil, tt.settings, tt.workers, tt.logger); err == nil || client != nil {
				t.Fatal("invalid worker construction was accepted")
			}
		})
	}
}

func TestReadyRejectsMissingPool(t *testing.T) {
	if Ready(context.Background(), nil) {
		t.Fatal("missing pool reported ready")
	}
}
