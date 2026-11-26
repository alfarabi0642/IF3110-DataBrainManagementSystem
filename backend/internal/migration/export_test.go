package migration

import "github.com/jackc/pgx/v5/pgxpool"

func (s *DraftStore) Pool() *pgxpool.Pool { return s.pool }
