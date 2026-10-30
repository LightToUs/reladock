package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

type ConfigRule struct {
	ID       string `json:"id"`
	TenantID string `json:"tenantId"`
	RuleInput
	ListenPort         int        `json:"listenPort"`
	Version            int64      `json:"version"`
	Status             string     `json:"status"`
	SubscriptionID     string     `json:"subscriptionId,omitempty"`
	ExpiresAt          *time.Time `json:"expiresAt,omitempty"`
	QuotaBytes         int64      `json:"quotaBytes,omitempty"`
	UsageUploadBytes   int64      `json:"usageUploadBytes,omitempty"`
	UsageDownloadBytes int64      `json:"usageDownloadBytes,omitempty"`
	DevelopmentBypass  bool       `json:"developmentBypass,omitempty"`
}
type NodeConfig struct {
	SchemaVersion int          `json:"schemaVersion"`
	NodeID        string       `json:"nodeId"`
	Generation    int64        `json:"generation"`
	ListenAddress string       `json:"listenAddress"`
	Rules         []ConfigRule `json:"rules"`
}
type ConfigJob struct {
	ID         string          `json:"id"`
	NodeID     string          `json:"nodeId"`
	Generation int64           `json:"generation"`
	Payload    json.RawMessage `json:"payload"`
	Checksum   string          `json:"checksum"`
	LeaseToken string          `json:"leaseToken"`
	LeaseUntil time.Time       `json:"leaseUntil"`
	Attempts   int             `json:"attempts"`
}
type ConfigReport struct {
	LeaseToken string `json:"leaseToken"`
	Checksum   string `json:"checksum"`
	Outcome    string `json:"outcome"`
	ErrorCode  string `json:"errorCode"`
}

