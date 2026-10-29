package platform

import (
	"context"
	"errors"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

var billingUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type Wallet struct {
	Currency       string `json:"currency"`
	BalanceCents   int64  `json:"balanceCents"`
	Initialized    bool   `json:"initialized"`
	SpendingFrozen bool   `json:"spendingFrozen"`
}

type LedgerEntry struct {
	ID                int64     `json:"id"`
	RuleID            string    `json:"ruleId,omitempty"`
	Currency          string    `json:"currency"`
	Kind              string    `json:"kind"`
	AmountCents       int64     `json:"amountCents"`
	BalanceAfterCents int64     `json:"balanceAfterCents"`
	Key               string    `json:"idempotencyKey"`
	Reason            string    `json:"reason"`
	Operator          string    `json:"operator"`
	CreatedAt         time.Time `json:"createdAt"`
}

// WalletAdjustment is a trusted local operator correction, never a payment or
// public top-up input. A rule reference is optional for a wallet-level correction;
// any future usage charge must require its own rule reference and price version.
type WalletAdjustment struct {
	TenantID    string
	RuleID      string
	Key         string
	AmountCents int64
	Reason      string
	Operator    string
}

const ledgerColumns = "id,COALESCE(rule_id::text,''),currency,kind,amount_cents,balance_after_cents,idempotency_key,reason,operator_name,created_at"

func scanLedger(row pgx.Row) (LedgerEntry, error) {
	var e LedgerEntry
	err := row.Scan(&e.ID, &e.RuleID, &e.Currency, &e.Kind, &e.AmountCents, &e.BalanceAfterCents, &e.Key, &e.Reason, &e.Operator, &e.CreatedAt)
	return e, err
}

// AdjustWalletFromCLI is reserved for a local CLI with database access. The caller
// must obtain explicit operator authorization before invoking this method.
// Never wire this method to HTTP, even for platform administrators.
// Negative corrections cannot overdraw. Reversals append a new correction/key.
func (s *Store) AdjustWalletFromCLI(ctx context.Context, input WalletAdjustment) (LedgerEntry, error) {
	if !billingUUID.MatchString(input.TenantID) || input.RuleID != "" && !billingUUID.MatchString(input.RuleID) || !idempotencyKey.MatchString(input.Key) || input.AmountCents == 0 || input.AmountCents == math.MinInt64 || !billingText(input.Reason, 500) || !billingText(input.Operator, 120) {
		return LedgerEntry{}, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return LedgerEntry{}, err
	}
	defer tx.Rollback(ctx)
	var tenant string
	err = tx.QueryRow(ctx, "SELECT id::text FROM tenants WHERE id=$1 FOR KEY SHARE", input.TenantID).Scan(&tenant)
	if errors.Is(err, pgx.ErrNoRows) {
		return LedgerEntry{}, ErrNotFound
	}
	if err != nil {
		return LedgerEntry{}, err
	}
	if input.RuleID != "" {
		if err = billingOwnRule(ctx, tx, input.TenantID, input.RuleID); err != nil {
			return LedgerEntry{}, err
		}
	}
	if _, err = tx.Exec(ctx, "INSERT INTO billing_wallets(tenant_id) VALUES($1) ON CONFLICT DO NOTHING", input.TenantID); err != nil {
		return LedgerEntry{}, err
	}
	var balance int64
	if err = tx.QueryRow(ctx, "SELECT balance_cents FROM billing_wallets WHERE tenant_id=$1 FOR UPDATE", input.TenantID).Scan(&balance); err != nil {
		return LedgerEntry{}, err
	}
	old, err := scanLedger(tx.QueryRow(ctx, "SELECT "+ledgerColumns+" FROM billing_ledger WHERE tenant_id=$1 AND idempotency_key=$2", input.TenantID, input.Key))
	if err == nil {
		if old.RuleID != input.RuleID || old.AmountCents != input.AmountCents || old.Reason != input.Reason || old.Operator != input.Operator {
			return LedgerEntry{}, ErrConflict
		}
		return old, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return LedgerEntry{}, err
	}
	if input.AmountCents > 0 && balance > math.MaxInt64-input.AmountCents {
		return LedgerEntry{}, ErrInvalid
	}
	if input.AmountCents < 0 && balance < -input.AmountCents {
		return LedgerEntry{}, ErrConflict
	}
	balance += input.AmountCents
	if _, err = tx.Exec(ctx, "UPDATE billing_wallets SET balance_cents=$2 WHERE tenant_id=$1", input.TenantID, balance); err != nil {
		return LedgerEntry{}, err
	}
	entry, err := scanLedger(tx.QueryRow(ctx, "INSERT INTO billing_ledger(tenant_id,rule_id,kind,amount_cents,balance_after_cents,idempotency_key,reason,operator_name) VALUES($1,NULLIF($2,'')::uuid,'local-adjustment',$3,$4,$5,$6,$7) RETURNING "+ledgerColumns, input.TenantID, input.RuleID, input.AmountCents, balance, input.Key, input.Reason, input.Operator))
	if err != nil {
		return LedgerEntry{}, err
	}
	if err = platformLog(ctx, tx, nil, "wallet.adjust", input.TenantID, map[string]any{"ledgerId": entry.ID, "ruleId": input.RuleID, "operator": input.Operator, "amountCents": input.AmountCents}); err != nil {
		return LedgerEntry{}, err
	}
	return entry, tx.Commit(ctx)
}

func billingText(value string, max int) bool {
	return utf8.ValidString(value) && strings.TrimSpace(value) != "" && utf8.RuneCountInString(value) <= max && !strings.ContainsRune(value, 0)
}

// UsageReport deliberately contains no tenant or node field. nodeID must come
// from the verified, currently authorized mTLS node identity at the call site.
// Counters cover upload+download for ONE rule at ONE designated billing boundary.
// They count from zero on each never-reused epoch; gaps in sequence are allowed
// because counters are cumulative. New reports on subscribed rules require an
// explicit period ID; previously accepted legacy reports remain replayable.
// The future collector must obtain the ID from authoritative configuration and
// start a fresh epoch/counter at every period change; never relabel buffered WAL.
type UsageReport struct {
	RuleID         string `json:"ruleId"`
	SubscriptionID string `json:"subscriptionId,omitempty"`
	Epoch          string `json:"epoch"`
	Sequence       int64  `json:"sequence"`
	UploadBytes    int64  `json:"uploadBytes"`
	DownloadBytes  int64  `json:"downloadBytes"`
}

type UsageReceipt struct {
	RuleID             string `json:"ruleId"`
	SubscriptionID     string `json:"subscriptionId,omitempty"`
	Epoch              string `json:"epoch"`
	Sequence           int64  `json:"sequence"`
	UploadDeltaBytes   int64  `json:"uploadDeltaBytes"`
	DownloadDeltaBytes int64  `json:"downloadDeltaBytes"`
	Duplicate          bool   `json:"duplicate"`
}

type RuleUsage struct {
	RuleID         string     `json:"ruleId"`
	UploadBytes    int64      `json:"uploadBytes"`
	DownloadBytes  int64      `json:"downloadBytes"`
	TotalBytes     int64      `json:"totalBytes"`
	HasReports     bool       `json:"hasReports"`
	LastReportedAt *time.Time `json:"lastReportedAt"`
}

// ReportUsage atomically records a cumulative report and its deltas. It does not
// infer prices, quota, payment, health, deployment or charge a wallet. Soft-deleted
// rules still accept buffered reports from their owning node. First report in a
// new epoch adds its full counters. Explicitly bound epochs accept monotonic late
// reports for their original period, including closed periods; legacy unbound
// retired epochs accept exact replays only. Receipt times never assign a period.
func (s *Store) ReportUsage(ctx context.Context, nodeID string, input UsageReport, fingerprint ...string) (UsageReceipt, error) {
	receipt := UsageReceipt{RuleID: input.RuleID, SubscriptionID: input.SubscriptionID, Epoch: input.Epoch, Sequence: input.Sequence}
	if !nodeIDPattern.MatchString(nodeID) || !billingUUID.MatchString(input.RuleID) || input.SubscriptionID != "" && !billingUUID.MatchString(input.SubscriptionID) || !idempotencyKey.MatchString(input.Epoch) || input.Sequence <= 0 || !billingBytePair(input.UploadBytes, input.DownloadBytes) {
		return receipt, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return receipt, err
	}
	defer tx.Rollback(ctx)
	// Same lock and credential check as configuration delivery/rotation. HTTP
	// callers must pass the verified certificate fingerprint; omission is reserved
	// for trusted in-process callers, never for an unauthenticated network request.
	if err = lockNode(ctx, tx, nodeID); err != nil {
		return receipt, err
	}
	if err = checkNodeCredential(ctx, tx, nodeID, fingerprint); err != nil {
		return receipt, err
	}
	var tenantID, ownerNode string
	// Rule row is the stream mutex, shared by every epoch and sequence, including
	// the very first concurrent reports. Ownership is read under that same lock.
	err = tx.QueryRow(ctx, "SELECT tenant_id::text,node_id FROM relay_rules WHERE id=$1 FOR UPDATE", input.RuleID).Scan(&tenantID, &ownerNode)
	if errors.Is(err, pgx.ErrNoRows) {
		return receipt, ErrNotFound
	}
	if err != nil {
		return receipt, err
	}
	if ownerNode != nodeID {
		return receipt, ErrForbidden
	}
	var oldUp, oldDown int64
	var boundPeriod string
	err = tx.QueryRow(ctx, `SELECT upload_bytes,download_bytes,upload_delta_bytes,download_delta_bytes,COALESCE(subscription_id::text,'') FROM billing_usage_reports WHERE rule_id=$1 AND epoch=$2 AND sequence=$3`, input.RuleID, input.Epoch, input.Sequence).Scan(&oldUp, &oldDown, &receipt.UploadDeltaBytes, &receipt.DownloadDeltaBytes, &boundPeriod)
	if err == nil {
		if oldUp != input.UploadBytes || oldDown != input.DownloadBytes || boundPeriod != input.SubscriptionID {
			return receipt, ErrConflict
		}
		receipt.Duplicate = true
		return receipt, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return receipt, err
	}
	// A pre-purchase legacy report already durably accepted must remain ACKable,
	// even after the rule becomes subscribed, so an ordered WAL can drain. Only
	// NEW reports require a period; exact replay above also compares the binding.
	var periodUp, periodDown int64
	if input.SubscriptionID == "" {
		var subscribed bool
		if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM rule_subscriptions WHERE rule_id=$1)", input.RuleID).Scan(&subscribed); err != nil {
			return receipt, err
		}
		if subscribed {
			return receipt, ErrInvalid
		}
	} else {
		err = tx.QueryRow(ctx, `SELECT u.upload_bytes,u.download_bytes FROM subscription_usage u JOIN rule_subscriptions p ON p.id=u.subscription_id WHERE u.subscription_id=$1 AND p.rule_id=$2 AND p.tenant_id=$3 AND u.node_id=$4`, input.SubscriptionID, input.RuleID, tenantID, nodeID).Scan(&periodUp, &periodDown)
		if errors.Is(err, pgx.ErrNoRows) {
			return receipt, ErrNotFound
		}
		if err != nil {
			return receipt, err
		}
	}
	var active string
	var totalUp, totalDown int64
	err = tx.QueryRow(ctx, "SELECT active_epoch,upload_bytes,download_bytes FROM billing_usage_totals WHERE rule_id=$1", input.RuleID).Scan(&active, &totalUp, &totalDown)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return receipt, err
	}
	var lastSequence int64
	err = tx.QueryRow(ctx, "SELECT last_sequence,upload_bytes,download_bytes,COALESCE(subscription_id::text,'') FROM billing_usage_epochs WHERE rule_id=$1 AND epoch=$2", input.RuleID, input.Epoch).Scan(&lastSequence, &oldUp, &oldDown, &boundPeriod)
	known := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return receipt, err
	}
	if known && (boundPeriod != input.SubscriptionID || input.SubscriptionID == "" && active != input.Epoch || input.Sequence <= lastSequence || input.UploadBytes < oldUp || input.DownloadBytes < oldDown) {
		return receipt, ErrConflict
	}
	if !known {
		oldUp = 0
		oldDown = 0
	}
	receipt.UploadDeltaBytes = input.UploadBytes - oldUp
	receipt.DownloadDeltaBytes = input.DownloadBytes - oldDown
	if totalUp > math.MaxInt64-receipt.UploadDeltaBytes || totalDown > math.MaxInt64-receipt.DownloadDeltaBytes {
		return receipt, ErrInvalid
	}
	totalUp += receipt.UploadDeltaBytes
	totalDown += receipt.DownloadDeltaBytes
	if !billingBytePair(totalUp, totalDown) {
		return receipt, ErrInvalid
	}
	if input.SubscriptionID != "" {
		if periodUp > math.MaxInt64-receipt.UploadDeltaBytes || periodDown > math.MaxInt64-receipt.DownloadDeltaBytes {
			return receipt, ErrInvalid
		}
		periodUp += receipt.UploadDeltaBytes
		periodDown += receipt.DownloadDeltaBytes
		if !billingBytePair(periodUp, periodDown) {
			return receipt, ErrInvalid
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO billing_usage_epochs(rule_id,tenant_id,node_id,epoch,last_sequence,upload_bytes,download_bytes,subscription_id) VALUES($1,$2,$3,$4,$5,$6,$7,NULLIF($8,'')::uuid) ON CONFLICT(rule_id,epoch) DO UPDATE SET last_sequence=EXCLUDED.last_sequence,upload_bytes=EXCLUDED.upload_bytes,download_bytes=EXCLUDED.download_bytes`, input.RuleID, tenantID, nodeID, input.Epoch, input.Sequence, input.UploadBytes, input.DownloadBytes, input.SubscriptionID); err != nil {
		return receipt, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO billing_usage_reports(rule_id,tenant_id,node_id,epoch,sequence,upload_bytes,download_bytes,upload_delta_bytes,download_delta_bytes,subscription_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,NULLIF($10,'')::uuid)`, input.RuleID, tenantID, nodeID, input.Epoch, input.Sequence, input.UploadBytes, input.DownloadBytes, receipt.UploadDeltaBytes, receipt.DownloadDeltaBytes, input.SubscriptionID); err != nil {
		return receipt, err
	}
	if !known || active == "" {
		active = input.Epoch
	}
	if _, err = tx.Exec(ctx, `INSERT INTO billing_usage_totals(rule_id,tenant_id,node_id,active_epoch,upload_bytes,download_bytes) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(rule_id) DO UPDATE SET active_epoch=EXCLUDED.active_epoch,upload_bytes=EXCLUDED.upload_bytes,download_bytes=EXCLUDED.download_bytes,updated_at=now()`, input.RuleID, tenantID, nodeID, active, totalUp, totalDown); err != nil {
		return receipt, err
	}
	if input.SubscriptionID != "" {
		if _, err = tx.Exec(ctx, "UPDATE subscription_usage SET upload_bytes=$2,download_bytes=$3,updated_at=clock_timestamp() WHERE subscription_id=$1", input.SubscriptionID, periodUp, periodDown); err != nil {
			return receipt, err
		}
	}
	if err = EnforceRuleQuota(ctx, tx, input.RuleID); err != nil {
		return receipt, err
	}
	return receipt, tx.Commit(ctx)
}

