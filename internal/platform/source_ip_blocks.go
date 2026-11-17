package platform

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const maxActiveSourceIPBlocksPerRule = 256

type SourceIPBlock struct {
	ID               string     `json:"id"`
	TenantID         string     `json:"tenantId"`
	RuleID           string     `json:"ruleId"`
	CIDR             string     `json:"cidr"`
	Reason           string     `json:"reason"`
	State            string     `json:"state"`
	RequestedVersion int64      `json:"requestedVersion"`
	RemovedVersion   int64      `json:"removedVersion,omitempty"`
	Applied          bool       `json:"applied"`
	CreatedAt        time.Time  `json:"createdAt"`
	RemovedAt        *time.Time `json:"removedAt,omitempty"`
}

type SourceIPBlockInput struct {
	CIDR           string `json:"cidr"`
	Reason         string `json:"reason"`
	DesiredVersion int64  `json:"desiredVersion"`
}

type SourceIPBlockPage struct {
	Items          []SourceIPBlock `json:"items"`
	DesiredVersion int64           `json:"desiredVersion"`
	AppliedVersion int64           `json:"appliedVersion"`
}

func canonicalSourceCIDR(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ErrInvalid
	}
	if addr, err := netip.ParseAddr(raw); err == nil {
		if !addr.Is4() || addr.IsUnspecified() || addr.IsLoopback() || addr.IsMulticast() {
			return "", ErrInvalid
		}
		return netip.PrefixFrom(addr, 32).String(), nil
	}
	prefix, err := netip.ParsePrefix(raw)
	if err != nil || !prefix.Addr().Is4() || prefix.Bits() == 0 || prefix != prefix.Masked() || prefix.String() != raw {
		return "", ErrInvalid
	}
	addr := prefix.Addr()
	if addr.IsUnspecified() || addr.IsLoopback() || addr.IsMulticast() {
		return "", ErrInvalid
	}
	return prefix.String(), nil
}

func scanSourceIPBlock(row pgx.Row) (SourceIPBlock, error) {
	var block SourceIPBlock
	var removedVersion *int64
	err := row.Scan(&block.ID, &block.TenantID, &block.RuleID, &block.CIDR, &block.Reason, &block.State,
		&block.RequestedVersion, &removedVersion, &block.CreatedAt, &block.RemovedAt, &block.Applied)
	if removedVersion != nil {
		block.RemovedVersion = *removedVersion
	}
	return block, err
}

const sourceIPBlockColumns = `b.id,b.tenant_id,b.rule_id,b.cidr::text,b.reason,b.state,b.requested_version,b.removed_version,b.created_at,b.removed_at,
 CASE WHEN b.state='active' THEN r.applied_version>=b.requested_version ELSE r.applied_version>=b.removed_version END`

func (s *Store) AdminSourceIPBlocks(ctx context.Context, who Identity, tenantID, ruleID string) (SourceIPBlockPage, error) {
	result := SourceIPBlockPage{Items: []SourceIPBlock{}}
	if !billingUUID.MatchString(tenantID) || !billingUUID.MatchString(ruleID) {
		return result, ErrInvalid
	}
	tx, err := s.adminReadTx(ctx, who)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	if err = tx.QueryRow(ctx, `SELECT desired_version,applied_version FROM relay_rules WHERE id=$1 AND tenant_id=$2 AND status<>'deleted'`, ruleID, tenantID).Scan(&result.DesiredVersion, &result.AppliedVersion); errors.Is(err, pgx.ErrNoRows) {
		return result, ErrNotFound
	} else if err != nil {
		return result, err
	}
	rows, err := tx.Query(ctx, `SELECT `+sourceIPBlockColumns+` FROM rule_source_ip_blocks b JOIN relay_rules r ON r.id=b.rule_id AND r.tenant_id=b.tenant_id WHERE b.rule_id=$1 AND b.tenant_id=$2 AND b.state='active' ORDER BY b.created_at,b.id`, ruleID, tenantID)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		block, scanErr := scanSourceIPBlock(rows)
		if scanErr != nil {
			return result, scanErr
		}
		result.Items = append(result.Items, block)
	}
	return result, rows.Err()
}

