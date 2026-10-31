package platform

import (
	"context"
	"regexp"
	"time"
)

const AgentHeartbeatStaleAfter = 90 * time.Second

var agentVersionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/+:-]{0,79}$`)

type AgentHeartbeat struct {
	Version             string    `json:"version"`
	Mode                string    `json:"mode"`
	StartedAt           time.Time `json:"startedAt"`
	CertificateNotAfter time.Time `json:"certificateNotAfter"`
	RotationState       string    `json:"rotationState"`
	RotationErrorCode   string    `json:"rotationErrorCode"`
}

func validRotationError(code string) bool {
	switch code {
	case "state-invalid", "key-mismatch", "certificate-invalid", "publish-failed", "renew-unavailable", "activate-unavailable":
		return true
	default:
		return false
	}
}

func validateAgentHeartbeat(h AgentHeartbeat, now time.Time) error {
	if !agentVersionPattern.MatchString(h.Version) || (h.Mode != "validation" && h.Mode != "apply") || h.StartedAt.IsZero() || h.StartedAt.After(now.Add(time.Minute)) || h.CertificateNotAfter.IsZero() || !h.CertificateNotAfter.After(now) {
		return ErrInvalid
	}
	switch h.RotationState {
	case "idle", "pending":
		if h.RotationErrorCode != "" {
			return ErrInvalid
		}
	case "error":
		if !validRotationError(h.RotationErrorCode) {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

func (s *Store) ReportAgentHeartbeat(ctx context.Context, nodeID, fingerprint string, heartbeat AgentHeartbeat) error {
	if !nodeIDPattern.MatchString(nodeID) || !fingerprintPattern.MatchString(fingerprint) {
		return ErrInvalid
	}
	now := time.Now().UTC()
	if err := validateAgentHeartbeat(heartbeat, now); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = lockNode(ctx, tx, nodeID); err != nil {
		return err
	}
	if err = checkNodeCredential(ctx, tx, nodeID, []string{fingerprint}); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE nodes SET
last_seen_at=now(),agent_heartbeat_at=now(),agent_version=$2,agent_mode=$3,agent_started_at=$4,agent_certificate_not_after=$5,
agent_rotation_error_since=CASE WHEN $6='error' AND (agent_rotation_state<>'error' OR agent_rotation_error_code<>$7 OR agent_rotation_error_since IS NULL) THEN now() WHEN $6='error' THEN agent_rotation_error_since ELSE NULL END,
agent_rotation_state=$6,agent_rotation_error_code=$7
WHERE id=$1`, nodeID, heartbeat.Version, heartbeat.Mode, heartbeat.StartedAt, heartbeat.CertificateNotAfter, heartbeat.RotationState, heartbeat.RotationErrorCode)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
