// Package jobs mengintegrasikan antrean River pada database platform. API hanya menulis job ke
// transaksi caller, sedangkan worker berjalan sebagai proses terpisah.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/config"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const (
	// PayloadVersion adalah versi payload yang didukung build ini.
	PayloadVersion = 1

	// schema adalah schema platform tempat tabel River berada; migration 000001 membuatnya dan
	// pool runtime memakai search_path pg_catalog,databrain.
	schema = "databrain"

	executionKind = "migration_execution"
	referenceKind = "jobs_reference"
)

// ErrPayload menandai payload job yang tidak sah. Payload divalidasi sebelum SQL dijalankan.
var ErrPayload = errors.New("invalid job payload")

// ExecutionJobArgs hanya membawa referensi eksekusi dan header W3C trace context non-rahasia.
// SQL, kredensial, sesi dan data target tidak pernah masuk antrean.
type ExecutionJobArgs struct {
	Version     int       `json:"version"`
	ExecutionID uuid.UUID `json:"execution_id"`
	RequestID   uuid.UUID `json:"request_id"`
	TraceParent string    `json:"traceparent,omitempty"`
	TraceState  string    `json:"tracestate,omitempty"`
}

func (ExecutionJobArgs) Kind() string { return executionKind }

func (a ExecutionJobArgs) Validate() error {
	switch {
	case a.Version != PayloadVersion:
		return fmt.Errorf("%w: version must be %d, got %d", ErrPayload, PayloadVersion, a.Version)
	case a.ExecutionID == uuid.Nil:
		return fmt.Errorf("%w: execution ID must not be empty", ErrPayload)
	case a.RequestID == uuid.Nil:
		return fmt.Errorf("%w: request ID must not be empty", ErrPayload)
	}
	return nil
}

// ReferenceArgs adalah job referensi untuk menguji enqueue, konsumsi, drain, dan persistensi
// melalui proses worker nyata secara terisolasi.
type ReferenceArgs struct {
	Version     int       `json:"version"`
	ReferenceID uuid.UUID `json:"reference_id"`
}

func (ReferenceArgs) Kind() string { return referenceKind }

func (a ReferenceArgs) Validate() error {
	switch {
	case a.Version != PayloadVersion:
		return fmt.Errorf("%w: version must be %d, got %d", ErrPayload, PayloadVersion, a.Version)
	case a.ReferenceID == uuid.Nil:
		return fmt.Errorf("%w: reference ID must not be empty", ErrPayload)
	}
	return nil
}

// ReferenceWorker satu-satunya kind yang terdaftar pada tahap fondasi. Executor migration produksi
// belum ada, sehingga worker tidak boleh mengambil job migration yang belum dapat dijalankan.
type ReferenceWorker struct {
	river.WorkerDefaults[ReferenceArgs]
}

func (w *ReferenceWorker) Work(_ context.Context, job *river.Job[ReferenceArgs]) error {
	if err := job.Args.Validate(); err != nil {
		// Payload invalid bersifat permanen; mengulang tidak akan membuatnya sah.
		return river.JobCancel(err)
	}
	return nil
}

// SnoozeError meminta River menjadwalkan ulang job tanpa menghitungnya sebagai kegagalan SQL
// (misalnya saat lock kepemilikan target sedang dipegang worker lain).
type SnoozeError struct {
	Duration time.Duration
	Reason   string
}

func (e SnoozeError) Error() string {
	if e.Reason == "" {
		return "job snoozed"
	}
	return e.Reason
}

// PermanentError menandai kegagalan permanen atau penolakan kebijakan yang tidak boleh diulang.
type PermanentError struct {
	Err error
}

func (e PermanentError) Error() string {
	if e.Err == nil {
		return "permanent job failure"
	}
	return e.Err.Error()
}

func (e PermanentError) Unwrap() error { return e.Err }

