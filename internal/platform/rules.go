package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

type RuleInput struct {
	Name          string `json:"name"`
	Protocol      string `json:"protocol"`
	TargetHost    string `json:"targetHost"`
	TargetPort    int    `json:"targetPort"`
	BandwidthMbps int    `json:"bandwidthMbps"`
	NodeID        string `json:"nodeId"`
}
type Rule struct {
	ID string `json:"id"`
	RuleInput
	ListenPort     int       `json:"listenPort"`
	Status         string    `json:"status"`
	DesiredVersion int64     `json:"desiredVersion"`
	AppliedVersion int64     `json:"appliedVersion"`
	AppliedStatus  string    `json:"appliedStatus"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}
type AuditLog struct {
	ID         int64     `json:"id"`
	ActorID    string    `json:"actorId"`
	Action     string    `json:"action"`
	ResourceID string    `json:"resourceId"`
	CreatedAt  time.Time `json:"createdAt"`
}

var hostname = regexp.MustCompile(`^(?i:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)(?:\.(?i:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?))+$`)
var idempotencyKey = regexp.MustCompile(`^[a-zA-Z0-9._:-]{8,128}$`)

func validateRule(input RuleInput) (RuleInput, error) {
	input.Name = strings.TrimSpace(input.Name)
	input.TargetHost = strings.ToLower(strings.TrimSpace(input.TargetHost))
	if input.NodeID == "" {
		input.NodeID = "hk-01"
	}
	if !utf8.ValidString(input.Name) || utf8.RuneCountInString(input.Name) < 1 || utf8.RuneCountInString(input.Name) > 80 || len(input.TargetHost) > 253 || (net.ParseIP(input.TargetHost) == nil && !hostname.MatchString(input.TargetHost)) || input.TargetPort < 1 || input.TargetPort > 65535 || input.BandwidthMbps < 1 || input.BandwidthMbps > 1000 {
		return RuleInput{}, ErrInvalid
	}
	if input.Protocol != "tcp" && input.Protocol != "udp" && input.Protocol != "both" {
		return RuleInput{}, ErrInvalid
	}
	if len(input.NodeID) > 80 {
		return RuleInput{}, ErrInvalid
	}
	return input, nil
}

const ruleColumns = "id,name,protocol,target_host,target_port,bandwidth_mbps,node_id,listen_port,status,desired_version,applied_version,created_at,updated_at,applied_status"

func scanRule(row pgx.Row) (Rule, error) {
	var r Rule
	err := row.Scan(&r.ID, &r.Name, &r.Protocol, &r.TargetHost, &r.TargetPort, &r.BandwidthMbps, &r.NodeID, &r.ListenPort, &r.Status, &r.DesiredVersion, &r.AppliedVersion, &r.CreatedAt, &r.UpdatedAt, &r.AppliedStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}
func canWrite(who Identity) bool { return who.Role == "owner" || who.Role == "admin" }

func (s *Store) CreateRule(ctx context.Context, who Identity, key string, input RuleInput) (Rule, error) {
	if !canWrite(who) {
		return Rule{}, ErrForbidden
	}
	input, err := validateRule(input)
	if err != nil || !idempotencyKey.MatchString(key) {
		return Rule{}, ErrInvalid
	}
	payload, _ := json.Marshal(input)
	hash := sha256.Sum256(payload)
	requestHash := hex.EncodeToString(hash[:])
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Rule{}, err
	}
	defer tx.Rollback(ctx)
	if err = tenantWriteAccess(ctx, tx, who); err != nil {
		return Rule{}, err
	}
	// Serialize replays even when the same key is submitted for different nodes.
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "reladock-request:"+who.TenantID+":"+key); err != nil {
		return Rule{}, err
	}
	var previousHash, ruleID string
	err = tx.QueryRow(ctx, "SELECT request_hash,rule_id FROM idempotency_requests WHERE tenant_id=$1 AND key=$2", who.TenantID, key).Scan(&previousHash, &ruleID)
	if err == nil {
		if previousHash != requestHash {
			return Rule{}, ErrConflict
		}
		return scanRule(tx.QueryRow(ctx, "SELECT "+ruleColumns+" FROM relay_rules WHERE id=$1 AND tenant_id=$2", ruleID, who.TenantID))
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Rule{}, err
	}
	// The unique endpoint constraint is the final authority; this node lock avoids
	// repeated collisions under concurrent allocation and covers both protocols.
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "reladock-node:"+input.NodeID); err != nil {
		return Rule{}, err
	}
	var address, nodeStatus string
	var nodeCapacity int
	if err = tx.QueryRow(ctx, "SELECT listen_address::text,status,capacity_mbps FROM nodes WHERE id=$1", input.NodeID).Scan(&address, &nodeStatus, &nodeCapacity); errors.Is(err, pgx.ErrNoRows) {
		return Rule{}, ErrNotFound
	}
	if err != nil {
		return Rule{}, err
	}
	if nodeStatus == "maintenance" {
		return Rule{}, ErrConflict
	}
	if input.BandwidthMbps > nodeCapacity {
		return Rule{}, ErrInvalid
	}
	protocols := []string{input.Protocol}
	if input.Protocol == "both" {
		protocols = []string{"tcp", "udp"}
	}
	var port int
	err = tx.QueryRow(ctx, `SELECT p FROM generate_series(20000,60000) AS p WHERE NOT EXISTS(SELECT 1 FROM endpoint_allocations e WHERE e.node_id=$1 AND e.listen_address=$2::inet AND e.listen_port=p AND e.protocol=ANY($3::text[])) ORDER BY p LIMIT 1`, input.NodeID, address, protocols).Scan(&port)
	if errors.Is(err, pgx.ErrNoRows) {
		return Rule{}, ErrCapacity
	}
	if err != nil {
		return Rule{}, err
	}
	rule, err := scanRule(tx.QueryRow(ctx, `INSERT INTO relay_rules(id,tenant_id,node_id,name,protocol,target_host,target_port,bandwidth_mbps,listen_port,status) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'pending-agent') RETURNING `+ruleColumns, newID(), who.TenantID, input.NodeID, input.Name, input.Protocol, input.TargetHost, input.TargetPort, input.BandwidthMbps, port))
	if err != nil {
		return Rule{}, err
	}
	for _, protocol := range protocols {
		if _, err = tx.Exec(ctx, "INSERT INTO endpoint_allocations(node_id,listen_address,protocol,listen_port,rule_id,tenant_id) VALUES($1,$2,$3,$4,$5,$6)", input.NodeID, address, protocol, port, rule.ID, who.TenantID); err != nil {
			return Rule{}, err
		}
	}
	if _, err = tx.Exec(ctx, "INSERT INTO idempotency_requests(tenant_id,key,request_hash,rule_id) VALUES($1,$2,$3,$4)", who.TenantID, key, requestHash, rule.ID); err != nil {
		return Rule{}, err
	}
	if err = enqueueConfig(ctx, tx, input.NodeID); err != nil {
		return Rule{}, err
	}
	if err = audit(ctx, tx, who, "rule.create", rule.ID); err != nil {
		return Rule{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Rule{}, err
	}
	return rule, nil
}

func (s *Store) ListRules(ctx context.Context, who Identity) ([]Rule, error) {
	rows, err := s.pool.Query(ctx, "SELECT "+ruleColumns+" FROM relay_rules WHERE tenant_id=$1 AND status<>'deleted' ORDER BY created_at DESC,id", who.TenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	rules := []Rule{}
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	return rules, rows.Err()
}

func (s *Store) PauseRule(ctx context.Context, who Identity, id string) (Rule, error) {
	return s.changeRule(ctx, who, id, "paused")
}
func (s *Store) ResumeRule(ctx context.Context, who Identity, id string) (Rule, error) {
	return s.changeRule(ctx, who, id, "pending-agent")
}
func (s *Store) DeleteRule(ctx context.Context, who Identity, id string) error {
	_, err := s.changeRule(ctx, who, id, "deleted")
	return err
}
func changeRuleState(ctx context.Context, tx pgx.Tx, tenantID, id, status string, expected *int64) (Rule, bool, error) {
	var nodeID string
	err := tx.QueryRow(ctx, "SELECT node_id FROM relay_rules WHERE id=$1 AND tenant_id=$2", id, tenantID).Scan(&nodeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Rule{}, false, ErrNotFound
	}
	if err != nil {
		return Rule{}, false, err
	}
	if err = lockNode(ctx, tx, nodeID); err != nil {
		return Rule{}, false, err
	}
	rule, err := scanRule(tx.QueryRow(ctx, "SELECT "+ruleColumns+" FROM relay_rules WHERE id=$1 AND tenant_id=$2 FOR UPDATE", id, tenantID))
	if err != nil {
		return Rule{}, false, err
	}
	if expected != nil && rule.DesiredVersion != *expected {
		return Rule{}, false, ErrConflict
	}
	if rule.Status == "deleted" && status != "deleted" {
		return Rule{}, false, ErrNotFound
	}
	if status == "pending-agent" {
		if err := CheckRuleResumeAllowed(ctx, tx, id); err != nil {
			return Rule{}, false, err
		}
	}
	if rule.Status == status {
		return rule, false, nil
	}
	rule, err = scanRule(tx.QueryRow(ctx, "UPDATE relay_rules SET status=$1,desired_version=desired_version+1,updated_at=now() WHERE id=$2 AND tenant_id=$3 RETURNING "+ruleColumns, status, id, tenantID))
	if err != nil {
		return Rule{}, false, err
	}
	if status == "deleted" {
		if _, err = tx.Exec(ctx, "DELETE FROM endpoint_allocations WHERE rule_id=$1 AND tenant_id=$2 AND NOT EXISTS(SELECT 1 FROM relay_rules WHERE id=$1 AND ever_dispatched)", id, tenantID); err != nil {
			return Rule{}, false, err
		}
	}
	if err = enqueueConfig(ctx, tx, nodeID); err != nil {
		return Rule{}, false, err
	}
	if status == "deleted" {
		if _, err = tx.Exec(ctx, "UPDATE relay_rules SET delivery_generation=(SELECT desired_generation FROM nodes WHERE id=$2) WHERE id=$1", id, nodeID); err != nil {
			return Rule{}, false, err
		}
	}
	return rule, true, nil
}
func (s *Store) changeRule(ctx context.Context, who Identity, id, status string) (Rule, error) {
	if !canWrite(who) {
		return Rule{}, ErrForbidden
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Rule{}, err
	}
	defer tx.Rollback(ctx)
	if err = tenantWriteAccess(ctx, tx, who); err != nil {
		return Rule{}, err
	}
	rule, changed, err := changeRuleState(ctx, tx, who.TenantID, id, status, nil)
	if err != nil {
		return Rule{}, err
	}
	if !changed {
		return rule, nil
	}
	action := map[string]string{"paused": "rule.pause", "pending-agent": "rule.resume", "deleted": "rule.delete"}[status]
	if err = audit(ctx, tx, who, action, id); err != nil {
		return Rule{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Rule{}, err
	}
	return rule, nil
}

func (s *Store) AuditLogs(ctx context.Context, who Identity) ([]AuditLog, error) {
	rows, err := s.pool.Query(ctx, "SELECT id,actor_id,action,resource_id,created_at FROM audit_logs WHERE tenant_id=$1 ORDER BY id DESC LIMIT 50", who.TenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	logs := []AuditLog{}
	for rows.Next() {
		var log AuditLog
		if err = rows.Scan(&log.ID, &log.ActorID, &log.Action, &log.ResourceID, &log.CreatedAt); err != nil {
			return nil, err
		}
		logs = append(logs, log)
	}
	return logs, rows.Err()
}
