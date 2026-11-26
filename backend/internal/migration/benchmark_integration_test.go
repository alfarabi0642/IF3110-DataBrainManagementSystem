package migration_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"runtime"
	"testing"
	"time"

	. "github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/sqlpolicy"
	migrationworker "github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/worker"
	"github.com/google/uuid"
)

// TestBenchmarkCalibrationPostgres membuat dataset sintetis pada database target disposable dan
// menaikkan jumlah baris sampai satu UPDATE seluruh tabel benar-benar melewati satu detik. Tidak ada
// sleep atau delay buatan. Tes ini berat dan hanya berjalan dengan DATABRAIN_BENCHMARK=1 pada
// fixture yang lolos pemeriksaan identitas yang sama dengan fixture integrasi lain.
func TestBenchmarkCalibrationPostgres(t *testing.T) {
	if os.Getenv("DATABRAIN_BENCHMARK") != "1" {
		t.Skip("set DATABRAIN_BENCHMARK=1 to run the disposable calibration benchmark")
	}
	f := newDraftFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if _, err := f.target.Exec(ctx, `CREATE TABLE public.benchmark_dataset (
	    id bigint PRIMARY KEY, value bigint NOT NULL, payload text NOT NULL)`); err != nil {
		t.Fatalf("cannot create calibration dataset: %v", err)
	}
	recipe, err := sqlpolicy.ValidateBatchRecipe(ctx, f.target, sqlpolicy.BatchConfig{
		Schema: "public", Table: "benchmark_dataset", KeyColumn: "id", ValueColumn: "value",
		Delta: 1, BatchSize: 1000,
	})
	if err != nil || recipe.UpSQL == "" {
		t.Fatalf("calibration dataset does not satisfy the v1 batch recipe: %v", err)
	}
	var serverVersion, database, size string
	if err := f.target.QueryRow(ctx, `SELECT current_setting('server_version'),current_database(),
	    pg_size_pretty(pg_total_relation_size('public.benchmark_dataset'))`).Scan(&serverVersion, &database, &size); err != nil {
		t.Fatal(err)
	}
	t.Logf("environment: postgres=%s database=%s go=%s/%s", serverVersion, database, runtime.GOOS, runtime.GOARCH)

	const (
		initialRows = 100_000
		maxRows     = 2_000_000
	)
	rows, inserted, iterations := 0, 0, 0
	var elapsed time.Duration
	for {
		next := initialRows
		if rows > 0 {
			next = rows * 2
		}
		if next > maxRows {
			t.Fatalf("calibration cap %d rows reached without a >1s whole-table UPDATE (last %s)", maxRows, elapsed)
		}
		tag, err := f.target.Exec(ctx, `INSERT INTO public.benchmark_dataset (id,value,payload)
		    SELECT g,0,repeat(md5(g::text),2) FROM generate_series($1::bigint,$2::bigint) g`, inserted+1, next)
		if err != nil {
			t.Fatalf("cannot generate calibration rows: %v", err)
		}
		inserted += int(tag.RowsAffected())
		rows = next
		if _, err := f.target.Exec(ctx, `ANALYZE public.benchmark_dataset`); err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		tag, err = f.target.Exec(ctx, `UPDATE public.benchmark_dataset SET value=value+1`)
		elapsed = time.Since(started)
		iterations++
		if err != nil {
			t.Fatalf("calibration UPDATE failed: %v", err)
		}
		if int(tag.RowsAffected()) != rows {
			t.Fatalf("calibration UPDATE touched %d of %d rows", tag.RowsAffected(), rows)
		}
		if err := f.target.QueryRow(ctx, `SELECT pg_size_pretty(pg_total_relation_size('public.benchmark_dataset'))`).Scan(&size); err != nil {
			t.Fatal(err)
		}
		t.Logf("calibration: rows=%d update=%s size=%s", rows, elapsed.Round(time.Millisecond), size)
		if elapsed > time.Second {
			break
		}
	}
	var count int
	var minimum, maximum int64
	if err := f.target.QueryRow(ctx, `SELECT count(*),min(value),max(value) FROM public.benchmark_dataset`).
		Scan(&count, &minimum, &maximum); err != nil {
		t.Fatal(err)
	}
	// Setiap baris menerima satu increment untuk setiap iterasi setelah baris itu dibuat; nilai
	// minimum 1 dan maksimum jumlah iterasi membuktikan tidak ada baris yang terlewat atau berubah.
	if count != rows || minimum != 1 || maximum != int64(iterations) {
		t.Fatalf("dataset changed unexpectedly: count=%d rows=%d values=%d..%d iterations=%d", count, rows, minimum, maximum, iterations)
	}
	t.Logf("calibration acceptance: rows=%d update=%s iterations=%d min=%d max=%d size=%s", rows, elapsed.Round(time.Millisecond), iterations, minimum, maximum, size)
}

