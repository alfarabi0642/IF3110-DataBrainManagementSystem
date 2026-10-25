package auth

import (
	"context"
	"time"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/platformdb"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
)

const cleanupBatchSize = 100

// Cleanup menjalankan satu putaran dengan batas waktu, dipanggil tiap menit dan dibatalkan saat
// shutdown. Setiap delete memakai commit terpisah; pengulangan aman jika sebagian cleanup gagal.
func (s *Service) Cleanup(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lease, err := platformdb.Acquire(ctx, s.pool)
	if err != nil {
		return sessionFailure(problem.PlatformUnavailable)
	}
	defer lease.Release()
	for _, query := range []string{
		`WITH expired AS (
		    SELECT st.token_hash FROM databrain.session_tokens st JOIN databrain.sessions s ON s.id=st.session_id
		    WHERE st.valid_until<=statement_timestamp() AND st.token_hash<>s.current_token_hash
		    ORDER BY st.valid_until,st.token_hash LIMIT $1 FOR UPDATE OF st SKIP LOCKED
		) DELETE FROM databrain.session_tokens st USING expired e WHERE st.token_hash=e.token_hash`,
		`WITH expired AS (
		    SELECT id FROM databrain.sessions
		    WHERE (kind='anonymous' AND absolute_expires_at<=statement_timestamp())
		       OR revoked_at<statement_timestamp()-interval '7 days'
		       OR absolute_expires_at<statement_timestamp()-interval '7 days'
		       OR (kind='authenticated' AND last_seen_at+interval '30 minutes'<statement_timestamp()-interval '7 days')
		    ORDER BY id LIMIT $1 FOR UPDATE SKIP LOCKED
		) DELETE FROM databrain.sessions s USING expired e WHERE s.id=e.id`,
		`WITH expired AS (
		    SELECT key_type,key_hash,window_start FROM databrain.login_windows
		    WHERE window_start<statement_timestamp()-interval '30 minutes'
		    ORDER BY window_start,key_type,key_hash LIMIT $1 FOR UPDATE SKIP LOCKED
		) DELETE FROM databrain.login_windows w USING expired e
		    WHERE w.key_type=e.key_type AND w.key_hash=e.key_hash AND w.window_start=e.window_start`,
	} {
		if _, err := lease.Exec(ctx, query, cleanupBatchSize); err != nil {
			return sessionFailure(problem.PlatformUnavailable)
		}
	}
	return nil
}
