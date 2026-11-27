package worker

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/execstore"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/migration/journal"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/targets"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ExecutionEvidenceState adalah klasifikasi bukti target di bawah target advisory lock (3110,2).
type ExecutionEvidenceState string

const (
	EvidenceNotStarted     ExecutionEvidenceState = "not_started"
	EvidenceProvenRollback ExecutionEvidenceState = "proven_rollback"
	EvidenceCommitted      ExecutionEvidenceState = "committed"
	EvidencePartial        ExecutionEvidenceState = "partial"
	EvidenceUnprovable     ExecutionEvidenceState = "unprovable"
)

var (
	ErrReconcileUnprovable = errors.New("execution outcome cannot be proven from target journal; gate retained")
	ErrReconcilePartial    = errors.New("execution has partial batch commits; use approved resume or compensation")
	ErrReconcileContended  = errors.New("target ownership lock is held by an active worker")
)

// TargetEvidenceResult merangkum hasil pemeriksaan target journal yang terverifikasi.
type TargetEvidenceResult struct {
	State  ExecutionEvidenceState
	Commit *journal.JournalCommit
	Owner  *journal.JournalOwner
	Reason string
}

// ReconcileResult adalah ringkasan aman hasil rekonsiliasi operator.
type ReconcileResult struct {
	ExecutionID uuid.UUID
	Evidence    ExecutionEvidenceState
	FinalPhase  string
	Outcome     string
	AlreadyDone bool
}

// IsTransientSQLState mengembalikan true hanya untuk SQLSTATE PostgreSQL sementara yang aman
// diulang setelah rollback terkonfirmasi dan nol batch ter-commit.
func IsTransientSQLState(sqlstate string) bool {
	switch sqlstate {
	case "40001", // serialization_failure
		"40P01", // deadlock_detected
		"55P03", // lock_not_available
		"53300": // too_many_connections
		return true
	}
	return strings.HasPrefix(sqlstate, "08") // connection_exception
}

// ClassifyTargetEvidence mengklasifikasikan status nyata pada target DB setelah lock (3110,2)
// diperoleh. Journal yang hilang/tidak aman/versi berbeda atau hash tidak cocok selalu menghasilkan
// EvidenceUnprovable — tidak pernah dianggap rollback.
func ClassifyTargetEvidence(ctx context.Context, conn *pgx.Conn, executionID uuid.UUID,
	expectedDirection, expectedPlanHash string, hadPriorStart bool) TargetEvidenceResult {
	if conn == nil || executionID == uuid.Nil || !journal.IsValidPlanHash(expectedPlanHash) {
		return TargetEvidenceResult{State: EvidenceUnprovable, Reason: "invalid_inputs"}
	}
	if err := journal.VerifyTargetJournal(ctx, conn); err != nil {
		return TargetEvidenceResult{State: EvidenceUnprovable, Reason: "journal_unverified"}
	}
	owner, err := journal.ReadJournalOwner(ctx, conn)
	if err != nil {
		return TargetEvidenceResult{State: EvidenceUnprovable, Reason: "owner_read_failed"}
	}
	if owner != nil && owner.RootExecutionID != executionID {
		return TargetEvidenceResult{State: EvidenceUnprovable, Owner: owner, Reason: "owned_by_other_execution"}
	}
	commits, err := journal.ReadJournalCommits(ctx, conn, executionID)
	if err != nil {
		return TargetEvidenceResult{State: EvidenceUnprovable, Owner: owner, Reason: "commits_read_failed"}
	}
	for i := range commits {
		c := commits[i]
		if c.BatchOrdinal == 0 && c.Direction == expectedDirection {
			if c.PlanHash != expectedPlanHash {
				return TargetEvidenceResult{State: EvidenceUnprovable, Owner: owner, Reason: "commit_hash_mismatch"}
			}
			return TargetEvidenceResult{State: EvidenceCommitted, Commit: &c, Owner: owner, Reason: "normal_commit_proven"}
		}
	}
	if len(commits) > 0 {
		return TargetEvidenceResult{State: EvidencePartial, Owner: owner, Reason: "partial_batches_present"}
	}
	if hadPriorStart {
		return TargetEvidenceResult{State: EvidenceProvenRollback, Owner: owner, Reason: "zero_commits_under_lock"}
	}
	return TargetEvidenceResult{State: EvidenceNotStarted, Owner: owner, Reason: "zero_commits_under_lock"}
}

