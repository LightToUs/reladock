package platform

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func tenantWriteAccess(ctx context.Context, tx pgx.Tx, who Identity) error {
	var role string
	err := tx.QueryRow(ctx, "SELECT m.role FROM members m JOIN tenants t ON t.id=m.tenant_id WHERE m.id=$1 AND m.tenant_id=$2 AND t.status='active' FOR SHARE OF t,m", who.MemberID, who.TenantID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrUnauthorized
	}
	if err != nil {
		return err
	}
	if role != "owner" && role != "admin" {
		return ErrForbidden
	}
	return CheckAPIKeyWrite(ctx, tx, who)
}

func (s *Store) AdminSetRuleSourceLogging(ctx context.Context, who Identity, tenantID, id string, desiredVersion int64, enabled bool) (Rule, error) {
	if !billingUUID.MatchString(tenantID) || !billingUUID.MatchString(id) || desiredVersion < 1 {
		return Rule{}, ErrInvalid
	}
	if enabled && (s.sourceFlowRetentionDays < 30 || s.sourceFlowRetentionDays > 365) {
		return Rule{}, ErrSourceFlowRetentionUnavailable
	}
	tx, err := s.adminTx(ctx, who)
	if err != nil {
		return Rule{}, err
	}
	defer tx.Rollback(ctx)
	var nodeID string
	err = tx.QueryRow(ctx, `SELECT node_id FROM relay_rules WHERE id=$1 AND tenant_id=$2 AND status<>'deleted'`, id, tenantID).Scan(&nodeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Rule{}, ErrNotFound
	}
	if err != nil {
		return Rule{}, err
	}
	if err = lockNode(ctx, tx, nodeID); err != nil {
		return Rule{}, err
	}
	rule, err := scanRule(tx.QueryRow(ctx, `SELECT `+ruleColumns+` FROM relay_rules WHERE id=$1 AND tenant_id=$2 AND status<>'deleted' FOR UPDATE`, id, tenantID))
	if err != nil {
		return Rule{}, err
	}
	if rule.DesiredVersion != desiredVersion {
		return Rule{}, ErrConflict
	}
	if rule.SourceLoggingEnabled == enabled {
		if err = tx.Commit(ctx); err != nil {
			return Rule{}, err
		}
		return rule, nil
	}
	if enabled {
		var supported bool
		var mode string
		var heartbeatAt *time.Time
		if err = tx.QueryRow(ctx, `SELECT agent_source_flow_supported,agent_mode,agent_heartbeat_at FROM nodes WHERE id=$1`, nodeID).Scan(&supported, &mode, &heartbeatAt); err != nil {
			return Rule{}, err
		}
		if heartbeatAt == nil || heartbeatAt.Before(time.Now().UTC().Add(-AgentHeartbeatStaleAfter)) {
			return Rule{}, ErrSourceFlowAgentOffline
		}
		if !supported || mode != "apply" {
			return Rule{}, ErrSourceFlowAgentUnsupported
		}
	}
	rule, err = scanRule(tx.QueryRow(ctx, `UPDATE relay_rules SET source_logging_enabled=$1,desired_version=desired_version+1,updated_at=now() WHERE id=$2 AND tenant_id=$3 RETURNING `+ruleColumns, enabled, id, tenantID))
	if err != nil {
		return Rule{}, err
	}
	if err = s.enqueueConfig(ctx, tx, nodeID); err != nil {
		return Rule{}, err
	}
	action := "disable"
	if enabled {
		action = "enable"
	}
	if err = platformLog(ctx, tx, &who, "rule.source-logging."+action, id, map[string]any{"tenantId": tenantID, "desiredVersion": rule.DesiredVersion}); err != nil {
		return Rule{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Rule{}, err
	}
	return rule, nil
}

type ManagedRule struct {
	Rule
	TenantID string `json:"tenantId"`
	Company  string `json:"company"`
}

func (s *Store) AdminRules(ctx context.Context, who Identity, tenantID, nodeID string, page int) (Page[ManagedRule], error) {
	result := Page[ManagedRule]{Items: []ManagedRule{}, Page: page, PageSize: 50}
	if page < 1 || page > 100000 {
		return result, ErrInvalid
	}
	tx, err := s.adminReadTx(ctx, who)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	from := ` FROM relay_rules r JOIN tenants t ON t.id=r.tenant_id`
	where := ` WHERE r.status<>'deleted' AND ($1='' OR r.tenant_id=NULLIF($1,'')::uuid) AND ($2='' OR r.node_id=$2)`
	if err = tx.QueryRow(ctx, "SELECT count(*)"+from+where, tenantID, nodeID).Scan(&result.Total); err != nil {
		return result, err
	}
	cols := []string{}
	for _, col := range strings.Split(ruleColumns, ",") {
		cols = append(cols, "r."+col)
	}
	rows, err := tx.Query(ctx, `SELECT r.tenant_id,t.name,`+strings.Join(cols, ",")+`,n.agent_source_flow_supported,n.agent_mode,
COALESCE(n.agent_heartbeat_at>=$4,false),COALESCE(sf.received_at>=$4,false),
CASE WHEN sf.received_at>=$4 THEN sf.collector_status ELSE 'unavailable' END
`+from+` JOIN nodes n ON n.id=r.node_id LEFT JOIN node_source_flow_latest sf ON sf.node_id=r.node_id`+where+`
ORDER BY r.created_at DESC,r.id LIMIT 50 OFFSET $3`, tenantID, nodeID, (page-1)*50, time.Now().UTC().Add(-AgentHeartbeatStaleAfter))
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var r ManagedRule
		if err = rows.Scan(&r.TenantID, &r.Company, &r.ID, &r.Name, &r.Protocol, &r.TargetHost, &r.TargetPort, &r.BandwidthMbps, &r.NodeID, &r.ListenPort, &r.Status, &r.DesiredVersion, &r.AppliedVersion, &r.CreatedAt, &r.UpdatedAt, &r.AppliedStatus, &r.SourceLoggingEnabled, &r.SourceLoggingAppliedEnabled, &r.SourceLoggingAppliedVersion, &r.SourceLoggingAppliedGeneration, &r.SourceLoggingSupported, &r.SourceLoggingAgentMode, &r.SourceLoggingHeartbeatFresh, &r.SourceLoggingStatusFresh, &r.SourceLoggingCollectorStatus); err != nil {
			return result, err
		}
		r.SourceLoggingUnsupported = !r.SourceLoggingSupported || r.SourceLoggingAgentMode != "apply" || !r.SourceLoggingHeartbeatFresh
		result.Items = append(result.Items, r)
	}
	return result, rows.Err()
}
func (s *Store) AdminChangeRule(ctx context.Context, who Identity, tenantID, id, action string, version int64) (Rule, error) {
	status := map[string]string{"pause": "paused", "resume": "pending-agent", "delete": "deleted"}[action]
	if status == "" || version < 1 {
		return Rule{}, ErrInvalid
	}
	tx, err := s.adminTx(ctx, who)
	if err != nil {
		return Rule{}, err
	}
	defer tx.Rollback(ctx)
	rule, changed, err := s.changeRuleState(ctx, tx, tenantID, id, status, &version)
	if err != nil {
		return Rule{}, err
	}
	if !changed {
		return rule, nil
	}
	if err = platformLog(ctx, tx, &who, "rule."+action, id, map[string]any{"tenantId": tenantID, "desiredVersion": rule.DesiredVersion}); err != nil {
		return Rule{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Rule{}, err
	}
	return rule, nil
}
