package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"reladock/internal/probe"
)

// MaxProbeReportAge bounds first receipt using the database clock. Exact latest
// replays remain idempotent, but still require current credentials/configuration.
const MaxProbeReportAge = 5 * time.Minute
const maxProbeLatencyMillis = 11000.0

var probeRuleID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func validProbeResult(result probe.Result) bool {
	if !nodeIDPattern.MatchString(result.NodeID) || !probeRuleID.MatchString(result.RuleID) || result.Generation < 1 || result.ObservedAt.IsZero() {
		return false
	}
	switch result.Type {
	case probe.UDP:
		return result.Status == probe.Unknown && result.LatencyMillis == nil
	case probe.TCP:
		if result.Status != probe.TargetReachable && result.Status != probe.TargetUnreachable || result.LatencyMillis == nil {
			return false
		}
		v := *result.LatencyMillis
		return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= maxProbeLatencyMillis
	default:
		// HTTP requires an operator-bound endpoint policy that is not yet stored.
		return false
	}
}

// ReportProbe accepts metadata-only observations from an mTLS-authenticated
// node. Unlike trusted config helpers, credentials are mandatory: exactly one
// enrolled fingerprint, rechecked AFTER the node lock. It performs no probing.
//
// Acceptance requires the current desired generation to be applied, the exact
// rule version enabled in that immutable snapshot, and matching tenant/target/
// protocol bindings. "pending-agent" is this schema's enabled snapshot status;
// only equality with applied_status plus an applied ACK makes it eligible here.
// This does not change rule state or establish end-to-end health.
func (s *Store) ReportProbe(ctx context.Context, nodeID string, result probe.Result, credential ...string) error {
	if len(credential) != 1 || !fingerprintPattern.MatchString(credential[0]) {
		return ErrUnauthorized
	}
	if result.NodeID != nodeID || !validProbeResult(result) {
		return ErrInvalid
	}
	result.ObservedAt = result.ObservedAt.UTC()
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

	var tenantID string
	var rule Rule
	var deliveryGeneration int64
	err = tx.QueryRow(ctx, `SELECT tenant_id,name,protocol,target_host,target_port,bandwidth_mbps,node_id,listen_port,status,desired_version,applied_version,applied_status,delivery_generation FROM relay_rules WHERE id=$1 AND node_id=$2 FOR SHARE`, result.RuleID, nodeID).Scan(&tenantID, &rule.Name, &rule.Protocol, &rule.TargetHost, &rule.TargetPort, &rule.BandwidthMbps, &rule.NodeID, &rule.ListenPort, &rule.Status, &rule.DesiredVersion, &rule.AppliedVersion, &rule.AppliedStatus, &deliveryGeneration)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	rule.ID = result.RuleID
	if rule.Status != "pending-agent" || rule.AppliedStatus != "pending-agent" || rule.AppliedVersion != rule.DesiredVersion || rule.AppliedVersion < 1 || deliveryGeneration != result.Generation {
		return ErrConflict
	}
	if result.Type == probe.TCP && rule.Protocol != "tcp" && rule.Protocol != "both" || result.Type == probe.UDP && rule.Protocol != "udp" && rule.Protocol != "both" {
		return ErrInvalid
	}

	var desired, applied int64
	var state, checksum string
	var payload []byte
	var acknowledged *time.Time
	var now time.Time
	err = tx.QueryRow(ctx, `SELECT n.desired_generation,n.applied_generation,COALESCE(o.state,''),COALESCE(o.payload,''::bytea),COALESCE(o.checksum,''),o.reported_at,clock_timestamp() FROM nodes n LEFT JOIN config_outbox o ON o.node_id=n.id AND o.generation=$2 WHERE n.id=$1`, nodeID, result.Generation).Scan(&desired, &applied, &state, &payload, &checksum, &acknowledged, &now)
	if err != nil {
		return err
	}
	if desired != result.Generation || applied != result.Generation || state != "applied" || acknowledged == nil {
		return ErrConflict
	}
	// Agent observation and database ACK timestamps have independent clocks.
	// The applied snapshot above is the authority, not cross-clock time ordering.
	if result.ObservedAt.After(now) {
		return ErrConflict
	}
	if len(payload) > 2<<20 {
		return ErrConflict
	}
	hash := sha256.Sum256(payload)
	if hex.EncodeToString(hash[:]) != checksum {
		return ErrConflict
	}
	var cfg NodeConfig
	if json.Unmarshal(payload, &cfg) != nil || cfg.NodeID != nodeID || cfg.Generation != result.Generation || cfg.SchemaVersion != 1 {
		return ErrConflict
	}
	matches := 0
	for _, r := range cfg.Rules {
		if r.ID != rule.ID {
			continue
		}
		matches++
		if r.TenantID != tenantID || r.RuleInput != rule.RuleInput || r.Version != rule.DesiredVersion || r.ListenPort != rule.ListenPort || r.Status != "pending-agent" {
			return ErrConflict
		}
	}
	if matches != 1 {
		return ErrConflict
	}

	canonical, err := json.Marshal(result)
	if err != nil {
		return ErrInvalid
	}
	reportHash := sha256.Sum256(canonical)
	digest := hex.EncodeToString(reportHash[:])
	var previousGeneration, previousNS int64
	var previousHash string
	err = tx.QueryRow(ctx, `SELECT generation,observed_ns,report_hash FROM probe_latest WHERE rule_id=$1 AND probe_type=$2 FOR UPDATE`, rule.ID, string(result.Type)).Scan(&previousGeneration, &previousNS, &previousHash)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if err == nil {
		if previousGeneration == result.Generation && previousHash == digest {
			return nil
		}
		if previousGeneration > result.Generation || previousNS >= result.ObservedAt.UnixNano() {
			return ErrConflict
		}
	}
	if now.Sub(result.ObservedAt) > MaxProbeReportAge {
		return ErrConflict
	}
	// observed_ns preserves producer nanoseconds: PostgreSQL timestamptz alone
	// rounds to microseconds and cannot distinguish exact replay from reordering.
	_, err = tx.Exec(ctx, `INSERT INTO probe_latest(rule_id,tenant_id,node_id,generation,probe_type,status,observed_at,observed_ns,latency_ms,report_hash) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT(rule_id,probe_type) DO UPDATE SET tenant_id=EXCLUDED.tenant_id,node_id=EXCLUDED.node_id,generation=EXCLUDED.generation,status=EXCLUDED.status,observed_at=EXCLUDED.observed_at,observed_ns=EXCLUDED.observed_ns,latency_ms=EXCLUDED.latency_ms,report_hash=EXCLUDED.report_hash,received_at=clock_timestamp()`, rule.ID, tenantID, nodeID, result.Generation, string(result.Type), string(result.Status), result.ObservedAt, result.ObservedAt.UnixNano(), result.LatencyMillis, digest)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ProbeResults returns at most one TCP and one UDP observation for a tenant's
// current applied/enabled rule generation. Deleted/foreign rules return 404;
// pending, paused or unapplied configurations return an empty list. Consumers
// MUST assess ObservedAt freshness; old results never imply current health.
func (s *Store) ProbeResults(ctx context.Context, who Identity, ruleID string) ([]probe.Result, error) {
	if !probeRuleID.MatchString(ruleID) {
		return nil, ErrInvalid
	}
	tx, _, err := s.memberTx(ctx, who, false)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var id string
	err = tx.QueryRow(ctx, `SELECT id FROM relay_rules WHERE id=$1 AND tenant_id=$2 AND status<>'deleted' FOR SHARE`, ruleID, who.TenantID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT p.node_id,p.rule_id,p.generation,p.observed_ns,p.probe_type,p.status,p.latency_ms FROM probe_latest p JOIN relay_rules r ON r.id=p.rule_id AND r.tenant_id=p.tenant_id JOIN nodes n ON n.id=r.node_id JOIN config_outbox o ON o.node_id=n.id AND o.generation=p.generation WHERE p.rule_id=$1 AND p.tenant_id=$2 AND p.node_id=r.node_id AND p.generation=n.applied_generation AND p.generation=n.desired_generation AND p.generation=r.delivery_generation AND r.status='pending-agent' AND r.applied_status='pending-agent' AND r.applied_version=r.desired_version AND o.state='applied' ORDER BY p.probe_type LIMIT 2`, ruleID, who.TenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	results := []probe.Result{}
	for rows.Next() {
		var result probe.Result
		var ns int64
		var kind, status string
		if err = rows.Scan(&result.NodeID, &result.RuleID, &result.Generation, &ns, &kind, &status, &result.LatencyMillis); err != nil {
			return nil, err
		}
		result.Type = probe.Type(kind)
		result.Status = probe.Status(status)
		result.ObservedAt = time.Unix(0, ns).UTC()
		results = append(results, result)
	}
	return results, rows.Err()
}
