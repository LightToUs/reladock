package platform

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var planCode = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

// Every field is configured by an authorized platform administrator. Periods are
// elapsed seconds, not calendar months. Versions are immutable; create another
// version to change price, quota or bandwidth. No default plans are installed.
// Current schema permits positive prices/quotas, periods of 1..31,536,000 seconds
// and bandwidth of 1..1000 Mbps (also checked against the selected node capacity).
// Every configured version remains selectable; catalog retirement is not built.
type PlanInput struct {
	Code          string `json:"code"`
	Version       int64  `json:"version"`
	Name          string `json:"name"`
	PriceCents    int64  `json:"priceCents"`
	QuotaBytes    int64  `json:"quotaBytes"`
	BandwidthMbps int    `json:"bandwidthMbps"`
	PeriodSeconds int64  `json:"periodSeconds"`
}

type PlanPriceVersion struct {
	ID string `json:"id"`
	PlanInput
	Currency  string    `json:"currency"`
	CreatedAt time.Time `json:"createdAt"`
}

const planColumns = "id,code,version,name,price_cents,quota_bytes,bandwidth_mbps,period_seconds,currency,created_at"

func scanPlan(row pgx.Row) (PlanPriceVersion, error) {
	var p PlanPriceVersion
	err := row.Scan(&p.ID, &p.Code, &p.Version, &p.Name, &p.PriceCents, &p.QuotaBytes, &p.BandwidthMbps, &p.PeriodSeconds, &p.Currency, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

func (s *Store) AdminCreatePlan(ctx context.Context, who Identity, input PlanInput) (PlanPriceVersion, error) {
	tx, err := s.adminTx(ctx, who)
	if err != nil {
		return PlanPriceVersion{}, err
	}
	defer tx.Rollback(ctx)
	input.Name = strings.TrimSpace(input.Name)
	if !planCode.MatchString(input.Code) || input.Version < 1 || input.Version > 1000000 || !billingText(input.Name, 120) || input.PriceCents <= 0 || input.QuotaBytes <= 0 || input.BandwidthMbps < 1 || input.BandwidthMbps > 1000 || input.PeriodSeconds < 1 || input.PeriodSeconds > 31536000 {
		return PlanPriceVersion{}, ErrInvalid
	}
	old, err := scanPlan(tx.QueryRow(ctx, "SELECT "+planColumns+" FROM plan_price_versions WHERE code=$1 AND version=$2", input.Code, input.Version))
	if err == nil {
		if old.PlanInput != input {
			return PlanPriceVersion{}, ErrConflict
		}
		return old, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return PlanPriceVersion{}, err
	}
	p, err := scanPlan(tx.QueryRow(ctx, "INSERT INTO plan_price_versions(id,code,version,name,price_cents,quota_bytes,bandwidth_mbps,period_seconds) VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING "+planColumns, newID(), input.Code, input.Version, input.Name, input.PriceCents, input.QuotaBytes, input.BandwidthMbps, input.PeriodSeconds))
	if err != nil {
		return p, err
	}
	if err = platformLog(ctx, tx, &who, "plan.create", p.ID, map[string]any{"code": p.Code, "version": p.Version}); err != nil {
		return PlanPriceVersion{}, err
	}
	return p, tx.Commit(ctx)
}

func queryPlans(ctx context.Context, tx pgx.Tx, page int) (Page[PlanPriceVersion], error) {
	result := Page[PlanPriceVersion]{Items: []PlanPriceVersion{}, Page: page, PageSize: 50}
	if page < 1 || page > 100000 {
		return result, ErrInvalid
	}
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM plan_price_versions").Scan(&result.Total); err != nil {
		return result, err
	}
	rows, err := tx.Query(ctx, "SELECT "+planColumns+" FROM plan_price_versions ORDER BY code,version DESC LIMIT 50 OFFSET $1", (page-1)*50)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		p, e := scanPlan(rows)
		if e != nil {
			return result, e
		}
		result.Items = append(result.Items, p)
	}
	return result, rows.Err()
}

func (s *Store) AdminPlans(ctx context.Context, who Identity, page int) (Page[PlanPriceVersion], error) {
	tx, err := s.adminReadTx(ctx, who)
	if err != nil {
		return Page[PlanPriceVersion]{}, err
	}
	defer tx.Rollback(ctx)
	return queryPlans(ctx, tx, page)
}