// ReconcileExecution adalah jalur rekonsiliasi operator berbasis bukti (manage reconcile-execution).
// Tidak ada opsi force-success: commit hanya difinalisasi jika terbukti di databrain_runtime, dan
// failure hanya dilepas jika lock + journal terverifikasi membuktikan nol commit.
func ReconcileExecution(ctx context.Context, pool *pgxpool.Pool, provider *targets.Provider, appSecret []byte, executionID uuid.UUID) (ReconcileResult, error) {
	if executionID == uuid.Nil {
		return ReconcileResult{}, errors.New("execution ID is required")
	}
	exec, err := NewWorkerExecutor(pool, provider, appSecret, slog.Default())
	if err != nil {
		return ReconcileResult{}, err
	}
	rec, err := exec.loadExecution(ctx, executionID)
	if err != nil {
		return ReconcileResult{}, err
	}
	op := rec.op
	if execstore.PlanHash(op) != op.PlanHash {
		return ReconcileResult{ExecutionID: executionID, Evidence: EvidenceUnprovable}, ErrReconcileUnprovable
	}
	if rec.phase == "finished" {
		return ReconcileResult{
			ExecutionID: executionID,
			FinalPhase:  rec.phase,
			Outcome:     rec.outcome,
			AlreadyDone: true,
		}, nil
	}

	conn, err := provider.OpenMigration(ctx, op.TargetID, op.TargetConfigRevision, executionID)
	if err != nil {
		return ReconcileResult{ExecutionID: executionID, Evidence: EvidenceUnprovable}, ErrReconcileUnprovable
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), workerCleanupTimeout)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()

	lock, acquired, err := journal.TryTargetLock(ctx, conn)
	if err != nil {
		return ReconcileResult{ExecutionID: executionID, Evidence: EvidenceUnprovable}, err
	}
	if !acquired {
		return ReconcileResult{ExecutionID: executionID, Evidence: EvidenceUnprovable}, ErrReconcileContended
	}
	defer func() {
		relCtx, cancel := context.WithTimeout(context.Background(), workerCleanupTimeout)
		defer cancel()
		_ = lock.Release(relCtx)
	}()

	ownerSecret, err := journal.DeriveOwnerSecret(appSecret, executionID)
	if err != nil {
		return ReconcileResult{}, err
	}

	ev := ClassifyTargetEvidence(ctx, conn, executionID, op.Direction, op.PlanHash, rec.phase != "queued")
	switch ev.State {
	case EvidenceCommitted:
		if err := exec.finalizeAndClearOwner(conn, executionID, 0, op, *ev.Commit, ownerSecret); err != nil {
			return ReconcileResult{ExecutionID: executionID, Evidence: ev.State}, err
		}
		return ReconcileResult{
			ExecutionID: executionID,
			Evidence:    EvidenceCommitted,
			FinalPhase:  "finished",
			Outcome:     "succeeded",
		}, nil

	case EvidenceNotStarted, EvidenceProvenRollback:
		if ev.Owner != nil && ev.Owner.RootExecutionID == executionID {
			if err := journal.ClearJournalOwner(ctx, conn, executionID, ownerSecret); err != nil {
				return ReconcileResult{ExecutionID: executionID, Evidence: ev.State}, err
			}
		}
		if err := exec.markFailedAndRelease(executionID, 0, "RECONCILED_NOT_COMMITTED",
			"Target journal confirmed zero committed changes under lock.", ""); err != nil {
			return ReconcileResult{ExecutionID: executionID, Evidence: ev.State}, err
		}
		return ReconcileResult{
			ExecutionID: executionID,
			Evidence:    ev.State,
			FinalPhase:  "finished",
			Outcome:     "failed",
		}, nil

	case EvidencePartial:
		return ReconcileResult{ExecutionID: executionID, Evidence: EvidencePartial}, ErrReconcilePartial

	default:
		return ReconcileResult{ExecutionID: executionID, Evidence: EvidenceUnprovable}, ErrReconcileUnprovable
	}
}
