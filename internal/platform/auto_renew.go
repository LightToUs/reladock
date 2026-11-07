package platform

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
)

// TenantSubscriptions returns one entry per package, not one per bound rule.
func (s *Store) TenantSubscriptions(ctx context.Context, who Identity, before int64) ([]RuleSubscription, error) {
	result := []RuleSubscription{}
	if before < 0 {
		return result, ErrInvalid
	}
	tx, err := s.billingReadTx(ctx, who)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT id FROM rule_subscriptions WHERE tenant_id=$1 AND ($2::bigint=0 OR ordinal<$2) ORDER BY ordinal DESC LIMIT 100`, who.TenantID, before)
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

// SetSubscriptionAutoRenew records explicit tenant consent. Old closed periods
// cannot change the successor's consent. Disabling never initiates a purchase.
func (s *Store) SetSubscriptionAutoRenew(ctx context.Context, who Identity, id string, enabled bool) (RuleSubscription, error) {
	if !billingUUID.MatchString(id) {
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
	result, err := tx.Exec(ctx, `UPDATE rule_subscriptions SET auto_renew=$3 WHERE id=$1 AND tenant_id=$2 AND final_upload_bytes IS NULL AND ends_at>clock_timestamp()`, id, who.TenantID, enabled)
	if err != nil {
		return RuleSubscription{}, err
	}
	if result.RowsAffected() != 1 {
		return RuleSubscription{}, ErrNotFound
	}
	if err = audit(ctx, tx, who, "subscription.auto-renew", id); err != nil {
		return RuleSubscription{}, err
	}
	p, err := readSubscription(ctx, tx, id)
	if err != nil {
		return p, err
	}
	return p, tx.Commit(ctx)
}

// Called under the node lock, in the same transaction as usage and enforcement.
// Business refusals commit the report and normal pause, instead of losing usage.
func (s *Store) autoRenewExhausted(ctx context.Context, tx pgx.Tx, p RuleSubscription, node string) (bool, error) {
	var tenant string
	var enabled, eligible bool
	err := tx.QueryRow(ctx, `SELECT tenant_id,auto_renew,ends_at>clock_timestamp() AND final_upload_bytes IS NULL FROM rule_subscriptions WHERE id=$1 FOR UPDATE`, p.ID).Scan(&tenant, &enabled, &eligible)
	if err != nil || !enabled || !eligible || p.Status != "quota-exhausted" {
		return false, err
	}
	refuse := func(reason string) (bool, error) {
		_, e := tx.Exec(ctx, "UPDATE rule_subscriptions SET auto_renew_error=$2 WHERE id=$1", p.ID, reason)
		return false, e
	}
	var active bool
	if err = tx.QueryRow(ctx, "SELECT status='active' FROM tenants WHERE id=$1 FOR SHARE", tenant).Scan(&active); err != nil {
		return false, err
	}
	if !active {
		return refuse("tenant-inactive")
	}
	if p.Plan.Offering == nil {
		return refuse("unavailable")
	}
	var sale, status string
	var capacity int
	var mode ResourceMode
	if err = tx.QueryRow(ctx, `SELECT o.state,n.status,n.capacity_mbps,n.resource_mode FROM line_offerings o JOIN nodes n ON n.id=o.node_id WHERE o.id=$1 AND n.id=$2 FOR SHARE OF o`, p.Plan.OfferingID, node).Scan(&sale, &status, &capacity, &mode); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return refuse("unavailable")
		}
		return false, err
	}
	if sale != "published" || !nodeAllowsSales(status) || mode != p.Plan.ResourceMode || capacity < p.Plan.BandwidthMbps {
		return refuse("unavailable")
	}
	// Only move bindings for which this is still the latest period. Deleted or
	// independently renewed rules must not be revived by an old report.
	rows, err := tx.Query(ctx, `SELECT r.id FROM subscription_usage u JOIN relay_rules r ON r.id=u.rule_id AND r.tenant_id=u.tenant_id WHERE u.subscription_id=$1 AND r.node_id=$2 AND r.status<>'deleted' AND NOT EXISTS(SELECT 1 FROM subscription_usage newer JOIN rule_subscriptions np ON np.id=newer.subscription_id WHERE newer.rule_id=r.id AND np.ordinal>$3) ORDER BY r.id`, p.ID, node, p.Ordinal)
	if err != nil {
		return false, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return false, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	if len(ids) == 0 {
		return false, nil
	}
	var balance int64
	var frozen bool
	err = tx.QueryRow(ctx, "SELECT balance_cents,spending_frozen FROM billing_wallets WHERE tenant_id=$1 FOR UPDATE", tenant).Scan(&balance, &frozen)
	if errors.Is(err, pgx.ErrNoRows) {
		return refuse("insufficient-balance")
	}
	if err != nil {
		return false, err
	}
	if frozen {
		return refuse("wallet-frozen")
	}
	if balance < p.Plan.PriceCents {
		return refuse("insufficient-balance")
	}
	balance -= p.Plan.PriceCents
	if _, err = tx.Exec(ctx, "UPDATE billing_wallets SET balance_cents=$2 WHERE tenant_id=$1", tenant, balance); err != nil {
		return false, err
	}
	var ledger int64
	key := "auto-renew:" + p.ID
	err = tx.QueryRow(ctx, `INSERT INTO billing_ledger(tenant_id,kind,amount_cents,balance_after_cents,idempotency_key,reason,operator_name,price_version_id) VALUES($1,'subscription-debit',$2,$3,$4,'Traffic quota auto renewal','auto-renew',$5) RETURNING id`, tenant, -p.Plan.PriceCents, balance, key, p.Plan.ID).Scan(&ledger)
	if err != nil {
		return false, err
	}
	next := newID()
	_, err = tx.Exec(ctx, `INSERT INTO rule_subscriptions(id,tenant_id,price_version_id,ledger_id,idempotency_key,starts_at,ends_at,base_upload_bytes,base_download_bytes,auto_renew,renewed_from) SELECT $1,$2,$3,$4,$5,t,t+make_interval(secs=>$6::double precision),0,0,true,$7 FROM (SELECT clock_timestamp() t) clock`, next, tenant, p.Plan.ID, ledger, key, p.Plan.PeriodSeconds, p.ID)
	if err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO subscription_usage(subscription_id,rule_id,tenant_id,node_id) SELECT $1,r.id,r.tenant_id,r.node_id FROM relay_rules r WHERE r.id=ANY($2::uuid[])`, next, ids); err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE rule_subscriptions SET final_upload_bytes=base_upload_bytes,final_download_bytes=base_download_bytes,auto_renew_error='' WHERE id=$1`, p.ID); err != nil {
		return false, err
	}
	// Manual pauses remain paused. A successful immediate renewal requests a new
	// snapshot/epoch; this does not claim uninterrupted forwarding or Agent ACK.
	if _, err = tx.Exec(ctx, `UPDATE relay_rules SET desired_version=desired_version+1,updated_at=now() WHERE id=ANY($1::uuid[])`, ids); err != nil {
		return false, err
	}
	if err = s.enqueueConfig(ctx, tx, node); err != nil {
		return false, err
	}
	if err = platformLog(ctx, tx, nil, "subscription.auto-renew.purchase", next, map[string]any{"previousId": p.ID, "ledgerId": ledger, "amountCents": p.Plan.PriceCents}); err != nil {
		return false, err
	}
	return true, nil
}