// TestBenchmarkNormalVsBatchEquivalencePostgres membandingkan eksekusi normal (>1 detik) dan
// eksekusi batch terbatas (batch_size=10000) melalui WorkerExecutor produksi pada dataset identik
// serta membuktikan kesetaraan jumlah baris dan checksum akhir.
func TestBenchmarkNormalVsBatchEquivalencePostgres(t *testing.T) {
	if os.Getenv("DATABRAIN_BENCHMARK") != "1" {
		t.Skip("set DATABRAIN_BENCHMARK=1 to run the normal vs batch comparison benchmark")
	}
	h := newWorkerTestHarness(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	if _, err := h.targetEnd.Exec(ctx, `CREATE TABLE public.benchmark_compare (
		id bigint PRIMARY KEY, value bigint NOT NULL, payload text NOT NULL)`); err != nil {
		t.Fatalf("create benchmark_compare failed: %v", err)
	}

	const (
		initialRows = 100_000
		maxRows     = 2_000_000
		batchSize   = 10_000
	)
	rows, inserted := 0, 0
	var normalElapsed time.Duration
	for {
		next := initialRows
		if rows > 0 {
			next = rows * 2
		}
		if next > maxRows {
			t.Fatalf("benchmark cap %d reached without >1s normal UPDATE (last %s)", maxRows, normalElapsed)
		}
		tag, err := h.targetEnd.Exec(ctx, `INSERT INTO public.benchmark_compare (id, value, payload)
			SELECT g, 0, repeat(md5(g::text), 2) FROM generate_series($1::bigint, $2::bigint) g`, inserted+1, next)
		if err != nil {
			t.Fatalf("seed benchmark_compare failed: %v", err)
		}
		inserted += int(tag.RowsAffected())
		rows = next
		if _, err := h.targetEnd.Exec(ctx, `ANALYZE public.benchmark_compare`); err != nil {
			t.Fatal(err)
		}
		// Reset to identical initial state (value=0) before measuring single normal UPDATE.
		if _, err := h.targetEnd.Exec(ctx, `UPDATE public.benchmark_compare SET value = 0`); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		tag, err = h.targetEnd.Exec(ctx, `UPDATE public.benchmark_compare SET value = value + 1`)
		normalElapsed = time.Since(start)
		if err != nil {
			t.Fatalf("normal UPDATE failed: %v", err)
		}
		if int(tag.RowsAffected()) != rows {
			t.Fatalf("normal UPDATE touched %d of %d rows", tag.RowsAffected(), rows)
		}
		t.Logf("normal measurement: rows=%d elapsed=%s", rows, normalElapsed.Round(time.Millisecond))
		if normalElapsed > time.Second {
			break
		}
	}

	var normalCount int64
	var normalSum int64
	if err := h.targetEnd.QueryRow(ctx, `SELECT count(*), sum(value) FROM public.benchmark_compare`).Scan(&normalCount, &normalSum); err != nil {
		t.Fatal(err)
	}

	// Reset exact same dataset back to value=0 for bounded batch execution through production WorkerExecutor.
	if _, err := h.targetEnd.Exec(ctx, `UPDATE public.benchmark_compare SET value = 0`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.targetEnd.Exec(ctx, `ANALYZE public.benchmark_compare`); err != nil {
		t.Fatal(err)
	}

	cfg := sqlpolicy.BatchConfig{
		Schema:      "public",
		Table:       "benchmark_compare",
		KeyColumn:   "id",
		ValueColumn: "value",
		Delta:       1,
		BatchSize:   batchSize,
	}
	plan, err := sqlpolicy.BuildBatchPlan(cfg)
	if err != nil {
		t.Fatal(err)
	}
	d, err := h.svc.CreateDraft(ctx, h.editor, h.rf.a, DraftInput{
		Title:   "benchmark-batch-compare",
		SQL:     plan.UpSQL,
		DownSQL: plan.DownSQL,
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := h.svc.SubmitBatchRequest(ctx, h.editor, h.rf.a, d.ID, d.Revision, uuid.New(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	adm := h.approveAndAdmitRequest(t, ctx, req)

	worker, err := migrationworker.NewWorkerExecutor(h.rf.db.Pool, h.provider, h.appSecret, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	batchStart := time.Now()
	if err := worker.Execute(ctx, adm.ExecutionID, req.ID, adm.JobID); err != nil {
		t.Fatalf("batch WorkerExecutor failed: %v", err)
	}
	batchElapsed := time.Since(batchStart)

	var batchCount int64
	var batchSum int64
	if err := h.targetEnd.QueryRow(ctx, `SELECT count(*), sum(value) FROM public.benchmark_compare`).Scan(&batchCount, &batchSum); err != nil {
		t.Fatal(err)
	}
	if batchCount != normalCount || batchSum != normalSum {
		t.Fatalf("equivalence mismatch: normal(count=%d,sum=%d) vs batch(count=%d,sum=%d)", normalCount, normalSum, batchCount, batchSum)
	}
	view, err := h.svc.Execution(ctx, h.editor, h.rf.a, adm.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("benchmark equivalence PASS: rows=%d normal=%s batch=%s batches=%d batch_size=%d sum=%d",
		rows, normalElapsed.Round(time.Millisecond), batchElapsed.Round(time.Millisecond), view.CommittedBatches, batchSize, batchSum)
}