// Caller holds the node lock. Snapshot and node generation commit with the mutation.
func (s *Store) enqueueConfig(ctx context.Context, tx pgx.Tx, nodeID string) error {
	var cfg NodeConfig
	cfg.SchemaVersion = 1
	cfg.NodeID = nodeID
	cfg.Rules = []ConfigRule{}
	if err := tx.QueryRow(ctx, "UPDATE nodes SET desired_generation=desired_generation+1 WHERE id=$1 RETURNING desired_generation,host(listen_address)", nodeID).Scan(&cfg.Generation, &cfg.ListenAddress); err != nil {
		return err
	}
	var entitlementSchema bool
	if err := tx.QueryRow(ctx, "SELECT to_regclass('subscription_usage') IS NOT NULL").Scan(&entitlementSchema); err != nil {
		return err
	}
	query := `SELECT id,tenant_id,name,protocol,target_host,target_port,bandwidth_mbps,node_id,listen_port,desired_version,status,''::text,false,NULL::timestamptz,0::bigint,0::bigint,0::bigint FROM relay_rules WHERE node_id=$1 AND status<>'deleted' ORDER BY id`
	if entitlementSchema {
		query = `SELECT r.id,r.tenant_id,r.name,r.protocol,r.target_host,r.target_port,r.bandwidth_mbps,r.node_id,r.listen_port,r.desired_version,r.status,COALESCE(p.id::text,''),COALESCE(p.ends_at>clock_timestamp() AND COALESCE(u.upload_bytes,0)::numeric+COALESCE(u.download_bytes,0)::numeric<v.quota_bytes,false),p.ends_at,COALESCE(v.quota_bytes,0),COALESCE(u.upload_bytes,0),COALESCE(u.download_bytes,0) FROM relay_rules r LEFT JOIN LATERAL (SELECT id,price_version_id,ends_at FROM rule_subscriptions WHERE rule_id=r.id ORDER BY ordinal DESC LIMIT 1) p ON true LEFT JOIN plan_price_versions v ON v.id=p.price_version_id LEFT JOIN subscription_usage u ON u.subscription_id=p.id WHERE r.node_id=$1 AND r.status<>'deleted' ORDER BY r.id`
	}
	rows, err := tx.Query(ctx, query, nodeID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var r ConfigRule
		var entitled bool
		if err = rows.Scan(&r.ID, &r.TenantID, &r.Name, &r.Protocol, &r.TargetHost, &r.TargetPort, &r.BandwidthMbps, &r.NodeID, &r.ListenPort, &r.Version, &r.Status, &r.SubscriptionID, &entitled, &r.ExpiresAt, &r.QuotaBytes, &r.UsageUploadBytes, &r.UsageDownloadBytes); err != nil {
			rows.Close()
			return err
		}
		// Unpaid or expired drafts may reserve ports but never enable forwarding.
		// The legacy path exists only while migrating v1/v2 before billing tables;
		// migration 11 replaces all historical snapshots using this entitlement gate.
		if s.allowUnentitledRules && r.Status == "pending-agent" {
			r.DevelopmentBypass = true
			r.SubscriptionID = ""
			r.ExpiresAt = nil
			r.QuotaBytes = 0
			r.UsageUploadBytes = 0
			r.UsageDownloadBytes = 0
		} else if entitlementSchema && !entitled {
			r.Status = "paused"
		}
		cfg.Rules = append(cfg.Rules, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	payload, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if len(payload) > 2*1024*1024 {
		return ErrCapacity
	}
	hash := sha256.Sum256(payload)
	if _, err = tx.Exec(ctx, "UPDATE config_outbox SET state='superseded' WHERE node_id=$1 AND state IN ('queued','failed')", nodeID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO config_outbox(id,node_id,generation,payload,checksum,state) VALUES($1,$2,$3,$4,$5,'queued')", newID(), nodeID, cfg.Generation, payload, hex.EncodeToString(hash[:])); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "UPDATE relay_rules SET delivery_generation=$1 WHERE node_id=$2 AND status<>'deleted'", cfg.Generation, nodeID)
	return err
}

func (s *Store) ClaimConfig(ctx context.Context, nodeID, agentVersion string, credential ...string) (*ConfigJob, error) {
	if !nodeIDPattern.MatchString(nodeID) || len(agentVersion) < 1 || len(agentVersion) > 80 {
		return nil, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if err = lockNode(ctx, tx, nodeID); err != nil {
		return nil, err
	}
	if err = checkNodeCredential(ctx, tx, nodeID, credential); err != nil {
		return nil, err
	}
	result, err := tx.Exec(ctx, "UPDATE nodes SET last_seen_at=now(),agent_version=$2 WHERE id=$1", nodeID, agentVersion)
	if err != nil {
		return nil, err
	}
	if result.RowsAffected() == 0 {
		return nil, ErrNotFound
	}
	var applying bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM config_outbox WHERE node_id=$1 AND state='applying')", nodeID).Scan(&applying); err != nil {
		return nil, err
	}
	if applying {
		return nil, tx.Commit(ctx)
	}
	if _, err = tx.Exec(ctx, "UPDATE config_outbox SET state='failed',error_code='lease-exhausted',reported_at=now() WHERE node_id=$1 AND state='leased' AND lease_until<=now() AND attempts>=5", nodeID); err != nil {
		return nil, err
	}
	var job ConfigJob
	err = tx.QueryRow(ctx, `SELECT id,node_id,generation,payload,checksum,attempts FROM config_outbox WHERE node_id=$1 AND generation=(SELECT desired_generation FROM nodes WHERE id=$1) AND attempts<5 AND ((state IN ('queued','failed') AND next_attempt_at<=now()) OR (state='leased' AND lease_until<=now())) FOR UPDATE`, nodeID).Scan(&job.ID, &job.NodeID, &job.Generation, &job.Payload, &job.Checksum, &job.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, tx.Commit(ctx)
	}
	if err != nil {
		return nil, err
	}
	job.LeaseToken = sessionToken()
	job.LeaseUntil = time.Now().Add(time.Minute)
	job.Attempts++
	if _, err = tx.Exec(ctx, "UPDATE config_outbox SET state='leased',lease_token=$2,lease_until=$3,attempts=$4,error_code='' WHERE id=$1", job.ID, job.LeaseToken, job.LeaseUntil, job.Attempts); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &job, nil
}

func (s *Store) ReportConfig(ctx context.Context, nodeID, jobID string, report ConfigReport, credential ...string) error {
	if report.Outcome != "validated" && report.Outcome != "failed" && report.Outcome != "applied" {
		return ErrInvalid
	}
	if len(report.LeaseToken) != 43 || !fingerprintPattern.MatchString(report.Checksum) {
		return ErrInvalid
	}
	if report.Outcome != "failed" && report.ErrorCode != "" {
		return ErrInvalid
	}
	if report.Outcome == "failed" && report.ErrorCode != "invalid-config" && report.ErrorCode != "internal-error" {
		return ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = lockNode(ctx, tx, nodeID); err != nil {
		return err
	}
	if err = checkNodeCredential(ctx, tx, nodeID, credential); err != nil {
		return err
	}
	var state, token, checksum, errorCode string
	var generation, current int64
	var until time.Time
	var attempts int
	err = tx.QueryRow(ctx, `SELECT o.state,COALESCE(o.lease_token,''),o.checksum,o.error_code,o.generation,n.desired_generation,COALESCE(o.lease_until,now()),o.attempts FROM config_outbox o JOIN nodes n ON n.id=o.node_id WHERE o.id=$1 AND o.node_id=$2 FOR UPDATE OF o`, jobID, nodeID).Scan(&state, &token, &checksum, &errorCode, &generation, &current, &until, &attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if token != report.LeaseToken || checksum != report.Checksum {
		return ErrConflict
	}
	if state == report.Outcome && errorCode == report.ErrorCode {
		return nil
	}
	if state == "superseded" && generation < current {
		return nil
	}
	if report.Outcome == "applied" && state != "applying" {
		return ErrConflict
	}
	if state != "applying" && (state != "leased" || time.Now().After(until)) {
		return ErrConflict
	}
	if state == "applying" && report.Outcome == "validated" {
		return ErrConflict
	}
	outcome := report.Outcome
	if generation < current && state != "applying" {
		outcome = "superseded"
	}
	delay := time.Duration(1<<attempts) * time.Second
	if _, err = tx.Exec(ctx, "UPDATE config_outbox SET state=$2,error_code=$3,reported_at=now(),next_attempt_at=$4 WHERE id=$1", jobID, outcome, report.ErrorCode, time.Now().Add(delay)); err != nil {
		return err
	}
	if outcome == "validated" {
		if _, err = tx.Exec(ctx, "UPDATE nodes SET validated_generation=$2,last_seen_at=now() WHERE id=$1", nodeID, generation); err != nil {
			return err
		}
	}
	if outcome == "applied" {
		var payload []byte
		if err = tx.QueryRow(ctx, "SELECT payload FROM config_outbox WHERE id=$1", jobID).Scan(&payload); err != nil {
			return err
		}
		var cfg NodeConfig
		if err = json.Unmarshal(payload, &cfg); err != nil {
			return err
		}
		ids := []string{}
		for _, r := range cfg.Rules {
			ids = append(ids, r.ID)
			if _, err = tx.Exec(ctx, "UPDATE relay_rules SET applied_version=$2,applied_status=$3 WHERE id=$1 AND node_id=$4 AND applied_version<=$2", r.ID, r.Version, r.Status, nodeID); err != nil {
				return err
			}
		}
		// Absence from a full snapshot acknowledges removal, but allocation
		// reuse remains fenced until explicit conntrack reconciliation.
		if _, err = tx.Exec(ctx, "UPDATE relay_rules SET applied_version=desired_version,applied_status='deleted' WHERE node_id=$1 AND status='deleted' AND delivery_generation<=$2 AND id<>ALL($3::uuid[])", nodeID, generation, ids); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "UPDATE nodes SET applied_generation=GREATEST(applied_generation,$2),validated_generation=GREATEST(validated_generation,$2),last_seen_at=now() WHERE id=$1", nodeID, generation); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// Empty credentials are for trusted in-process callers only. Network handlers
// always supply the certificate fingerprint, rechecked under the rotation lock.
func checkNodeCredential(ctx context.Context, tx pgx.Tx, nodeID string, credential []string) error {
	if len(credential) == 0 {
		return nil
	}
	if len(credential) != 1 {
		return ErrUnauthorized
	}
	var id string
	err := tx.QueryRow(ctx, "SELECT id FROM nodes WHERE id=$1 AND certificate_fingerprint=$2", nodeID, credential[0]).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrUnauthorized
	}
	return err
}