func (s *Store) ListPlans(ctx context.Context, who Identity, page int) (Page[PlanPriceVersion], error) {
	tx, err := s.billingReadTx(ctx, who)
	if err != nil {
		return Page[PlanPriceVersion]{}, err
	}
	defer tx.Rollback(ctx)
	return queryPlans(ctx, tx, page)
}

type PurchasePlanInput struct {
	RuleID         string `json:"ruleId"`
	PriceVersionID string `json:"priceVersionId"`
}

type RuleSubscription struct {
	ID                   string           `json:"id"`
	Ordinal              int64            `json:"ordinal"`
	RuleID               string           `json:"ruleId"`
	Plan                 PlanPriceVersion `json:"plan"`
	LedgerID             int64            `json:"ledgerId"`
	StartsAt             time.Time        `json:"startsAt"`
	EndsAt               time.Time        `json:"endsAt"`
	BaseUploadBytes      int64            `json:"baseUploadBytes"`
	BaseDownloadBytes    int64            `json:"baseDownloadBytes"`
	UsedBytes            int64            `json:"usedBytes"`
	RemainingBytes       int64            `json:"remainingBytes"`
	Closed               bool             `json:"closed"`
	Status               string           `json:"status"`
	EnforcementRequested bool             `json:"enforcementRequested"`
}

// Usage is the sum of deltas explicitly bound to this period. Closed historical
// periods continue receiving their own buffered reports after renewal; neither
// receipt time nor rule-wide baseline/final compatibility fields assign usage.
func readSubscription(ctx context.Context, tx pgx.Tx, id string) (RuleSubscription, error) {
	var p RuleSubscription
	var priceID, reason string
	var endUp, endDown int64
	var expired bool
	err := tx.QueryRow(ctx, `SELECT p.id,p.ordinal,p.rule_id,p.price_version_id,p.ledger_id,p.starts_at,p.ends_at,p.base_upload_bytes,p.base_download_bytes,COALESCE(u.upload_bytes,0),COALESCE(u.download_bytes,0),p.final_upload_bytes IS NOT NULL,p.ends_at<=clock_timestamp(),p.enforced_reason FROM rule_subscriptions p LEFT JOIN subscription_usage u ON u.subscription_id=p.id WHERE p.id=$1`, id).Scan(&p.ID, &p.Ordinal, &p.RuleID, &priceID, &p.LedgerID, &p.StartsAt, &p.EndsAt, &p.BaseUploadBytes, &p.BaseDownloadBytes, &endUp, &endDown, &p.Closed, &expired, &reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ErrNotFound
	}
	if err != nil {
		return p, err
	}
	p.Plan, err = scanPlan(tx.QueryRow(ctx, "SELECT "+planColumns+" FROM plan_price_versions WHERE id=$1", priceID))
	if err != nil {
		return p, err
	}
	if !billingBytePair(endUp, endDown) {
		return p, ErrConflict
	}
	p.UsedBytes = endUp + endDown
	p.RemainingBytes = p.Plan.QuotaBytes - p.UsedBytes
	if p.RemainingBytes < 0 {
		p.RemainingBytes = 0
	}
	p.Status = "active"
	if p.UsedBytes >= p.Plan.QuotaBytes {
		p.Status = "quota-exhausted"
	} else if expired {
		p.Status = "expired"
	}
	if p.Closed {
		p.Status = "closed"
	}
	p.EnforcementRequested = reason != ""
	return p, nil
}

