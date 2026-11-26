package execstore

import (
	"context"
	"errors"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/jobs"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const enqueueReceiptColumns = `intent_hash,execution_id,job_id`

// EnqueueResult mengidentifikasi job dan execution yang tersimpan. Replayed menandai key yang sama
// mengembalikan penerimaan lama, bukan penerimaan baru.
type EnqueueResult struct {
	ExecutionID uuid.UUID
	JobID       int64
	Replayed    bool
}

type enqueueReceipt struct {
	intentHash  string
	executionID uuid.UUID
	jobID       int64
}

func (r enqueueReceipt) result(replayed bool) EnqueueResult {
	return EnqueueResult{ExecutionID: r.executionID, JobID: r.jobID, Replayed: replayed}
}

func enqueueFailure(code problem.Code) error { return &problem.Error{Code: code} }

func enqueueStoreError(err error) error {
	var p *pgconn.PgError
	if errors.As(err, &p) {
		switch p.ConstraintName {
		case "migration_streams_single_active_owner", "one_unresolved_attempt_per_target":
			return enqueueFailure(problem.TargetBusy)
		case "execution_receipts_pkey", "execution_receipts_execution_id_key", "execution_receipts_job_id_key":
			return enqueueFailure(problem.IdempotencyConflict)
		case "migration_streams_active_execution_fk", "migration_executions_source_execution_id_fkey",
			"migration_executions_target_id_fkey", "migration_executions_actor_id_fkey",
			"execution_receipts_actor_id_fkey", "execution_receipts_target_id_execution_id_fkey":
			return enqueueFailure(problem.InvalidRequest)
		}
		if p.Code == "23503" || p.Code == "23514" {
			return enqueueFailure(problem.InvalidRequest)
		}
	}
	return enqueueFailure(problem.PlatformUnavailable)
}

// EnqueueStore menerima operasi yang sudah disetujui dan menulis execution, job, serta receipt pada
// transaksi milik caller. Store tidak pernah Begin, Commit, atau Rollback dan tidak membuka koneksi
// target. Caller wajib mengonsumsi approval pada transaksi yang sama agar penolakan guard membatalkan
// seluruh tulisan.
type EnqueueStore struct {
	pool     *pgxpool.Pool
	enqueuer *jobs.Enqueuer
}

func NewEnqueueStore(pool *pgxpool.Pool, enqueuer *jobs.Enqueuer) (*EnqueueStore, error) {
	if pool == nil || enqueuer == nil {
		return nil, enqueueFailure(problem.InternalError)
	}
	return &EnqueueStore{pool: pool, enqueuer: enqueuer}, nil
}

func lookupExecutionReceipt(ctx context.Context, db platformdb.DBTX, targetID, actorID, key uuid.UUID) (*enqueueReceipt, error) {
	var receipt enqueueReceipt
	err := db.QueryRow(ctx, `SELECT `+enqueueReceiptColumns+` FROM databrain.execution_receipts
	    WHERE target_id=$1 AND actor_id=$2 AND request_key=$3`, targetID, actorID, key).
		Scan(&receipt.intentHash, &receipt.executionID, &receipt.jobID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, enqueueStoreError(err)
	}
	return &receipt, nil
}

// Enqueue menulis execution/receipt dan InsertTx memakai transaksi caller. Replay dengan key dan
// intent yang sama mengembalikan ID lama meski target sedang sibuk; key yang sama dengan intent
// berbeda ditolak. Transaksi yang dibatalkan caller membatalkan job juga.
func (s *EnqueueStore) Enqueue(ctx context.Context, tx pgx.Tx, op ApprovedOperation, requestKey uuid.UUID) (EnqueueResult, error) {
	if tx == nil {
		return EnqueueResult{}, enqueueFailure(problem.InternalError)
	}
	if err := op.validate(requestKey); err != nil {
		return EnqueueResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// Replay diperiksa sebelum contention stream; penerimaan yang sudah commit tidak perlu menunggu.
	receipt, err := lookupExecutionReceipt(ctx, tx, op.TargetID, op.ActorID, requestKey)
	if err != nil {
		return EnqueueResult{}, err
	}
	if receipt != nil {
		if receipt.intentHash != op.PlanHash {
			return EnqueueResult{}, enqueueFailure(problem.IdempotencyConflict)
		}
		return receipt.result(true), nil
	}
	var activeAttempt, activeExec *string
	err = tx.QueryRow(ctx, `SELECT active_attempt_id::text, active_execution_id::text
	    FROM databrain.migration_streams WHERE target_id=$1 FOR UPDATE`, op.TargetID).Scan(&activeAttempt, &activeExec)
	if errors.Is(err, pgx.ErrNoRows) {
		return EnqueueResult{}, enqueueFailure(problem.DatabaseNotFound)
	}
	if err != nil {
		return EnqueueResult{}, enqueueStoreError(err)
	}
	// Statement READ COMMITTED baru melihat pemenang yang commit saat lock stream ditunggu.
	receipt, err = lookupExecutionReceipt(ctx, tx, op.TargetID, op.ActorID, requestKey)
	if err != nil {
		return EnqueueResult{}, err
	}
	if receipt != nil {
		if receipt.intentHash != op.PlanHash {
			return EnqueueResult{}, enqueueFailure(problem.IdempotencyConflict)
		}
		return receipt.result(true), nil
	}
	if activeAttempt != nil {
		return EnqueueResult{}, enqueueFailure(problem.TargetBusy)
	}
	if activeExec != nil {
		canTransfer := false
		if (op.Direction == DirectionResume || op.Direction == DirectionCompensate) &&
			op.SourceExecutionID != nil && *activeExec == op.SourceExecutionID.String() {
			var srcPhase, srcOutcome string
			if err := tx.QueryRow(ctx, `SELECT phase, coalesce(outcome,'') FROM databrain.migration_executions
				WHERE target_id=$1 AND id=$2 FOR UPDATE`, op.TargetID, *op.SourceExecutionID).Scan(&srcPhase, &srcOutcome); err == nil &&
				srcPhase == "blocked" && (srcOutcome == "partial" || srcOutcome == "partially_reverted") {
				canTransfer = true
			}
		}
		if !canTransfer {
			return EnqueueResult{}, enqueueFailure(problem.TargetBusy)
		}
	}
	var configRevision int64
	err = tx.QueryRow(ctx, `SELECT config_revision FROM databrain.database_targets
	    WHERE id=$1 AND is_active FOR SHARE`, op.TargetID).Scan(&configRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return EnqueueResult{}, enqueueFailure(problem.DatabaseNotFound)
	}
	if err != nil {
		return EnqueueResult{}, enqueueStoreError(err)
	}
	if configRevision != op.TargetConfigRevision {
		return EnqueueResult{}, enqueueFailure(problem.RevisionConflict)
	}
	executionID := uuid.New()
	if err := insertExecution(ctx, tx, executionID, op); err != nil {
		return EnqueueResult{}, enqueueStoreError(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE databrain.migration_streams SET active_execution_id=$2
	    WHERE target_id=$1`, op.TargetID, executionID); err != nil {
		return EnqueueResult{}, enqueueStoreError(err)
	}
	jobID, err := s.enqueuer.Enqueue(ctx, tx, jobs.ExecutionJobArgs{
		Version: jobs.PayloadVersion, ExecutionID: executionID, RequestID: op.RequestID,
	})
	if err != nil {
		return EnqueueResult{}, enqueueFailure(problem.PlatformUnavailable)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO databrain.execution_receipts
	    (target_id,actor_id,request_key,intent_hash,execution_id,job_id) VALUES ($1,$2,$3,$4,$5,$6)`,
		op.TargetID, op.ActorID, requestKey, op.PlanHash, executionID, jobID); err != nil {
		return EnqueueResult{}, enqueueStoreError(err)
	}
	return EnqueueResult{ExecutionID: executionID, JobID: jobID}, nil
}

// Receipt mencari penerimaan pada koneksi baru dan menyerialkannya dengan stream agar penerimaan yang
// sedang commit tidak disalahartikan sebagai ketiadaan. Nilai nil berarti belum ada bukti tersimpan;
// caller tidak boleh membuat job baru hanya karena hasil ini kosong.
func (s *EnqueueStore) Receipt(ctx context.Context, targetID, actorID, key uuid.UUID) (*EnqueueResult, error) {
	if targetID == uuid.Nil || actorID == uuid.Nil || key == uuid.Nil {
		return nil, enqueueFailure(problem.InvalidRequest)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lease, err := platformdb.Acquire(ctx, s.pool)
	if err != nil {
		return nil, enqueueFailure(problem.PlatformUnavailable)
	}
	defer lease.Release()
	tx, err := lease.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, enqueueFailure(problem.PlatformUnavailable)
	}
	defer rollbackTx(tx)
	var present bool
	err = tx.QueryRow(ctx, `SELECT true FROM databrain.migration_streams
	    WHERE target_id=$1 FOR SHARE`, targetID).Scan(&present)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, enqueueFailure(problem.DatabaseNotFound)
	}
	if err != nil {
		return nil, enqueueStoreError(err)
	}
	receipt, err := lookupExecutionReceipt(ctx, tx, targetID, actorID, key)
	if err != nil || receipt == nil {
		return nil, err
	}
	result := receipt.result(false)
	return &result, nil
}

func rollbackTx(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}