func (s *Store) AdminAddSourceIPBlock(ctx context.Context, who Identity, tenantID, ruleID string, input SourceIPBlockInput) (SourceIPBlock, error) {
	var zero SourceIPBlock
	if !billingUUID.MatchString(tenantID) || !billingUUID.MatchString(ruleID) || input.DesiredVersion < 1 {
		return zero, ErrInvalid
	}
	cidr, err := canonicalSourceCIDR(input.CIDR)
	reason := strings.TrimSpace(input.Reason)
	if err != nil || len(reason) < 1 || len(reason) > 500 {
		return zero, ErrInvalid
	}
	tx, err := s.adminTx(ctx, who)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback(ctx)
	nodeID, desiredVersion, _, err := lockSourceIPBlockRule(ctx, tx, tenantID, ruleID)
	if err != nil {
		return zero, err
	}
	if desiredVersion != input.DesiredVersion {
		return zero, ErrConflict
	}
	block, err := scanSourceIPBlock(tx.QueryRow(ctx, `SELECT `+sourceIPBlockColumns+` FROM rule_source_ip_blocks b JOIN relay_rules r ON r.id=b.rule_id AND r.tenant_id=b.tenant_id WHERE b.rule_id=$1 AND b.tenant_id=$2 AND b.cidr=$3::cidr AND b.state='active'`, ruleID, tenantID, cidr))
	if err == nil {
		if err = tx.Commit(ctx); err != nil {
			return zero, err
		}
		return block, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return zero, err
	}
	var count int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM rule_source_ip_blocks WHERE rule_id=$1 AND state='active'`, ruleID).Scan(&count); err != nil {
		return zero, err
	}
	if count >= maxActiveSourceIPBlocksPerRule {
		return zero, ErrCapacity
	}
	requestedVersion := desiredVersion + 1
	id := newID()
	block, err = scanSourceIPBlock(tx.QueryRow(ctx, `WITH inserted AS (
 INSERT INTO rule_source_ip_blocks(id,tenant_id,rule_id,cidr,reason,requested_version,created_by)
 VALUES($1,$2,$3,$4::cidr,$5,$6,$7) RETURNING *
) SELECT i.id,i.tenant_id,i.rule_id,i.cidr::text,i.reason,i.state,i.requested_version,i.removed_version,i.created_at,i.removed_at,false FROM inserted i`, id, tenantID, ruleID, cidr, reason, requestedVersion, who.MemberID))
	if err != nil {
		return zero, err
	}
	if _, err = tx.Exec(ctx, `UPDATE relay_rules SET desired_version=$1,updated_at=now() WHERE id=$2 AND tenant_id=$3`, requestedVersion, ruleID, tenantID); err != nil {
		return zero, err
	}
	if err = s.enqueueConfig(ctx, tx, nodeID); err != nil {
		return zero, err
	}
	if err = platformLog(ctx, tx, &who, "rule.source-ip-block.add", id, map[string]any{"tenantId": tenantID, "ruleId": ruleID, "cidr": cidr, "reason": reason, "desiredVersion": requestedVersion}); err != nil {
		return zero, err
	}
	if err = tx.Commit(ctx); err != nil {
		return zero, err
	}
	return block, nil
}

func (s *Store) AdminRemoveSourceIPBlock(ctx context.Context, who Identity, tenantID, ruleID, blockID string, desiredVersion int64) (SourceIPBlock, error) {
	var zero SourceIPBlock
	if !billingUUID.MatchString(tenantID) || !billingUUID.MatchString(ruleID) || !billingUUID.MatchString(blockID) || desiredVersion < 1 {
		return zero, ErrInvalid
	}
	tx, err := s.adminTx(ctx, who)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback(ctx)
	nodeID, currentVersion, appliedVersion, err := lockSourceIPBlockRule(ctx, tx, tenantID, ruleID)
	if err != nil {
		return zero, err
	}
	if currentVersion != desiredVersion {
		return zero, ErrConflict
	}
	removedVersion := currentVersion + 1
	block, err := scanSourceIPBlock(tx.QueryRow(ctx, `UPDATE rule_source_ip_blocks b SET state='removed',removed_version=$1,removed_by=$2,removed_at=now()
 WHERE b.id=$3 AND b.rule_id=$4 AND b.tenant_id=$5 AND b.state='active'
 RETURNING b.id,b.tenant_id,b.rule_id,b.cidr::text,b.reason,b.state,b.requested_version,b.removed_version,b.created_at,b.removed_at,$6>=b.removed_version`, removedVersion, who.MemberID, blockID, ruleID, tenantID, appliedVersion))
	if errors.Is(err, pgx.ErrNoRows) {
		return zero, ErrNotFound
	}
	if err != nil {
		return zero, err
	}
	if _, err = tx.Exec(ctx, `UPDATE relay_rules SET desired_version=$1,updated_at=now() WHERE id=$2 AND tenant_id=$3`, removedVersion, ruleID, tenantID); err != nil {
		return zero, err
	}
	if err = s.enqueueConfig(ctx, tx, nodeID); err != nil {
		return zero, err
	}
	if err = platformLog(ctx, tx, &who, "rule.source-ip-block.remove", blockID, map[string]any{"tenantId": tenantID, "ruleId": ruleID, "cidr": block.CIDR, "desiredVersion": removedVersion}); err != nil {
		return zero, err
	}
	if err = tx.Commit(ctx); err != nil {
		return zero, err
	}
	return block, nil
}

func lockSourceIPBlockRule(ctx context.Context, tx pgx.Tx, tenantID, ruleID string) (string, int64, int64, error) {
	var nodeID string
	if err := tx.QueryRow(ctx, `SELECT node_id FROM relay_rules WHERE id=$1 AND tenant_id=$2 AND status<>'deleted'`, ruleID, tenantID).Scan(&nodeID); errors.Is(err, pgx.ErrNoRows) {
		return "", 0, 0, ErrNotFound
	} else if err != nil {
		return "", 0, 0, err
	}
	if err := lockNode(ctx, tx, nodeID); err != nil {
		return "", 0, 0, err
	}
	var desiredVersion, appliedVersion int64
	if err := tx.QueryRow(ctx, `SELECT desired_version,applied_version FROM relay_rules WHERE id=$1 AND tenant_id=$2 AND status<>'deleted' FOR UPDATE`, ruleID, tenantID).Scan(&desiredVersion, &appliedVersion); err != nil {
		return "", 0, 0, err
	}
	return nodeID, desiredVersion, appliedVersion, nil
}
