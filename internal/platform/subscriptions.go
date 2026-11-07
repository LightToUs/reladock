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

var planCode = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
var offeringCode = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

// Every field is configured by an authorized platform administrator. Periods are
// elapsed seconds, not calendar months. Versions are immutable; create another
// version to change price, quota or bandwidth. No default plans are installed.
// Current schema permits positive prices/quotas, periods of 1..31,536,000 seconds
// and bandwidth of 1..1000 Mbps (also checked against the selected node capacity).
// New versions belong to a concrete line offering. Only published offerings on
// the selected rule's exact node enter the customer catalog; legacy unbound
// versions remain readable only as historical facts.
type PlanInput struct {
	OfferingID    string       `json:"offeringId"`
	Code          string       `json:"code"`
	Version       int64        `json:"version"`
	Name          string       `json:"name"`
	PriceCents    int64        `json:"priceCents"`
	QuotaBytes    int64        `json:"quotaBytes"`
	BandwidthMbps int          `json:"bandwidthMbps"`
	PeriodSeconds int64        `json:"periodSeconds"`
	ResourceMode  ResourceMode `json:"resourceMode"`
}

type PlanOfferingSummary struct {
	ID                string        `json:"id"`
	Code              string        `json:"code"`
	Name              string        `json:"name"`
	NodeID            string        `json:"nodeId"`
	SourceMarket      string        `json:"sourceMarket"`
	DestinationMarket string        `json:"destinationMarket"`
	State             OfferingState `json:"state"`
}

type PlanPriceVersion struct {
	ID string `json:"id"`
	PlanInput
	Offering  *PlanOfferingSummary `json:"offering,omitempty"`
	Currency  string               `json:"currency"`
	CreatedAt time.Time            `json:"createdAt"`
}

const planSelectColumns = "v.id,COALESCE(v.offering_id::text,''),v.code,v.version,v.name,v.price_cents,v.quota_bytes,v.bandwidth_mbps,v.period_seconds,v.resource_mode,v.currency,v.created_at,COALESCE(o.code,''),COALESCE(o.name,''),COALESCE(o.node_id,''),COALESCE(o.source_market,''),COALESCE(o.destination_market,''),COALESCE(o.state,'')"
const planSelectFrom = " FROM plan_price_versions v LEFT JOIN line_offerings o ON o.id=v.offering_id"

func scanPlan(row pgx.Row) (PlanPriceVersion, error) {
	var p PlanPriceVersion
	var offeringCode, offeringName, offeringNode, sourceMarket, destinationMarket string
	var offeringState OfferingState
	err := row.Scan(&p.ID, &p.OfferingID, &p.Code, &p.Version, &p.Name, &p.PriceCents, &p.QuotaBytes, &p.BandwidthMbps, &p.PeriodSeconds, &p.ResourceMode, &p.Currency, &p.CreatedAt, &offeringCode, &offeringName, &offeringNode, &sourceMarket, &destinationMarket, &offeringState)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ErrNotFound
	}
	if err == nil && p.OfferingID != "" {
		p.Offering = &PlanOfferingSummary{ID: p.OfferingID, Code: offeringCode, Name: offeringName, NodeID: offeringNode, SourceMarket: sourceMarket, DestinationMarket: destinationMarket, State: offeringState}
	}
	return p, err
}