// ExecutionHandler memproses satu job migration_execution. Diinjeksikan pada cmd/worker untuk
// menghindari import cycle antara internal/migration dan internal/jobs.
type ExecutionHandler func(ctx context.Context, args ExecutionJobArgs, jobID int64) error

// ExecutionRetryDelays menentukan jeda backoff terbatas untuk retry kegagalan sementara (1s, 5s, 15s, 60s).
var ExecutionRetryDelays = []time.Duration{
	1 * time.Second,
	5 * time.Second,
	15 * time.Second,
	60 * time.Second,
}

const ExecutionMaxAttempts = 5

func (ExecutionJobArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: ExecutionMaxAttempts}
}

// ExecutionWorker menjalankan job migration_execution melalui handler yang diinjeksikan.
type ExecutionWorker struct {
	river.WorkerDefaults[ExecutionJobArgs]
	handler ExecutionHandler
}

func (w *ExecutionWorker) NextRetry(job *river.Job[ExecutionJobArgs]) time.Time {
	idx := job.Attempt - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(ExecutionRetryDelays) {
		idx = len(ExecutionRetryDelays) - 1
	}
	return time.Now().UTC().Add(ExecutionRetryDelays[idx])
}

func (w *ExecutionWorker) Work(ctx context.Context, job *river.Job[ExecutionJobArgs]) error {
	if err := job.Args.Validate(); err != nil {
		return river.JobCancel(err)
	}
	if w.handler == nil {
		return river.JobCancel(errors.New("execution handler is not configured"))
	}
	ctx = telemetry.ExtractTraceContext(ctx, job.Args.TraceParent, job.Args.TraceState)
	ctx, span := telemetry.Tracer().Start(ctx, "river.consume",
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String(telemetry.FieldExecutionID, job.Args.ExecutionID.String()),
			attribute.String(telemetry.FieldRequestID, job.Args.RequestID.String()),
			attribute.Int64(telemetry.FieldJobID, job.ID),
			attribute.Int(telemetry.FieldAttempt, job.Attempt),
		),
	)
	defer span.End()

	err := w.handler(ctx, job.Args, job.ID)
	if err == nil {
		span.SetAttributes(attribute.String(telemetry.FieldStatus, "ok"))
		return nil
	}
	var snooze SnoozeError
	if errors.As(err, &snooze) {
		span.SetAttributes(attribute.String(telemetry.FieldStatus, "snoozed"))
		d := snooze.Duration
		if d <= 0 {
			d = 5 * time.Second
		}
		return river.JobSnooze(d)
	}
	var perm PermanentError
	if errors.As(err, &perm) {
		span.SetAttributes(attribute.String(telemetry.FieldStatus, "permanent_error"))
		return river.JobCancel(perm)
	}
	span.SetAttributes(attribute.String(telemetry.FieldStatus, "retryable_error"))
	return err
}

// NewWorkers membuat registry worker fondasi (hanya ReferenceWorker).
func NewWorkers() *river.Workers {
	workers := river.NewWorkers()
	river.AddWorker(workers, &ReferenceWorker{})
	return workers
}

// NewWorkersWithExecution mendaftarkan ReferenceWorker dan ExecutionWorker produksi.
func NewWorkersWithExecution(handler ExecutionHandler) *river.Workers {
	workers := NewWorkers()
	if handler != nil {
		river.AddWorker(workers, &ExecutionWorker{handler: handler})
	}
	return workers
}

// Enqueuer menulis job melalui transaksi milik caller. Client ini tidak memiliki pool sendiri dan
// tidak pernah mulai sebagai consumer.
type Enqueuer struct {
	client *river.Client[pgx.Tx]
}

// NewEnqueuer membuat client enqueue-only. InsertTx memakai transaksi caller, jadi tidak ada pool
// yang perlu dibuka atau ditutup oleh API. Schema disebut eksplisit agar penulisan tidak bergantung
// pada search_path koneksi caller.
func NewEnqueuer() (*Enqueuer, error) {
	client, err := river.NewClient(riverpgxv5.New(nil), &river.Config{Schema: schema})
	if err != nil {
		return nil, fmt.Errorf("cannot create enqueue-only queue client: %w", err)
	}
	return &Enqueuer{client: client}, nil
}