func billingBytePair(up, down int64) bool { return up >= 0 && down >= 0 && up <= math.MaxInt64-down }

// Revalidate membership against DB; no role or tenant supplied by the browser
// can widen scope. Viewers may read. Locks keep suspension/reassignment from
// crossing an already-authorized query. The outer HTTP layer verifies sessions.
func (s *Store) billingReadTx(ctx context.Context, who Identity) (pgx.Tx, error) {
	if !billingUUID.MatchString(who.MemberID) || !billingUUID.MatchString(who.TenantID) {
		return nil, ErrUnauthorized
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	var id string
	err = tx.QueryRow(ctx, `SELECT m.id::text FROM members m JOIN tenants t ON t.id=m.tenant_id WHERE m.id=$1 AND m.tenant_id=$2 AND t.status='active' FOR SHARE OF m,t`, who.MemberID, who.TenantID).Scan(&id)
	if err != nil {
		tx.Rollback(ctx)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrUnauthorized
		}
		return nil, err
	}
	return tx, nil
}

func billingOwnRule(ctx context.Context, tx pgx.Tx, tenantID, ruleID string) error {
	if !billingUUID.MatchString(ruleID) {
		return ErrInvalid
	}
	var id string
	err := tx.QueryRow(ctx, "SELECT id::text FROM relay_rules WHERE id=$1 AND tenant_id=$2", ruleID, tenantID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func (s *Store) Wallet(ctx context.Context, who Identity) (Wallet, error) {
	result := Wallet{Currency: "USD"}
	tx, err := s.billingReadTx(ctx, who)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	err = tx.QueryRow(ctx, "SELECT balance_cents,spending_frozen FROM billing_wallets WHERE tenant_id=$1", who.TenantID).Scan(&result.BalanceCents, &result.SpendingFrozen)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	result.Initialized = true
	return result, nil
}

// WalletLedger returns at most 100 entries, newest first. Pass the last entry ID
// as before for the next page; optional ruleID filters only this tenant's rule.
func (s *Store) WalletLedger(ctx context.Context, who Identity, ruleID string, before int64) ([]LedgerEntry, error) {
	result := []LedgerEntry{}
	if before < 0 {
		return result, ErrInvalid
	}
	tx, err := s.billingReadTx(ctx, who)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	if ruleID != "" {
		if err = billingOwnRule(ctx, tx, who.TenantID, ruleID); err != nil {
			return result, err
		}
	}
	rows, err := tx.Query(ctx, "SELECT "+ledgerColumns+" FROM billing_ledger WHERE tenant_id=$1 AND ($2='' OR rule_id=NULLIF($2,'')::uuid) AND ($3::bigint=0 OR id<$3) ORDER BY id DESC LIMIT 100", who.TenantID, ruleID, before)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		entry, e := scanLedger(rows)
		if e != nil {
			return result, e
		}
		result = append(result, entry)
	}
	return result, rows.Err()
}

func (s *Store) RuleUsage(ctx context.Context, who Identity, ruleID string) (RuleUsage, error) {
	result := RuleUsage{RuleID: ruleID}
	if !billingUUID.MatchString(ruleID) {
		return result, ErrInvalid
	}
	tx, err := s.billingReadTx(ctx, who)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	err = tx.QueryRow(ctx, `SELECT COALESCE(u.upload_bytes,0),COALESCE(u.download_bytes,0),u.rule_id IS NOT NULL,u.updated_at FROM relay_rules r LEFT JOIN billing_usage_totals u ON u.rule_id=r.id AND u.tenant_id=r.tenant_id WHERE r.id=$1 AND r.tenant_id=$2`, ruleID, who.TenantID).Scan(&result.UploadBytes, &result.DownloadBytes, &result.HasReports, &result.LastReportedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, ErrNotFound
	}
	if err != nil {
		return result, err
	}
	result.TotalBytes = result.UploadBytes + result.DownloadBytes
	return result, nil
}