// PurchasePlan atomically buys one period for one owned, nondeleted rule. The
// shared monetary wallet never shares byte quota or bandwidth between rules.
// Renewal is prohibited until expiry or exhaustion. Old global counter snapshots
// are compatibility metadata only: explicit per-period totals own accounting.
// A fresh period starts at purchase time; no early renewal, proration or rollover.
// Config is merely requested: a paid period is not evidence of Agent application.
func (s *Store) PurchasePlan(ctx context.Context, who Identity, key string, input PurchasePlanInput) (RuleSubscription, error) {
	if !idempotencyKey.MatchString(key) || !billingUUID.MatchString(input.RuleID) || !billingUUID.MatchString(input.PriceVersionID) {
		return RuleSubscription{}, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RuleSubscription{}, err
	}
	defer tx.Rollback(ctx)
	if err = tenantWriteAccess(ctx, tx, who); err != nil {
		return RuleSubscription{}, err
	}
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "reladock-subscription:"+who.TenantID+":"+key); err != nil {
		return RuleSubscription{}, err
	}
	var previousID, previousRule, previousPrice string
	err = tx.QueryRow(ctx, "SELECT id,rule_id,price_version_id FROM rule_subscriptions WHERE tenant_id=$1 AND idempotency_key=$2", who.TenantID, key).Scan(&previousID, &previousRule, &previousPrice)
	if err == nil {
		if previousRule != input.RuleID || previousPrice != input.PriceVersionID {
			return RuleSubscription{}, ErrConflict
		}
		return readSubscription(ctx, tx, previousID)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return RuleSubscription{}, err
	}
	var nodeID string
	err = tx.QueryRow(ctx, "SELECT node_id FROM relay_rules WHERE id=$1 AND tenant_id=$2 AND status<>'deleted'", input.RuleID, who.TenantID).Scan(&nodeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return RuleSubscription{}, ErrNotFound
	}
	if err != nil {
		return RuleSubscription{}, err
	}
	if err = lockNode(ctx, tx, nodeID); err != nil {
		return RuleSubscription{}, err
	}
	var lockedNode, status string
	// Serialize rule changes without blocking ledger foreign-key KEY SHARE locks.
	// Local corrections lock wallet before their ledger FK; FOR UPDATE here would
	// deadlock that path while this purchase waits for the same wallet.
	err = tx.QueryRow(ctx, "SELECT node_id,status FROM relay_rules WHERE id=$1 AND tenant_id=$2 FOR NO KEY UPDATE", input.RuleID, who.TenantID).Scan(&lockedNode, &status)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && status == "deleted" {
		return RuleSubscription{}, ErrNotFound
	}
	if err != nil {
		return RuleSubscription{}, err
	}
	if lockedNode != nodeID {
		return RuleSubscription{}, ErrConflict
	}
	plan, err := scanPlan(tx.QueryRow(ctx, "SELECT "+planColumns+" FROM plan_price_versions WHERE id=$1", input.PriceVersionID))
	if err != nil {
		return RuleSubscription{}, err
	}
	var capacity int
	if err = tx.QueryRow(ctx, "SELECT capacity_mbps FROM nodes WHERE id=$1", nodeID).Scan(&capacity); err != nil {
		return RuleSubscription{}, err
	}
	if plan.BandwidthMbps > capacity {
		return RuleSubscription{}, ErrInvalid
	}
	var up, down int64
	err = tx.QueryRow(ctx, "SELECT upload_bytes,download_bytes FROM billing_usage_totals WHERE rule_id=$1", input.RuleID).Scan(&up, &down)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return RuleSubscription{}, err
	}
	var oldID string
	err = tx.QueryRow(ctx, "SELECT id FROM rule_subscriptions WHERE rule_id=$1 ORDER BY ordinal DESC LIMIT 1 FOR UPDATE", input.RuleID).Scan(&oldID)
	if err == nil {
		old, e := readSubscription(ctx, tx, oldID)
		if e != nil {
			return RuleSubscription{}, e
		}
		if old.Status == "active" {
			return RuleSubscription{}, ErrConflict
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return RuleSubscription{}, err
	}
	var balance int64
	var frozen bool
	err = tx.QueryRow(ctx, "SELECT balance_cents,spending_frozen FROM billing_wallets WHERE tenant_id=$1 FOR UPDATE", who.TenantID).Scan(&balance, &frozen)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && balance < plan.PriceCents {
		return RuleSubscription{}, ErrConflict
	}
	if err != nil {
		return RuleSubscription{}, err
	}
	if frozen {
		return RuleSubscription{}, ErrForbidden
	}
	balance -= plan.PriceCents
	if _, err = tx.Exec(ctx, "UPDATE billing_wallets SET balance_cents=$2 WHERE tenant_id=$1", who.TenantID, balance); err != nil {
		return RuleSubscription{}, err
	}
	var ledgerID int64
	// A separate namespace prevents collisions with operator correction keys.
	ledgerKey := "subscription:" + newID()
	err = tx.QueryRow(ctx, `INSERT INTO billing_ledger(tenant_id,rule_id,kind,amount_cents,balance_after_cents,idempotency_key,reason,operator_name,price_version_id) VALUES($1,$2,'subscription-debit',$3,$4,$5,$6,$7,$8) RETURNING id`, who.TenantID, input.RuleID, -plan.PriceCents, balance, ledgerKey, fmt.Sprintf("Plan %s version %d", plan.Code, plan.Version), "member:"+who.MemberID, plan.ID).Scan(&ledgerID)
	if err != nil {
		return RuleSubscription{}, err
	}
	if oldID != "" {
		if _, err = tx.Exec(ctx, "UPDATE rule_subscriptions SET final_upload_bytes=$2,final_download_bytes=$3 WHERE id=$1", oldID, up, down); err != nil {
			return RuleSubscription{}, err
		}
	}
	id := newID()
	if _, err = tx.Exec(ctx, `INSERT INTO rule_subscriptions(id,tenant_id,rule_id,price_version_id,ledger_id,idempotency_key,starts_at,ends_at,base_upload_bytes,base_download_bytes) SELECT $1,$2,$3,$4,$5,$6,t,t+make_interval(secs=>$7::double precision),$8,$9 FROM (SELECT clock_timestamp() AS t) clock`, id, who.TenantID, input.RuleID, plan.ID, ledgerID, key, plan.PeriodSeconds, up, down); err != nil {
		return RuleSubscription{}, err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO subscription_usage(subscription_id,rule_id,tenant_id,node_id) VALUES($1,$2,$3,$4)", id, input.RuleID, who.TenantID, nodeID); err != nil {
		return RuleSubscription{}, err
	}
	if _, err = tx.Exec(ctx, "UPDATE relay_rules SET bandwidth_mbps=$2,status='pending-agent',desired_version=desired_version+1,updated_at=now() WHERE id=$1", input.RuleID, plan.BandwidthMbps); err != nil {
		return RuleSubscription{}, err
	}
	if err = s.enqueueConfig(ctx, tx, nodeID); err != nil {
		return RuleSubscription{}, err
	}
	if err = audit(ctx, tx, who, "subscription.purchase", id); err != nil {
		return RuleSubscription{}, err
	}
	result, err := readSubscription(ctx, tx, id)
	if err != nil {
		return RuleSubscription{}, err
	}
	return result, tx.Commit(ctx)
}

