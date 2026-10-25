package migration

import (
	"context"
	"errors"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// AttemptSummary berisi snapshot tetap tanpa SQL atau request key. Version berupa teks desimal agar
// bigint tidak kehilangan presisi.
type AttemptSummary struct {
	ID, TargetID, DraftID, ActorID                   uuid.UUID
	DraftRevision                                    int64
	Version, Title, TargetName, ActorUsername, State string
	CreatedAt                                        time.Time
	StartedAt, FinishedAt                            *time.Time
}

// AttemptDetail aman untuk pembaca history. Identitas operator, alasan, dan bukti privat tidak
// disertakan; ResolutionSummary memakai pesan tetap.
type AttemptDetail struct {
	AttemptSummary
	TargetConfigRevision                        int64
	SQL, SQLHash                                string
	CommitStartedAt, FirstUnknownAt, ResolvedAt *time.Time
	Failure                                     *ExecutionFailure
	ResolutionSummary                           string
}

const historyColumns = `id,target_id,draft_id,executor_id,draft_revision,version::text,
    title_snapshot,target_name_snapshot,executor_username_snapshot,state,created_at,started_at,finished_at`

func summaryDestinations(a *AttemptSummary) []any {
	return []any{&a.ID, &a.TargetID, &a.DraftID, &a.ActorID, &a.DraftRevision, &a.Version,
		&a.Title, &a.TargetName, &a.ActorUsername, &a.State, &a.CreatedAt, &a.StartedAt, &a.FinishedAt}
}

func utcAttemptTimes(a *AttemptSummary) {
	a.CreatedAt = a.CreatedAt.UTC()
	utcOptionalTimes(a.StartedAt, a.FinishedAt)
}

func utcOptionalTimes(values ...*time.Time) {
	for _, value := range values {
		if value != nil {
			*value = value.UTC()
		}
	}
}

// List membaca history satu target dengan urutan version numerik menurun dan limit+1. Pemanggil
// menentukan default dan mengotorisasi DatabaseView. Pembacaan hanya memakai platform, tanpa koneksi
// target.
func (s *AttemptStore) List(ctx context.Context, targetID uuid.UUID, limit, offset int) ([]AttemptSummary, bool, error) {
	if targetID == uuid.Nil || limit < 1 || limit > 200 || offset < 0 || offset > 100000 {
		return nil, false, attemptFailure(problem.InvalidRequest)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lease, err := platformdb.Acquire(ctx, s.pool)
	if err != nil {
		return nil, false, attemptFailure(problem.PlatformUnavailable)
	}
	defer lease.Release()
	rows, err := lease.Query(ctx, `SELECT `+historyColumns+` FROM databrain.migration_attempts a
        WHERE target_id=$1 ORDER BY a.version DESC LIMIT $2 OFFSET $3`, targetID, limit+1, offset)
	if err != nil {
		return nil, false, attemptFailure(problem.PlatformUnavailable)
	}
	defer rows.Close()
	items := make([]AttemptSummary, 0, limit)
	for rows.Next() {
		var a AttemptSummary
		if err := rows.Scan(summaryDestinations(&a)...); err != nil {
			return nil, false, attemptFailure(problem.PlatformUnavailable)
		}
		utcAttemptTimes(&a)
		items = append(items, a)
	}
	if rows.Err() != nil {
		return nil, false, attemptFailure(problem.PlatformUnavailable)
	}
	if len(items) > limit {
		return items[:limit], true, nil
	}
	return items, false, nil
}

// Get mengembalikan nil untuk attempt hilang atau milik target lain. Data berasal dari snapshot
// attempt, bukan metadata terkini. Pemanggil wajib mengotorisasi DatabaseView pada targetID.
func (s *AttemptStore) Get(ctx context.Context, targetID, attemptID uuid.UUID) (*AttemptDetail, error) {
	if targetID == uuid.Nil || attemptID == uuid.Nil {
		return nil, attemptFailure(problem.InvalidRequest)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lease, err := platformdb.Acquire(ctx, s.pool)
	if err != nil {
		return nil, attemptFailure(problem.PlatformUnavailable)
	}
	defer lease.Release()
	var a AttemptDetail
	var failure ExecutionFailure
	var evidence string
	dest := append(summaryDestinations(&a.AttemptSummary), &a.TargetConfigRevision, &a.SQL, &a.SQLHash,
		&a.CommitStartedAt, &a.FirstUnknownAt, &a.ResolvedAt, &failure.Code, &failure.SQLState, &failure.StatementIndex, &evidence)
	// Jangan membaca failure_message atau field operator privat. Pesan publik dibangun dari kode, tanpa
	// menampilkan teks tersimpan.
	err = lease.QueryRow(ctx, `SELECT `+historyColumns+`,target_config_revision,sql_snapshot,encode(sql_sha256,'hex'),
        commit_started_at,first_unknown_at,resolved_at,COALESCE(failure_code,''),
        COALESCE(failure_sqlstate,''),COALESCE(failure_statement_index,0),COALESCE(outcome_evidence,'')
        FROM databrain.migration_attempts WHERE target_id=$1 AND id=$2`, targetID, attemptID).Scan(dest...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, attemptFailure(problem.PlatformUnavailable)
	}
	utcAttemptTimes(&a.AttemptSummary)
	utcOptionalTimes(a.CommitStartedAt, a.FirstUnknownAt, a.ResolvedAt)
	a.Failure = publicAttemptFailure(failure)
	if a.ResolvedAt != nil {
		switch {
		case a.State == "succeeded" && evidence == "operator_commit":
			a.ResolutionSummary = "An operator verified that the migration committed."
		case a.State == "failed" && evidence == "operator_rollback":
			a.ResolutionSummary = "An operator verified that the migration did not commit."
		}
	}
	return &a, nil
}

func publicAttemptFailure(f ExecutionFailure) *ExecutionFailure {
	if f.Code == "" {
		return nil
	}
	switch f.Code {
	case "CONNECTION_FAILED":
		f.Message = "The target connection could not be opened."
	case "ACCESS_REVOKED":
		f.Message = "Execution permission was revoked before the transaction started."
	case "SQL_ERROR":
		f.Message = "The target rejected a migration statement or transaction commit."
	case "SQL_TIMEOUT":
		f.Message = "Execution exceeded its time limit."
	case "LOCK_TIMEOUT":
		f.Message = "A migration statement exceeded its lock wait limit."
	case "CANCELED":
		f.Message = "Execution was canceled."
	case "COMMIT_UNCERTAIN":
		f.Message = "The transaction outcome could not be confirmed."
	case "OUTCOME_PERSISTENCE_FAILED":
		f.Message = "The execution outcome could not be recorded."
	case "PROCESS_INTERRUPTED":
		f.Message = "Execution was interrupted; the outcome requires operator verification."
	default:
		f.Code, f.Message = "SQL_ERROR", "The migration failed."
	}
	validState := len(f.SQLState) == 5
	for _, c := range f.SQLState {
		validState = validState && (c >= '0' && c <= '9' || c >= 'A' && c <= 'Z')
	}
	if !validState {
		f.SQLState = ""
	}
	if f.StatementIndex < 1 || f.StatementIndex > 50 {
		f.StatementIndex = 0
	}
	return &f
}
