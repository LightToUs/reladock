package platform

import (
	"context"
	"regexp"
	"time"
)

const AgentHeartbeatStaleAfter = 90 * time.Second

var agentVersionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/+:-]{0,79}$`)

type AgentHeartbeat struct {
	// Negotiated via HTTP header so older strict JSON decoders keep accepting heartbeats.
	CertificateRotationSupported bool                `json:"-"`
	Version                      string              `json:"version"`
	Mode                         string              `json:"mode"`
	StartedAt                    time.Time           `json:"startedAt"`
	CertificateNotAfter          time.Time           `json:"certificateNotAfter"`
	RotationState                string              `json:"rotationState"`
	RotationErrorCode            string              `json:"rotationErrorCode"`
	KernelBootID                 string              `json:"kernelBootId,omitempty"`
	JournalSequence              int64               `json:"journalSequence,omitempty"`
	Resources                    ResourceObservation `json:"resources"`
}

type ResourceObservation struct {
	SampledAt        time.Time `json:"sampledAt"`
	CPUPercent       *int      `json:"cpuPercent"`
	MemoryPercent    *int      `json:"memoryPercent"`
	DiskPercent      *int      `json:"diskPercent"`
	ConntrackPercent *int      `json:"conntrackPercent"`
	DataplaneState   string    `json:"dataplaneState"`
}

func (r ResourceObservation) present() bool {
	return !r.SampledAt.IsZero() || r.CPUPercent != nil || r.MemoryPercent != nil || r.DiskPercent != nil || r.ConntrackPercent != nil || r.DataplaneState != ""
}

func validPercent(value *int) bool { return value == nil || *value >= 0 && *value <= 100 }

func validateResourceObservation(r ResourceObservation, mode string, now time.Time) error {
	if !r.present() { // Rolling upgrades may briefly send the pre-v24 heartbeat.
		return nil
	}
	if r.SampledAt.IsZero() || r.SampledAt.After(now.Add(time.Minute)) || r.SampledAt.Before(now.Add(-7*24*time.Hour)) || !validPercent(r.CPUPercent) || !validPercent(r.MemoryPercent) || !validPercent(r.DiskPercent) || !validPercent(r.ConntrackPercent) {
		return ErrInvalid
	}
	switch r.DataplaneState {
	case "not-applicable":
		if mode != "validation" {
			return ErrInvalid
		}
	case "in-sync", "drifted":
		if mode != "apply" {
			return ErrInvalid
		}
	case "unknown":
	default:
		return ErrInvalid
	}
	return nil
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
	if !agentVersionPattern.MatchString(h.Version) || (h.Mode != "validation" && h.Mode != "apply") || h.StartedAt.IsZero() || h.StartedAt.After(now.Add(time.Minute)) || h.CertificateNotAfter.IsZero() || !h.CertificateNotAfter.After(now) || h.KernelBootID != "" && !upgradeIDPattern.MatchString(h.KernelBootID) || h.JournalSequence < 0 {
		return ErrInvalid
	}
	if err := validateResourceObservation(h.Resources, h.Mode, now); err != nil {
		return err
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
	result, err := tx.Exec(ctx, `UPDATE nodes SET
last_seen_at=now(),agent_heartbeat_at=now(),agent_version=$2,agent_mode=$3,agent_started_at=$4,agent_certificate_not_after=$5,
agent_rotation_error_since=CASE WHEN $6='error' AND (agent_rotation_state<>'error' OR agent_rotation_error_code<>$7 OR agent_rotation_error_since IS NULL) THEN now() WHEN $6='error' THEN agent_rotation_error_since ELSE NULL END,
agent_rotation_state=$6,agent_rotation_error_code=$7
		,agent_kernel_boot_id=NULLIF($8,'')::uuid,agent_journal_sequence=$9
		,agent_certificate_rotation_supported=$10
WHERE id=$1 AND agent_journal_sequence<=$9`, nodeID, heartbeat.Version, heartbeat.Mode, heartbeat.StartedAt, heartbeat.CertificateNotAfter, heartbeat.RotationState, heartbeat.RotationErrorCode, heartbeat.KernelBootID, heartbeat.JournalSequence, heartbeat.CertificateRotationSupported)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrConflict
	}
	if heartbeat.Resources.present() {
		r := heartbeat.Resources
		_, err = tx.Exec(ctx, `INSERT INTO node_resource_latest(
node_id,sampled_at,received_at,cpu_percent,memory_percent,disk_percent,conntrack_percent,clock_skew_seconds,dataplane_state,
cpu_high_since,memory_high_since,disk_high_since,conntrack_high_since,clock_skew_since,dataplane_drift_since)
VALUES($1,$2::timestamptz,clock_timestamp(),$3::smallint,$4::smallint,$5::smallint,$6::smallint,floor(abs(extract(epoch FROM clock_timestamp()-$2::timestamptz)))::integer,$7::text,
 CASE WHEN $3::smallint>=90 THEN clock_timestamp() END,
 CASE WHEN $4::smallint>=90 THEN clock_timestamp() END,
 CASE WHEN $5::smallint>=85 THEN clock_timestamp() END,
 CASE WHEN $6::smallint>=80 THEN clock_timestamp() END,
 CASE WHEN abs(extract(epoch FROM clock_timestamp()-$2::timestamptz))>120 THEN clock_timestamp() END,
 CASE WHEN $7::text='drifted' THEN clock_timestamp() END)
ON CONFLICT(node_id) DO UPDATE SET
 sampled_at=EXCLUDED.sampled_at,received_at=EXCLUDED.received_at,cpu_percent=EXCLUDED.cpu_percent,memory_percent=EXCLUDED.memory_percent,disk_percent=EXCLUDED.disk_percent,conntrack_percent=EXCLUDED.conntrack_percent,clock_skew_seconds=EXCLUDED.clock_skew_seconds,dataplane_state=EXCLUDED.dataplane_state,
 cpu_high_since=CASE WHEN EXCLUDED.cpu_percent>=90 THEN COALESCE(node_resource_latest.cpu_high_since,EXCLUDED.received_at) END,
 memory_high_since=CASE WHEN EXCLUDED.memory_percent>=90 THEN COALESCE(node_resource_latest.memory_high_since,EXCLUDED.received_at) END,
 disk_high_since=CASE WHEN EXCLUDED.disk_percent>=85 THEN COALESCE(node_resource_latest.disk_high_since,EXCLUDED.received_at) END,
 conntrack_high_since=CASE WHEN EXCLUDED.conntrack_percent>=80 THEN COALESCE(node_resource_latest.conntrack_high_since,EXCLUDED.received_at) END,
 clock_skew_since=CASE WHEN EXCLUDED.clock_skew_seconds>120 THEN COALESCE(node_resource_latest.clock_skew_since,EXCLUDED.received_at) END,
 dataplane_drift_since=CASE WHEN EXCLUDED.dataplane_state='drifted' THEN COALESCE(node_resource_latest.dataplane_drift_since,EXCLUDED.received_at) END`, nodeID, r.SampledAt, r.CPUPercent, r.MemoryPercent, r.DiskPercent, r.ConntrackPercent, r.DataplaneState)
		if err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE node_drain_attestations SET consumed_at=COALESCE(consumed_at,clock_timestamp()) WHERE node_id=$1 AND consumed_at IS NULL AND (certificate_fingerprint<>$2 OR kernel_boot_id::text<>$3 OR agent_started_at<>$4 OR agent_version<>$5 OR journal_sequence<>$6)`, nodeID, fingerprint, heartbeat.KernelBootID, heartbeat.StartedAt, heartbeat.Version, heartbeat.JournalSequence); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
