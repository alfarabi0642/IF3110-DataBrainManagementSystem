package platformdb

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Process lock wajib diperoleh sebelum pool dipakai untuk recovery atau operasi domain.
func openProcessLock(ctx context.Context, cfg *pgx.ConnConfig) (_ *pgx.Conn, err error) {
	cfg = cfg.Copy()
	cfg.RuntimeParams["application_name"] = "databrain-process-lock"
	lock, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer func() {
		if err != nil {
			cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = lock.Close(cleanup)
		}
	}()
	if err := checkSchema(ctx, lock); err != nil {
		return nil, err
	}
	var safeRole bool
	if err := lock.QueryRow(ctx, `SELECT NOT (rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls)
	    AND NOT has_schema_privilege(current_user,'databrain','CREATE')
	    AND NOT has_table_privilege(current_user,'databrain.migration_attempts','DELETE,TRUNCATE')
	    FROM pg_roles WHERE rolname=current_user`).Scan(&safeRole); err != nil {
		return nil, ErrUnavailable
	}
	if !safeRole {
		return nil, ErrRuntimeRole
	}
	var acquired bool
	if err := lock.QueryRow(ctx, "SELECT pg_try_advisory_lock(3110,1)").Scan(&acquired); err != nil {
		return nil, ErrUnavailable
	}
	if !acquired {
		return nil, ErrProcessLock
	}
	return lock, nil
}

func (db *DB) Lost() <-chan struct{} { return db.lost }

// WithProcessLock menjalankan startup/maintenance pada koneksi pemegang process lock. Koneksi hilang
// membatalkan transaksi sekaligus melepas lock agar server baru tidak tumpang tindih dengan transaksi
// lama. Callback tidak boleh melepas lock atau menyimpan koneksi ini.
func (db *DB) WithProcessLock(ctx context.Context, work func(*pgx.Conn) error) error {
	db.lockMu.Lock()
	defer db.lockMu.Unlock()
	if !db.available() {
		return ErrLockLost
	}
	var held bool
	if err := db.lock.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype='advisory'
        AND pid=pg_backend_pid() AND classid=3110 AND objid=1 AND objsubid=2 AND granted)`).Scan(&held); err != nil || !held {
		return ErrLockLost
	}
	return work(db.lock)
}

func (db *DB) monitor(ctx context.Context) {
	defer close(db.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			db.lockMu.Lock()
			check, cancel := context.WithTimeout(ctx, 2*time.Second)
			var held bool
			err := db.lock.QueryRow(check, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype='advisory'
			    AND pid=pg_backend_pid() AND classid=3110 AND objid=1 AND objsubid=2 AND granted)`).Scan(&held)
			db.lockMu.Unlock()
			cancel()
			if ctx.Err() != nil {
				return
			}
			if err != nil || !held {
				close(db.lost)
				return
			}
		}
	}
}

// Ready memeriksa konektivitas, schema, dan kepemilikan process lock saat ini. Lock yang hilang tidak
// diambil ulang.
func (db *DB) Ready(ctx context.Context) bool {
	if !db.available() {
		return false
	}
	check, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	conn, err := Acquire(check, db.Pool)
	if err != nil {
		return false
	}
	defer conn.Release()
	return checkSchema(check, conn) == nil && db.available()
}

func (db *DB) available() bool {
	select {
	case <-db.lost:
		return false
	case <-db.closing:
		return false
	default:
		return true
	}
}
