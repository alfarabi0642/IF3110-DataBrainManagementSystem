package migration

import (
	"context"
	"errors"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Service mengotorisasi caller sebelum membaca atau mengubah data satu target.
type Service struct {
	store      *DraftStore
	runner     *Runner
	authorizer access.Authorizer
}

func NewService(store *DraftStore, runner *Runner, authorizer access.Authorizer) (*Service, error) {
	if store == nil || store.pool == nil || runner == nil || runner.store == nil || runner.store.pool != store.pool || authorizer == nil {
		return nil, draftFailure(problem.InternalError)
	}
	return &Service{store: store, runner: runner, authorizer: authorizer}, nil
}

type Database struct {
	ID     uuid.UUID
	Name   string
	Access access.Level
}

func (s *Service) Database(ctx context.Context, p access.Principal, target uuid.UUID, action access.Action) (Database, error) {
	level, err := s.authorizer.RequireDatabase(ctx, p, target, action)
	if err != nil {
		return Database{}, err
	}
	lease, err := platformdb.Acquire(ctx, s.store.pool)
	if err != nil {
		return Database{}, draftFailure(problem.PlatformUnavailable)
	}
	defer lease.Release()
	d := Database{ID: target, Access: level}
	err = lease.QueryRow(ctx, `SELECT name FROM databrain.database_targets WHERE id=$1 AND is_active`, target).Scan(&d.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return Database{}, draftFailure(problem.DatabaseNotFound)
	}
	if err != nil {
		return Database{}, draftFailure(problem.PlatformUnavailable)
	}
	return d, nil
}

func (s *Service) ListDrafts(ctx context.Context, p access.Principal, target uuid.UUID, limit, offset int) ([]DraftSummary, bool, error) {
	if _, err := s.authorizer.RequireDatabase(ctx, p, target, access.MigrationWrite); err != nil {
		return nil, false, err
	}
	drafts, more, err := s.store.List(ctx, target, limit, offset)
	if err == nil {
		err = s.draftAuthors(ctx, drafts)
	}
	return drafts, more, err
}

func (s *Service) GetDraft(ctx context.Context, p access.Principal, target, draft uuid.UUID) (Draft, error) {
	if _, err := s.authorizer.RequireDatabase(ctx, p, target, access.MigrationWrite); err != nil {
		return Draft{}, err
	}
	d, err := s.store.Get(ctx, target, draft)
	return s.withDraftAuthor(ctx, d, err)
}

func (s *Service) CreateDraft(ctx context.Context, p access.Principal, target uuid.UUID, input DraftInput) (Draft, error) {
	if _, err := s.authorizer.RequireDatabase(ctx, p, target, access.MigrationWrite); err != nil {
		return Draft{}, err
	}
	d, err := s.store.Create(ctx, target, p.UserID, input)
	return s.withDraftAuthor(ctx, d, err)
}

func (s *Service) SaveDraft(ctx context.Context, p access.Principal, target, draft uuid.UUID, input SaveDraftInput) (Draft, error) {
	if _, err := s.authorizer.RequireDatabase(ctx, p, target, access.MigrationWrite); err != nil {
		return Draft{}, err
	}
	d, err := s.store.Save(ctx, target, draft, p.UserID, input)
	return s.withDraftAuthor(ctx, d, err)
}

func (s *Service) withDraftAuthor(ctx context.Context, d Draft, err error) (Draft, error) {
	if err != nil {
		return Draft{}, err
	}
	items := []DraftSummary{d.DraftSummary}
	if err := s.draftAuthors(ctx, items); err != nil {
		return Draft{}, err
	}
	d.DraftSummary = items[0]
	return d, nil
}

func (s *Service) draftAuthors(ctx context.Context, drafts []DraftSummary) error {
	if len(drafts) == 0 {
		return nil
	}
	actors := make([]uuid.UUID, 0, len(drafts))
	for _, d := range drafts {
		actors = append(actors, d.UpdatedBy)
	}
	lease, err := platformdb.Acquire(ctx, s.store.pool)
	if err != nil {
		return draftFailure(problem.PlatformUnavailable)
	}
	defer lease.Release()
	rows, err := lease.Query(ctx, `SELECT id,username FROM databrain.users WHERE id=ANY($1)`, actors)
	if err != nil {
		return draftFailure(problem.PlatformUnavailable)
	}
	defer rows.Close()
	names := make(map[uuid.UUID]string)
	for rows.Next() {
		var id uuid.UUID
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return draftFailure(problem.PlatformUnavailable)
		}
		names[id] = name
	}
	if rows.Err() != nil {
		return draftFailure(problem.PlatformUnavailable)
	}
	for i := range drafts {
		drafts[i].UpdatedByUsername = names[drafts[i].UpdatedBy]
	}
	return nil
}

