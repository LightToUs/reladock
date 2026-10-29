package platform

import (
	"context"
	"encoding/json"
	"time"
)

type PreflightRecord struct {
	Report     json.RawMessage `json:"report"`
	ReportedAt *time.Time      `json:"reportedAt"`
}

func (s *Store) ReportPreflight(ctx context.Context, id string, raw json.RawMessage, fp string) error {
	if !json.Valid(raw) || len(raw) > 7000 || len(raw) < 2 || raw[0] != '{' {
		return ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = lockNode(ctx, tx, id); err != nil {
		return err
	}
	if err = checkNodeCredential(ctx, tx, id, []string{fp}); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO node_preflight(node_id,report) VALUES($1,$2) ON CONFLICT(node_id) DO UPDATE SET report=excluded.report,reported_at=now()", id, raw); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "UPDATE nodes SET last_seen_at=now() WHERE id=$1", id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) AdminPreflight(ctx context.Context, who Identity, id string) (PreflightRecord, error) {
	out := PreflightRecord{}
	tx, err := s.adminReadTx(ctx, who)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	err = tx.QueryRow(ctx, "SELECT p.report,p.reported_at FROM nodes n LEFT JOIN node_preflight p ON p.node_id=n.id WHERE n.id=$1", id).Scan(&out.Report, &out.ReportedAt)
	return out, err
}
