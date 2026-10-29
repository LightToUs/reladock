package platform

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// AllowRequest shares fixed-minute limits between control processes using the
// database clock. It fails closed when storage is unavailable.
func (s *Store) AllowRequest(ctx context.Context, client, category string, limit int) (bool, error) {
	if client == "" || len(client) > 256 || (category != "auth" && category != "api") || limit < 1 || limit > 100000 {
		return false, ErrInvalid
	}
	var n int
	err := s.pool.QueryRow(ctx, `INSERT INTO request_limits(key_hash,category,requests,expires_at) VALUES($1,$2,1,clock_timestamp()+interval '1 minute')
ON CONFLICT(key_hash,category) DO UPDATE SET requests=CASE WHEN request_limits.expires_at<=clock_timestamp() THEN 1 ELSE request_limits.requests+1 END,
expires_at=CASE WHEN request_limits.expires_at<=clock_timestamp() THEN clock_timestamp()+interval '1 minute' ELSE request_limits.expires_at END
WHERE request_limits.expires_at<=clock_timestamp() OR request_limits.requests<$3 RETURNING requests`, tokenHash(client), category, limit).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// PruneRequestLimits is a bounded maintenance operation; a busy client cannot
// force unbounded deletion work inside its request transaction.
func (s *Store) PruneRequestLimits(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM request_limits WHERE (key_hash,category) IN (SELECT key_hash,category FROM request_limits WHERE expires_at<clock_timestamp()-interval '1 day' LIMIT 1000)`)
	return err
}