type Review struct {
	Input   ReserveInput
	Draft   *Draft
	Receipt *AttemptReceipt
}

// Review membaca receipt sebelum revision terkini; pembacaan ini tidak mereservasi attempt.
func (s *Service) Review(ctx context.Context, p access.Principal, target, draft uuid.UUID, input *ReserveInput) (Review, error) {
	if _, err := s.authorizer.RequireDatabase(ctx, p, target, access.MigrationWrite); err != nil {
		return Review{}, err
	}
	var result Review
	if input != nil {
		if input.ExpectedRevision < 1 || input.RequestKey == uuid.Nil {
			return Review{}, attemptFailure(problem.InvalidRequest)
		}
		result.Input = *input
		r, err := s.runner.replay(ctx, target, draft, p.UserID, *input)
		if err != nil || r != nil {
			result.Receipt = r
			return result, err
		}
	}
	d, err := s.store.Get(ctx, target, draft)
	if err != nil {
		return Review{}, err
	}
	if input == nil {
		result.Input = ReserveInput{d.Revision, uuid.New()}
	} else if input.ExpectedRevision != d.Revision {
		return Review{}, attemptFailure(problem.RevisionConflict)
	}
	result.Draft = &d
	return result, nil
}

// Execute mempertahankan key saat persistence gagal; lookup tidak mengulang SQL.
func (s *Service) Execute(ctx context.Context, p access.Principal, target, draft uuid.UUID, input ReserveInput) (AttemptReceipt, error) {
	end := time.Now().Add(65 * time.Second)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(end) {
		end = deadline
	}
	r, err := s.runner.Execute(ctx, p, target, draft, input)
	if err == nil {
		return r, nil
	}
	var failure *problem.Error
	if errors.As(err, &failure) && (failure.Code == problem.SessionInvalid || failure.Code == problem.DatabaseNotFound || failure.Code == problem.DatabaseAccessDenied) {
		return r, err
	}
	lookup, cancel := cleanupExecutionContext(end, 2*time.Second)
	defer cancel()
	if stored, lookupErr := s.runner.replay(lookup, target, draft, p.UserID, input); lookupErr == nil && stored != nil {
		return *stored, nil
	}
	return r, err
}

func (s *Service) History(ctx context.Context, p access.Principal, target uuid.UUID, limit, offset int) ([]AttemptSummary, bool, error) {
	if _, err := s.authorizer.RequireDatabase(ctx, p, target, access.DatabaseView); err != nil {
		return nil, false, err
	}
	return s.runner.store.List(ctx, target, limit, offset)
}

func (s *Service) Attempt(ctx context.Context, p access.Principal, target, attempt uuid.UUID) (*AttemptDetail, error) {
	if _, err := s.authorizer.RequireDatabase(ctx, p, target, access.DatabaseView); err != nil {
		return nil, err
	}
	a, err := s.runner.store.Get(ctx, target, attempt)
	if err == nil && a == nil {
		err = attemptFailure(problem.AttemptNotFound)
	}
	return a, err
}

func (s *Service) Receipt(ctx context.Context, p access.Principal, target, key uuid.UUID) (*AttemptReceipt, error) {
	if _, err := s.authorizer.RequireDatabase(ctx, p, target, access.DatabaseView); err != nil {
		return nil, err
	}
	return s.runner.store.Receipt(ctx, target, p.UserID, key)
}
