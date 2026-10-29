package platform

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"time"
)

func (s *Store) BeginApplication(ctx context.Context, nodeID, jobID string, report ConfigReport, credential ...string) error {
	if report.Outcome != "applying" || report.ErrorCode != "" || len(report.LeaseToken) != 43 || !fingerprintPattern.MatchString(report.Checksum) {
		return ErrInvalid
	}
	tx, e := s.pool.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if e = lockNode(ctx, tx, nodeID); e != nil {
		return e
	}
	if e = checkNodeCredential(ctx, tx, nodeID, credential); e != nil {
		return e
	}
	var state, token, checksum string
	var generation, current int64
	var until time.Time
	var payload []byte
	e = tx.QueryRow(ctx, `SELECT o.state,COALESCE(o.lease_token,''),o.checksum,o.generation,n.desired_generation,COALESCE(o.lease_until,now()),o.payload FROM config_outbox o JOIN nodes n ON n.id=o.node_id WHERE o.id=$1 AND o.node_id=$2 FOR UPDATE OF o`, jobID, nodeID).Scan(&state, &token, &checksum, &generation, &current, &until, &payload)
	if errors.Is(e, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if e != nil {
		return e
	}
	if token != report.LeaseToken || checksum != report.Checksum {
		return ErrConflict
	}
	// Once begun the fence never expires on its own. A timed-out old process
	// could still mutate the kernel; a new lease is not proof it has stopped.
	if state == "applying" {
		return tx.Commit(ctx)
	}
	if state != "leased" || generation != current || time.Now().After(until) {
		return ErrConflict
	}
	var busy bool
	if e = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM config_outbox WHERE node_id=$1 AND state='applying')", nodeID).Scan(&busy); e != nil {
		return e
	}
	if busy {
		return ErrConflict
	}
	var cfg NodeConfig
	if e = json.Unmarshal(payload, &cfg); e != nil {
		return e
	}
	for _, r := range cfg.Rules {
		if _, e = tx.Exec(ctx, "UPDATE relay_rules SET ever_dispatched=true WHERE id=$1 AND node_id=$2", r.ID, nodeID); e != nil {
			return e
		}
	}
	if _, e = tx.Exec(ctx, "UPDATE config_outbox SET state='applying' WHERE id=$1", jobID); e != nil {
		return e
	}
	return tx.Commit(ctx)
}