// Enqueue menambahkan job ke transaksi caller tanpa commit. Payload divalidasi sebelum InsertTx;
// caller yang membatalkan transaksi juga membatalkan job. Header W3C TraceContext disisipkan secara
// otomatis dari ctx aktif agar proses worker melanjutkan trace yang sama.
func (e *Enqueuer) Enqueue(ctx context.Context, tx pgx.Tx, args ExecutionJobArgs) (int64, error) {
	if tx == nil {
		return 0, errors.New("job enqueue requires a caller transaction")
	}
	if err := args.Validate(); err != nil {
		return 0, err
	}
	ctx, span := telemetry.Tracer().Start(ctx, "river.enqueue",
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(
			attribute.String(telemetry.FieldExecutionID, args.ExecutionID.String()),
			attribute.String(telemetry.FieldRequestID, args.RequestID.String()),
		),
	)
	defer span.End()

	if args.TraceParent == "" {
		tp, ts := telemetry.InjectTraceContext(ctx)
		args.TraceParent = tp
		args.TraceState = ts
	}
	result, err := e.client.InsertTx(ctx, tx, args, nil)
	if err != nil {
		span.SetAttributes(attribute.String(telemetry.FieldStatus, "error"))
		return 0, fmt.Errorf("enqueue execution job: %w", err)
	}
	if result == nil || result.Job == nil || result.Job.ID < 1 {
		span.SetAttributes(attribute.String(telemetry.FieldStatus, "error"))
		return 0, errors.New("enqueue execution job returned no job ID")
	}
	span.SetAttributes(
		attribute.Int64(telemetry.FieldJobID, result.Job.ID),
		attribute.String(telemetry.FieldStatus, "enqueued"),
	)
	return result.Job.ID, nil
}

// NewWorker membuat client consumer dengan batas runtime yang tervalidasi. Leader election aktif
// agar scheduler retry/snooze, penyelamatan stuck job (JobRescuer), dan cleanup berjalan, sambil
// menjaga FetchOnlyKnownKinds agar worker tidak mengambil job yang handler-nya tidak terdaftar.
func NewWorker(pool *pgxpool.Pool, settings config.WorkerSettings, workers *river.Workers, logger *slog.Logger) (*river.Client[pgx.Tx], error) {
	if pool == nil || workers == nil || logger == nil {
		return nil, errors.New("worker requires a platform pool, workers and logger")
	}
	if settings.Concurrency < 1 || settings.Concurrency > 64 || settings.JobTimeout <= 0 || settings.DrainTimeout <= 0 {
		return nil, errors.New("worker limits must be positive and concurrency at most 64")
	}
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Logger:              logger,
		Schema:              schema,
		Workers:             workers,
		Queues:              map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: settings.Concurrency}},
		JobTimeout:          settings.JobTimeout,
		SoftStopTimeout:     settings.DrainTimeout,
		FetchOnlyKnownKinds: true,
	})
	if err != nil {
		return nil, fmt.Errorf("cannot create queue worker client: %w", err)
	}
	return client, nil
}

// Ready memeriksa tabel antrean wajib pada schema platform. Fungsi ini tidak melakukan DDL; schema
// yang hilang membuat worker tidak siap sampai migrator dijalankan.
func Ready(ctx context.Context, pool *pgxpool.Pool) bool {
	if pool == nil {
		return false
	}
	check, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var ready bool
	err := pool.QueryRow(check, `SELECT to_regclass($1) IS NOT NULL
	    AND to_regclass($2) IS NOT NULL AND to_regclass($3) IS NOT NULL AND to_regclass($4) IS NOT NULL`,
		schema+".river_job", schema+".river_queue", schema+".river_leader", schema+".river_notification").Scan(&ready)
	return err == nil && ready
}
