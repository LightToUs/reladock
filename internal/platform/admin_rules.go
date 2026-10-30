package platform

import (
	"context"
	"errors"
	"strings"

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
	where := ` FROM relay_rules r JOIN tenants t ON t.id=r.tenant_id WHERE r.status<>'deleted' AND ($1='' OR r.tenant_id=NULLIF($1,'')::uuid) AND ($2='' OR r.node_id=$2)`
	if err = tx.QueryRow(ctx, "SELECT count(*)"+where, tenantID, nodeID).Scan(&result.Total); err != nil {
		return result, err
	}
	cols := []string{}
	for _, col := range strings.Split(ruleColumns, ",") {
		cols = append(cols, "r."+col)
	}
	rows, err := tx.Query(ctx, "SELECT r.tenant_id,t.name,"+strings.Join(cols, ",")+where+" ORDER BY r.created_at DESC,r.id LIMIT 50 OFFSET $3", tenantID, nodeID, (page-1)*50)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var r ManagedRule
		if err = rows.Scan(&r.TenantID, &r.Company, &r.ID, &r.Name, &r.Protocol, &r.TargetHost, &r.TargetPort, &r.BandwidthMbps, &r.NodeID, &r.ListenPort, &r.Status, &r.DesiredVersion, &r.AppliedVersion, &r.CreatedAt, &r.UpdatedAt, &r.AppliedStatus); err != nil {
			return result, err
		}
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