func (s *Store) AdminCreatePlan(ctx context.Context, who Identity, input PlanInput) (PlanPriceVersion, error) {
	tx, err := s.adminTx(ctx, who)
	if err != nil {
		return PlanPriceVersion{}, err
	}
	defer tx.Rollback(ctx)
	input.Code = strings.ToLower(strings.TrimSpace(input.Code))
	input.Name = strings.TrimSpace(input.Name)
	if !billingUUID.MatchString(input.OfferingID) || !planCode.MatchString(input.Code) || input.Version < 0 || input.Version > 1000000 || !billingText(input.Name, 120) || input.PriceCents <= 0 || input.QuotaBytes <= 0 || input.BandwidthMbps < 1 || input.BandwidthMbps > 1000 || input.PeriodSeconds < 1 || input.PeriodSeconds > 31536000 {
		return PlanPriceVersion{}, ErrInvalid
	}
	var offeringMode ResourceMode
	var offeringState OfferingState
	err = tx.QueryRow(ctx, "SELECT n.resource_mode,o.state FROM line_offerings o JOIN nodes n ON n.id=o.node_id WHERE o.id=$1 FOR SHARE OF o,n", input.OfferingID).Scan(&offeringMode, &offeringState)
	if errors.Is(err, pgx.ErrNoRows) {
		return PlanPriceVersion{}, ErrNotFound
	}
	if err != nil {
		return PlanPriceVersion{}, err
	}
	if offeringState == OfferingStateRetired || input.ResourceMode != "" && input.ResourceMode != offeringMode {
		return PlanPriceVersion{}, ErrConflict
	}
	input.ResourceMode = offeringMode
	if input.Version == 0 {
		if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "plan-version:"+input.Code); err != nil {
			return PlanPriceVersion{}, err
		}
		latest, latestErr := scanPlan(tx.QueryRow(ctx, "SELECT "+planSelectColumns+planSelectFrom+" WHERE v.code=$1 ORDER BY v.version DESC LIMIT 1", input.Code))
		switch {
		case latestErr == nil:
			input.Version = latest.Version
			if latest.PlanInput == input {
				return latest, nil
			}
			if latest.Version >= 1000000 {
				return PlanPriceVersion{}, ErrConflict
			}
			input.Version = latest.Version + 1
		case errors.Is(latestErr, ErrNotFound):
			input.Version = 1
		default:
			return PlanPriceVersion{}, latestErr
		}
	}
	old, err := scanPlan(tx.QueryRow(ctx, "SELECT "+planSelectColumns+planSelectFrom+" WHERE v.code=$1 AND v.version=$2", input.Code, input.Version))
	if err == nil {
		if old.PlanInput != input {
			return PlanPriceVersion{}, ErrConflict
		}
		return old, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return PlanPriceVersion{}, err
	}
	id := newID()
	if _, err = tx.Exec(ctx, "INSERT INTO plan_price_versions(id,offering_id,code,version,name,price_cents,quota_bytes,bandwidth_mbps,period_seconds,resource_mode) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)", id, input.OfferingID, input.Code, input.Version, input.Name, input.PriceCents, input.QuotaBytes, input.BandwidthMbps, input.PeriodSeconds, input.ResourceMode); err != nil {
		return PlanPriceVersion{}, err
	}
	p, err := scanPlan(tx.QueryRow(ctx, "SELECT "+planSelectColumns+planSelectFrom+" WHERE v.id=$1", id))
	if err != nil {
		return p, err
	}
	if err = platformLog(ctx, tx, &who, "plan.create", p.ID, map[string]any{"code": p.Code, "version": p.Version, "offeringId": p.OfferingID}); err != nil {
		return PlanPriceVersion{}, err
	}
	return p, tx.Commit(ctx)
}

func queryAdminPlans(ctx context.Context, tx pgx.Tx, page int) (Page[PlanPriceVersion], error) {
	result := Page[PlanPriceVersion]{Items: []PlanPriceVersion{}, Page: page, PageSize: 50}
	if page < 1 || page > 100000 {
		return result, ErrInvalid
	}
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM plan_price_versions").Scan(&result.Total); err != nil {
		return result, err
	}
	rows, err := tx.Query(ctx, "SELECT "+planSelectColumns+planSelectFrom+" ORDER BY v.code,v.version DESC LIMIT 50 OFFSET $1", (page-1)*50)
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
	return queryAdminPlans(ctx, tx, page)
}

