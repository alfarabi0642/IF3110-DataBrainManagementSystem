package migration

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const MaxResolutionBytes = 64 << 10

var (
	ErrResolutionInput = errors.New("resolution requires succeeded or failed, an operator name, and nonempty UTF-8 evidence (maximum 64 KiB)")
	ErrResolutionState = errors.New("only an unknown attempt with its active stream can be resolved")
)

type ResolutionInput struct {
	Outcome, Operator, Evidence string
}

func ValidateResolution(input ResolutionInput) error {
	if (input.Outcome != "succeeded" && input.Outcome != "failed") ||
		!utf8.ValidString(input.Operator) || strings.TrimSpace(input.Operator) == "" || utf8.RuneCountInString(input.Operator) > 120 || strings.ContainsRune(input.Operator, 0) ||
		!utf8.ValidString(input.Evidence) || strings.TrimSpace(input.Evidence) == "" || len(input.Evidence) > MaxResolutionBytes || strings.ContainsRune(input.Evidence, 0) {
		return ErrResolutionInput
	}
	return nil
}

// RecoverInterrupted berjalan setelah process lock diperoleh dan sebelum menerima traffic. Fungsi ini
// tidak membuka target atau mengulang SQL attempt yang terputus.
func RecoverInterrupted(ctx context.Context, db *platformdb.DB) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var count int64
	err := db.WithProcessLock(ctx, func(conn *pgx.Conn) error {
		tx, err := conn.Begin(ctx)
		if err != nil {
			return attemptFailure(problem.PlatformUnavailable)
		}
		defer rollbackAttempt(tx)
		// Kunci stream dalam urutan tetap sebelum menyentuh attempt.
		rows, err := tx.Query(ctx, `SELECT target_id FROM databrain.migration_streams ORDER BY target_id FOR UPDATE`)
		if err != nil {
			return attemptFailure(problem.PlatformUnavailable)
		}
		for rows.Next() {
			// Pengurasan hasil mengunci baris; ID target tidak perlu disimpan.
		}
		rows.Close()
		if rows.Err() != nil {
			return attemptFailure(problem.PlatformUnavailable)
		}
		var consistent bool
		err = tx.QueryRow(ctx, `SELECT NOT EXISTS (
            SELECT 1 FROM databrain.migration_attempts a LEFT JOIN databrain.migration_streams s ON s.target_id=a.target_id
            WHERE a.state IN ('reserved','running','unknown') AND s.active_attempt_id IS DISTINCT FROM a.id
        ) AND NOT EXISTS (
            SELECT 1 FROM databrain.migration_streams s JOIN databrain.migration_attempts a ON a.target_id=s.target_id AND a.id=s.active_attempt_id
            WHERE a.state NOT IN ('reserved','running','unknown')
        )`).Scan(&consistent)
		if err != nil || !consistent {
			return attemptFailure(problem.PlatformUnavailable)
		}
		tag, err := tx.Exec(ctx, `UPDATE databrain.migration_attempts SET state='unknown',
            failure_code='PROCESS_INTERRUPTED',failure_message='Execution was interrupted; the outcome requires operator verification.',
            failure_sqlstate=NULL,failure_statement_index=NULL WHERE state IN ('reserved','running')`)
		if err != nil {
			return attemptFailure(problem.PlatformUnavailable)
		}
		if err := tx.Commit(ctx); err != nil {
			return attemptFailure(problem.PlatformUnavailable)
		}
		count = tag.RowsAffected()
		return nil
	})
	return count, err
}

// ResolveAttempt mencatat bukti eksplisit operator saat maintenance. Fungsi ini tidak memverifikasi
// bukti, menjalankan SQL, atau menyimpulkan hasil dari schema target terkini. Tanpa bukti cukup,
// status tetap unknown.
func ResolveAttempt(ctx context.Context, db *platformdb.DB, attemptID uuid.UUID, input ResolutionInput) error {
	if attemptID == uuid.Nil {
		return ErrResolutionInput
	}
	if err := ValidateResolution(input); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return db.WithProcessLock(ctx, func(conn *pgx.Conn) error {
		tx, err := conn.Begin(ctx)
		if err != nil {
			return attemptFailure(problem.PlatformUnavailable)
		}
		defer rollbackAttempt(tx)
		// CLI menerima UUID attempt global; cari target di server, lalu batasi setiap lock/update pada
		// pasangan target dan attempt itu.
		var targetID uuid.UUID
		var activeID *uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT target_id FROM databrain.migration_attempts WHERE id=$1`, attemptID).Scan(&targetID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrResolutionState
			}
			return attemptFailure(problem.PlatformUnavailable)
		}
		var state string
		err = tx.QueryRow(ctx, `SELECT active_attempt_id FROM databrain.migration_streams WHERE target_id=$1 FOR UPDATE`, targetID).Scan(&activeID)
		if err != nil {
			return attemptFailure(problem.PlatformUnavailable)
		}
		err = tx.QueryRow(ctx, `SELECT state FROM databrain.migration_attempts WHERE target_id=$1 AND id=$2 FOR UPDATE`, targetID, attemptID).Scan(&state)
		if err != nil {
			return attemptFailure(problem.PlatformUnavailable)
		}
		if state != "unknown" || activeID == nil || *activeID != attemptID {
			return ErrResolutionState
		}
		evidence := "operator_rollback"
		if input.Outcome == "succeeded" {
			evidence = "operator_commit"
		}
		_, err = tx.Exec(ctx, `UPDATE databrain.migration_attempts SET state=$3,outcome_evidence=$4,
            finished_at=now(),resolved_at=now(),resolved_by_operator=$5,resolution_reason=$6,
            failure_code=CASE WHEN $3='succeeded' THEN NULL ELSE failure_code END,
            failure_message=CASE WHEN $3='succeeded' THEN NULL ELSE failure_message END,
            failure_sqlstate=CASE WHEN $3='succeeded' THEN NULL ELSE failure_sqlstate END,
            failure_statement_index=CASE WHEN $3='succeeded' THEN NULL ELSE failure_statement_index END
            WHERE target_id=$1 AND id=$2`, targetID, attemptID, input.Outcome, evidence, input.Operator, input.Evidence)
		if err != nil {
			return attemptFailure(problem.PlatformUnavailable)
		}
		tag, err := tx.Exec(ctx, `UPDATE databrain.migration_streams SET active_attempt_id=NULL WHERE target_id=$1 AND active_attempt_id=$2`, targetID, attemptID)
		if err != nil || tag.RowsAffected() != 1 {
			return attemptFailure(problem.PlatformUnavailable)
		}
		if err := tx.Commit(ctx); err != nil {
			return attemptFailure(problem.PlatformUnavailable)
		}
		return nil
	})
}
