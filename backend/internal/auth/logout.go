package auth

import (
	"context"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/jackc/pgx/v5"
)

// Logout melakukan commit revoke sebelum adapter boleh menghapus cookie.
func (s *Service) Logout(ctx context.Context, state Session) error {
	if state.Kind != AuthenticatedSession {
		return sessionFailure(problem.SessionInvalid)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lease, err := platformdb.Acquire(ctx, s.pool)
	if err != nil {
		return sessionFailure(problem.PlatformUnavailable)
	}
	defer lease.Release()
	tx, err := lease.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return sessionFailure(problem.PlatformUnavailable)
	}
	defer rollbackAuth(tx)
	if err := s.RevokeSession(ctx, tx, state.ID); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return sessionFailure(problem.PlatformUnavailable)
	}
	return nil
}