func (s *Store) ListPlans(ctx context.Context, who Identity, ruleID string, page int) (Page[PlanPriceVersion], error) {
	tx, err := s.billingReadTx(ctx, who)
	if err != nil {
		return Page[PlanPriceVersion]{}, err
	}
	defer tx.Rollback(ctx)
	if ruleID != "" && !billingUUID.MatchString(ruleID) || page < 1 || page > 100000 {
		return Page[PlanPriceVersion]{}, ErrInvalid
	}
	result := Page[PlanPriceVersion]{Items: []PlanPriceVersion{}, Page: page, PageSize: 50}
	if ruleID == "" {
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM plan_price_versions v JOIN line_offerings o ON o.id=v.offering_id JOIN nodes n ON n.id=o.node_id WHERE o.state='published' AND n.status IN ('unprovisioned','ready') AND (n.resource_mode='shared' OR n.dedicated_tenant_id=$1)`, who.TenantID).Scan(&result.Total); err != nil {
			return result, err
		}
		rows, err := tx.Query(ctx, "SELECT "+planSelectColumns+planSelectFrom+" JOIN nodes n ON n.id=o.node_id WHERE o.state='published' AND n.status IN ('unprovisioned','ready') AND (n.resource_mode='shared' OR n.dedicated_tenant_id=$1) ORDER BY v.code,v.version DESC LIMIT 50 OFFSET $2", who.TenantID, (page-1)*50)
		if err != nil {
			return result, err
		}
		defer rows.Close()
		for rows.Next() {
			plan, scanErr := scanPlan(rows)
			if scanErr != nil {
				return result, scanErr
			}
			result.Items = append(result.Items, plan)
		}
		return result, rows.Err()
	}
	var nodeID string
	var resourceMode ResourceMode
	var nodeStatus string
	err = tx.QueryRow(ctx, `SELECT n.id,n.resource_mode,n.status FROM relay_rules r JOIN nodes n ON n.id=r.node_id WHERE r.id=$1 AND r.tenant_id=$2 AND r.status<>'deleted'`, ruleID, who.TenantID).Scan(&nodeID, &resourceMode, &nodeStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return Page[PlanPriceVersion]{}, ErrNotFound
	}
	if err != nil {
		return Page[PlanPriceVersion]{}, err
	}
	if !nodeAllowsSales(nodeStatus) {
		return result, nil
	}
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM plan_price_versions v JOIN line_offerings o ON o.id=v.offering_id WHERE o.node_id=$1 AND o.state='published' AND v.resource_mode=$2`, nodeID, resourceMode).Scan(&result.Total); err != nil {
		return result, err
	}
	rows, err := tx.Query(ctx, "SELECT "+planSelectColumns+" FROM plan_price_versions v JOIN line_offerings o ON o.id=v.offering_id WHERE o.node_id=$1 AND o.state='published' AND v.resource_mode=$2 ORDER BY v.code,v.version DESC LIMIT 50 OFFSET $3", nodeID, resourceMode, (page-1)*50)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		plan, scanErr := scanPlan(rows)
		if scanErr != nil {
			return result, scanErr
		}
		result.Items = append(result.Items, plan)
	}
	return result, rows.Err()
}

type PurchasePlanInput struct {
	RuleID         string `json:"ruleId"`
	PriceVersionID string `json:"priceVersionId"`
}

