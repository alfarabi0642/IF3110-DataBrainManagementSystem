package platformdb

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func runtimePoolConfig(databaseURL string) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, ErrUnavailable
	}
	cfg.MaxConns, cfg.MinConns, cfg.MinIdleConns = 10, 0, 0
	cfg.PingTimeout = 2 * time.Second
	cfg.ConnConfig.ConnectTimeout = 5 * time.Second
	cfg.ConnConfig.RuntimeParams["application_name"] = "databrain-platform"
	cfg.ConnConfig.RuntimeParams["search_path"] = "pg_catalog,databrain"
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = "10000"
	return cfg, nil
}

func openPool(ctx context.Context, cfg *pgxpool.Config) (*pgxpool.Pool, error) {
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, ErrUnavailable
	}
	conn, err := Acquire(ctx, pool)
	if err == nil {
		err = conn.Ping(ctx)
		conn.Release()
	}
	if err != nil {
		pool.Close()
		return nil, ErrUnavailable
	}
	return pool, nil
}

// Acquire membatasi penantian koneksi pool; pemanggil wajib Release. Batas query/transaksi tetap
// berasal dari context operasi.
func Acquire(ctx context.Context, pool *pgxpool.Pool) (*pgxpool.Conn, error) {
	acquire, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	conn, err := pool.Acquire(acquire)
	if err != nil {
		return nil, ErrUnavailable
	}
	return conn, nil
}

func checkSchema(ctx context.Context, query DBTX) error {
	var version int64
	var dirty bool
	err := query.QueryRow(ctx, `SELECT version,dirty FROM public.schema_migrations
	    WHERE (SELECT count(*) FROM public.schema_migrations)=1`).Scan(&version, &dirty)
	if err != nil || version != SchemaVersion || dirty {
		return ErrSchema
	}
	return nil
}