// RuleSubscriptions lists at most 100 periods, newest first. before is an
// exclusive ordinal cursor; zero starts at the latest. Tenant ownership is read
// from DB, including for historical/deleted rules.
func (s *Store) RuleSubscriptions(ctx context.Context, who Identity, ruleID string, before int64) ([]RuleSubscription, error) {
	result := []RuleSubscription{}
	if before < 0 {
		return result, ErrInvalid
	}
	tx, err := s.billingReadTx(ctx, who)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	if err = billingOwnRule(ctx, tx, who.TenantID, ruleID); err != nil {
		return result, err
	}
	rows, err := tx.Query(ctx, "SELECT id FROM rule_subscriptions WHERE tenant_id=$1 AND rule_id=$2 AND ($3::bigint=0 OR ordinal<$3) ORDER BY ordinal DESC LIMIT 100", who.TenantID, ruleID, before)
	if err != nil {
		return result, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return result, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	for _, id := range ids {
		p, e := readSubscription(ctx, tx, id)
		if e != nil {
			return result, e
		}
		result = append(result, p)
	}
	return result, nil
}

// EnforceRuleQuota must run INSIDE the usage transaction after totals are updated,
// with the existing node advisory lock and rule row lock held (in that order).
// It requests a paused config and never claims actual traffic has stopped.
// Collection/reporting/worker/Agent delays allow overshoot; this is not a hard
// packet-level quota. No subscription means no implicitly granted entitlement.
func (s *Store) EnforceRuleQuota(ctx context.Context, tx pgx.Tx, ruleID string) error {
	_, err := s.enforceRuleQuota(ctx, tx, ruleID)
	return err
}

// CheckRuleResumeAllowed is called before ANY resume shortcut/mutation, holding
// the node advisory lock and rule row lock. Unsubscribed rules retain existing
// behavior; no default entitlement is granted. Subscribed rules require their
// latest period to have time and quota remaining. This guard never rewrites the
// bandwidth from an old quote or purchase; the latest purchase owns that value.
func CheckRuleResumeAllowed(ctx context.Context, tx pgx.Tx, ruleID string) error {
	var id string
	err := tx.QueryRow(ctx, "SELECT id FROM rule_subscriptions WHERE rule_id=$1 ORDER BY ordinal DESC LIMIT 1", ruleID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	period, err := readSubscription(ctx, tx, id)
	if err != nil {
		return err
	}
	if period.Status != "active" {
		return ErrConflict
	}
	return nil
}

func (s *Store) enforceRuleQuota(ctx context.Context, tx pgx.Tx, ruleID string) (bool, error) {
	if s.allowUnentitledRules {
		return false, nil
	}
	var id string
	err := tx.QueryRow(ctx, "SELECT id FROM rule_subscriptions WHERE rule_id=$1 ORDER BY ordinal DESC LIMIT 1", ruleID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	period, err := readSubscription(ctx, tx, id)
	if err != nil {
		return false, err
	}
	if period.Status != "expired" && period.Status != "quota-exhausted" {
		return false, nil
	}
	var node, status string
	if err = tx.QueryRow(ctx, "SELECT node_id,status FROM relay_rules WHERE id=$1", ruleID).Scan(&node, &status); err != nil {
		return false, err
	}
	if status == "deleted" {
		return false, nil
	}
	if _, err = tx.Exec(ctx, "UPDATE rule_subscriptions SET enforced_reason=$2,enforced_at=clock_timestamp() WHERE id=$1", id, period.Status); err != nil {
		return false, err
	}
	if status == "paused" {
		return false, nil
	}
	if _, err = tx.Exec(ctx, "UPDATE relay_rules SET status='paused',desired_version=desired_version+1,updated_at=now() WHERE id=$1", ruleID); err != nil {
		return false, err
	}
	if err = s.enqueueConfig(ctx, tx, node); err != nil {
		return false, err
	}
	if err = platformLog(ctx, tx, nil, "subscription.enforce", ruleID, map[string]any{"subscriptionId": id, "reason": period.Status}); err != nil {
		return false, err
	}
	return true, nil
}

// EnforceExpiredSubscriptions is an explicitly invoked bounded worker (1..100).
// It also catches exhausted quotas, including rules manually resumed meanwhile.
// Each rule commits independently; count includes committed pause requests before
// a possible later error. No scheduler or network rule application is installed.
func (s *Store) EnforceExpiredSubscriptions(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 100 {
		return 0, ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT r.id,r.node_id FROM relay_rules r JOIN LATERAL (SELECT p.id,p.ends_at,v.quota_bytes FROM rule_subscriptions p JOIN plan_price_versions v ON v.id=p.price_version_id WHERE p.rule_id=r.id ORDER BY p.ordinal DESC LIMIT 1) p ON true LEFT JOIN subscription_usage u ON u.subscription_id=p.id WHERE r.status NOT IN ('paused','deleted') AND (p.ends_at<=clock_timestamp() OR COALESCE(u.upload_bytes,0)::numeric+COALESCE(u.download_bytes,0)::numeric>=p.quota_bytes) ORDER BY p.ends_at,r.id LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	type candidate struct{ rule, node string }
	candidates := []candidate{}
	for rows.Next() {
		var c candidate
		if err = rows.Scan(&c.rule, &c.node); err != nil {
			rows.Close()
			return 0, err
		}
		candidates = append(candidates, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	count := 0
	for _, c := range candidates {
		changed, e := s.enforceSubscriptionCandidate(ctx, c.rule, c.node)
		if e != nil {
			return count, e
		}
		if changed {
			count++
		}
	}
	return count, nil
}

func (s *Store) enforceSubscriptionCandidate(ctx context.Context, ruleID, nodeID string) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if err = lockNode(ctx, tx, nodeID); err != nil {
		return false, err
	}
	var currentNode string
	err = tx.QueryRow(ctx, "SELECT node_id FROM relay_rules WHERE id=$1 FOR UPDATE", ruleID).Scan(&currentNode)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if currentNode != nodeID {
		return false, ErrConflict
	}
	changed, err := s.enforceRuleQuota(ctx, tx, ruleID)
	if err != nil {
		return false, err
	}
	return changed, tx.Commit(ctx)
}