type RuleSubscription struct {
	AutoRenew            bool             `json:"autoRenew"`
	AutoRenewError       string           `json:"autoRenewError,omitempty"`
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
	err := tx.QueryRow(ctx, `SELECT p.id,p.ordinal,COALESCE(p.rule_id::text,''),p.price_version_id,p.ledger_id,p.starts_at,p.ends_at,p.base_upload_bytes,p.base_download_bytes,COALESCE(u.upload_bytes,0),COALESCE(u.download_bytes,0),p.final_upload_bytes IS NOT NULL,p.ends_at<=clock_timestamp(),p.enforced_reason FROM rule_subscriptions p LEFT JOIN (SELECT subscription_id,SUM(upload_bytes)::bigint AS upload_bytes,SUM(download_bytes)::bigint AS download_bytes FROM subscription_usage GROUP BY subscription_id) u ON u.subscription_id=p.id WHERE p.id=$1`, id).Scan(&p.ID, &p.Ordinal, &p.RuleID, &priceID, &p.LedgerID, &p.StartsAt, &p.EndsAt, &p.BaseUploadBytes, &p.BaseDownloadBytes, &endUp, &endDown, &p.Closed, &expired, &reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ErrNotFound
	}
	if err != nil {
		return p, err
	}
	p.Plan, err = scanPlan(tx.QueryRow(ctx, "SELECT "+planSelectColumns+planSelectFrom+" WHERE v.id=$1", priceID))
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
	err = tx.QueryRow(ctx, `SELECT COALESCE((to_jsonb(p)->>'auto_renew')::boolean,false),COALESCE(to_jsonb(p)->>'auto_renew_error','') FROM rule_subscriptions p WHERE id=$1`, id).Scan(&p.AutoRenew, &p.AutoRenewError)
	if err != nil {
		return p, err
	}
	return p, nil
}

// PurchasePlan atomically buys one period for one owned, nondeleted rule. The
// shared monetary wallet never shares byte quota or bandwidth between rules.
// Renewal is prohibited until expiry or exhaustion. Old global counter snapshots
// are compatibility metadata only: explicit per-period totals own accounting.
// A fresh period starts at purchase time; no early renewal, proration or rollover.
// Config is merely requested: a paid period is not evidence of Agent application.
func (s *Store) PurchasePlan(ctx context.Context, who Identity, key string, input PurchasePlanInput) (RuleSubscription, error) {
	if !idempotencyKey.MatchString(key) || input.RuleID != "" && !billingUUID.MatchString(input.RuleID) || !billingUUID.MatchString(input.PriceVersionID) {
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
	err = tx.QueryRow(ctx, "SELECT id,COALESCE(rule_id::text,''),price_version_id FROM rule_subscriptions WHERE tenant_id=$1 AND idempotency_key=$2", who.TenantID, key).Scan(&previousID, &previousRule, &previousPrice)
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
	if input.RuleID != "" {
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
	}
	var lockedNode, status string
	// Serialize rule changes without blocking ledger foreign-key KEY SHARE locks.
	// Local corrections lock wallet before their ledger FK; FOR UPDATE here would
	// deadlock that path while this purchase waits for the same wallet.
	if input.RuleID != "" {
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
	}
	plan, err := scanPlan(tx.QueryRow(ctx, "SELECT "+planSelectColumns+planSelectFrom+" WHERE v.id=$1", input.PriceVersionID))
	if err != nil {
		return RuleSubscription{}, err
	}
	if plan.Offering == nil {
		return RuleSubscription{}, ErrConflict
	}
	var offeringNode string
	var offeringState OfferingState
	err = tx.QueryRow(ctx, "SELECT node_id,state FROM line_offerings WHERE id=$1 FOR SHARE", plan.OfferingID).Scan(&offeringNode, &offeringState)
	if errors.Is(err, pgx.ErrNoRows) {
		return RuleSubscription{}, ErrConflict
	}
	if err != nil {
		return RuleSubscription{}, err
	}
	if offeringState != OfferingStatePublished || input.RuleID != "" && offeringNode != nodeID {
		return RuleSubscription{}, ErrConflict
	}
	if input.RuleID == "" {
		nodeID = offeringNode
		if err = lockNode(ctx, tx, nodeID); err != nil {
			return RuleSubscription{}, err
		}
	}
	var capacity int
	var resourceMode ResourceMode
	var nodeStatus string
	if err = tx.QueryRow(ctx, "SELECT capacity_mbps,resource_mode,status FROM nodes WHERE id=$1", nodeID).Scan(&capacity, &resourceMode, &nodeStatus); err != nil {
		return RuleSubscription{}, err
	}
	if plan.ResourceMode != resourceMode {
		return RuleSubscription{}, ErrConflict
	}
	if !nodeAllowsSales(nodeStatus) {
		return RuleSubscription{}, ErrNodeNotAllocatable
	}
	if plan.BandwidthMbps > capacity {
		return RuleSubscription{}, ErrInvalid
	}
	var up, down int64
	if input.RuleID != "" {
		err = tx.QueryRow(ctx, "SELECT upload_bytes,download_bytes FROM billing_usage_totals WHERE rule_id=$1", input.RuleID).Scan(&up, &down)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return RuleSubscription{}, err
		}
	}
	var oldID string
	if input.RuleID != "" {
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
	}
	var balance int64
	var frozen bool
	err = tx.QueryRow(ctx, "SELECT balance_cents,spending_frozen FROM billing_wallets WHERE tenant_id=$1 FOR UPDATE", who.TenantID).Scan(&balance, &frozen)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && balance < plan.PriceCents {
		return RuleSubscription{}, ErrInsufficientBalance
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
	err = tx.QueryRow(ctx, `INSERT INTO billing_ledger(tenant_id,rule_id,kind,amount_cents,balance_after_cents,idempotency_key,reason,operator_name,price_version_id) VALUES($1,NULLIF($2,'')::uuid,'subscription-debit',$3,$4,$5,$6,$7,$8) RETURNING id`, who.TenantID, input.RuleID, -plan.PriceCents, balance, ledgerKey, fmt.Sprintf("Plan %s version %d", plan.Code, plan.Version), "member:"+who.MemberID, plan.ID).Scan(&ledgerID)
	if err != nil {
		return RuleSubscription{}, err
	}
	if oldID != "" {
		if _, err = tx.Exec(ctx, "UPDATE rule_subscriptions SET final_upload_bytes=$2,final_download_bytes=$3 WHERE id=$1", oldID, up, down); err != nil {
			return RuleSubscription{}, err
		}
	}
	id := newID()
	if _, err = tx.Exec(ctx, `INSERT INTO rule_subscriptions(id,tenant_id,rule_id,price_version_id,ledger_id,idempotency_key,starts_at,ends_at,base_upload_bytes,base_download_bytes) SELECT $1,$2,NULLIF($3,'')::uuid,$4,$5,$6,t,t+make_interval(secs=>$7::double precision),$8,$9 FROM (SELECT clock_timestamp() AS t) clock`, id, who.TenantID, input.RuleID, plan.ID, ledgerID, key, plan.PeriodSeconds, up, down); err != nil {
		return RuleSubscription{}, err
	}
	if input.RuleID != "" {
		if _, err = tx.Exec(ctx, "INSERT INTO subscription_usage(subscription_id,rule_id,tenant_id,node_id) VALUES($1,$2,$3,$4)", id, input.RuleID, who.TenantID, nodeID); err != nil {
			return RuleSubscription{}, err
		}
	}
	if input.RuleID != "" {
		if _, err = tx.Exec(ctx, "UPDATE relay_rules SET bandwidth_mbps=$2,status='pending-agent',desired_version=desired_version+1,updated_at=now() WHERE id=$1", input.RuleID, plan.BandwidthMbps); err != nil {
			return RuleSubscription{}, err
		}
		if err = s.enqueueConfig(ctx, tx, nodeID); err != nil {
			return RuleSubscription{}, err
		}
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
	rows, err := tx.Query(ctx, "SELECT p.id FROM rule_subscriptions p WHERE p.tenant_id=$1 AND (p.rule_id=$2 OR EXISTS(SELECT 1 FROM subscription_usage u WHERE u.subscription_id=p.id AND u.rule_id=$2 AND u.tenant_id=$1)) AND ($3::bigint=0 OR p.ordinal<$3) ORDER BY p.ordinal DESC LIMIT 100", who.TenantID, ruleID, before)
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
		if p.RuleID == "" {
			p.RuleID = ruleID
		}
		result = append(result, p)
	}
	return result, nil
}

// UnboundSubscriptions lists paid periods ready to be consumed by a new rule.
func (s *Store) UnboundSubscriptions(ctx context.Context, who Identity) ([]RuleSubscription, error) {
	result := []RuleSubscription{}
	tx, err := s.billingReadTx(ctx, who)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, "SELECT p.id FROM rule_subscriptions p WHERE p.tenant_id=$1 AND p.final_upload_bytes IS NULL AND p.ends_at>clock_timestamp() ORDER BY p.ordinal DESC LIMIT 100", who.TenantID)
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
	if err = rows.Err(); err != nil {
		rows.Close()
		return result, err
	}
	rows.Close()
	for _, id := range ids {
		period, readErr := readSubscription(ctx, tx, id)
		if readErr != nil {
			return result, readErr
		}
		if period.Status == "active" {
			result = append(result, period)
		}
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
	err := tx.QueryRow(ctx, "SELECT p.id FROM rule_subscriptions p WHERE p.rule_id=$1 OR EXISTS(SELECT 1 FROM subscription_usage u WHERE u.subscription_id=p.id AND u.rule_id=$1) ORDER BY p.ordinal DESC LIMIT 1", ruleID).Scan(&id)
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
	err := tx.QueryRow(ctx, "SELECT p.id FROM rule_subscriptions p WHERE p.rule_id=$1 OR EXISTS(SELECT 1 FROM subscription_usage u WHERE u.subscription_id=p.id AND u.rule_id=$1) ORDER BY p.ordinal DESC LIMIT 1", ruleID).Scan(&id)
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
	if renewed, e := s.autoRenewExhausted(ctx, tx, period, node); e != nil || renewed {
		return renewed, e
	}
	if _, err = tx.Exec(ctx, "UPDATE rule_subscriptions SET enforced_reason=$2,enforced_at=clock_timestamp() WHERE id=$1", id, period.Status); err != nil {
		return false, err
	}
	rows, err := tx.Query(ctx, "SELECT DISTINCT r.id,r.node_id,r.status FROM relay_rules r LEFT JOIN subscription_usage u ON u.rule_id=r.id AND u.subscription_id=$1 WHERE (u.subscription_id IS NOT NULL OR r.id=(SELECT rule_id FROM rule_subscriptions WHERE id=$1)) AND r.status<>'deleted'", id)
	if err != nil {
		return false, err
	}
	type boundRule struct{ id, node, status string }
	bound := []boundRule{}
	for rows.Next() {
		var item boundRule
		if err = rows.Scan(&item.id, &item.node, &item.status); err != nil {
			rows.Close()
			return false, err
		}
		bound = append(bound, item)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	rows.Close()
	changed := false
	for _, item := range bound {
		if item.status == "paused" {
			continue
		}
		if _, err = tx.Exec(ctx, "UPDATE relay_rules SET status='paused',desired_version=desired_version+1,updated_at=now() WHERE id=$1", item.id); err != nil {
			return false, err
		}
		changed = true
	}
	if !changed {
		return false, nil
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
	rows, err := s.pool.Query(ctx, `SELECT r.id,r.node_id FROM relay_rules r JOIN LATERAL (SELECT p.id,p.ends_at,v.quota_bytes FROM rule_subscriptions p JOIN plan_price_versions v ON v.id=p.price_version_id WHERE p.rule_id=r.id OR EXISTS(SELECT 1 FROM subscription_usage su WHERE su.subscription_id=p.id AND su.rule_id=r.id) ORDER BY p.ordinal DESC LIMIT 1) p ON true LEFT JOIN LATERAL (SELECT SUM(upload_bytes)::numeric AS upload_bytes,SUM(download_bytes)::numeric AS download_bytes FROM subscription_usage su WHERE su.subscription_id=p.id) u ON true WHERE r.status NOT IN ('paused','deleted') AND (p.ends_at<=clock_timestamp() OR COALESCE(u.upload_bytes,0)+COALESCE(u.download_bytes,0)>=p.quota_bytes) ORDER BY p.ends_at,r.id LIMIT $1`, limit)
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
