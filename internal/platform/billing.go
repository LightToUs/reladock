package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
// Development reports instead carry the checksummed snapshot generation, which is
// re-authorized from the immutable delivery payload even after bypass is off.
// The future collector must obtain the ID from authoritative configuration and
// start a fresh epoch/counter at every period change; never relabel buffered WAL.
type UsageReport struct {
	RuleID            string `json:"ruleId"`
	SubscriptionID    string `json:"subscriptionId,omitempty"`
	Epoch             string `json:"epoch"`
	Sequence          int64  `json:"sequence"`
	UploadBytes       int64  `json:"uploadBytes"`
	DownloadBytes     int64  `json:"downloadBytes"`
	ConfigGeneration  int64  `json:"configGeneration,omitempty"`
	DevelopmentBypass bool   `json:"developmentBypass,omitempty"`
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

type RuleTelemetry struct {
	RuleID                 string     `json:"ruleId"`
	BandwidthMbps          int        `json:"bandwidthMbps"`
	UploadBytes            int64      `json:"uploadBytes"`
	DownloadBytes          int64      `json:"downloadBytes"`
	TotalBytes             int64      `json:"totalBytes"`
	HasReports             bool       `json:"hasReports"`
	LastReportedAt         *time.Time `json:"lastReportedAt"`
	RateState              string     `json:"rateState"`
	UploadBytesPerSecond   *int64     `json:"uploadBytesPerSecond"`
	DownloadBytesPerSecond *int64     `json:"downloadBytesPerSecond"`
	TotalBytesPerSecond    *int64     `json:"totalBytesPerSecond"`
	UtilizationBasisPoints *int64     `json:"utilizationBasisPoints"`
	RateWindowSeconds      *int64     `json:"rateWindowSeconds"`
}

type UsagePoint struct {
	StartedAt     time.Time `json:"startedAt"`
	UploadBytes   int64     `json:"uploadBytes"`
	DownloadBytes int64     `json:"downloadBytes"`
	TotalBytes    int64     `json:"totalBytes"`
}

type RuleTelemetrySeries struct {
	Summary       RuleTelemetry `json:"summary"`
	Window        string        `json:"window"`
	BucketSeconds int64         `json:"bucketSeconds"`
	Points        []UsagePoint  `json:"points"`
}

type telemetryHistorySnapshotHookKey struct{}

// ReportUsage atomically records a cumulative report and its deltas. It does not
// infer prices, payment, health or deployment. Explicitly enabled quota renewal
// may debit the wallet in the same transaction. Soft-deleted
// rules still accept buffered reports from their owning node. First report in a
// new epoch adds its full counters. Explicitly bound epochs accept monotonic late
// reports for their original period, including closed periods; legacy unbound
// retired epochs accept exact replays only. Receipt times never assign a period.
func (s *Store) ReportUsage(ctx context.Context, nodeID string, input UsageReport, fingerprint ...string) (UsageReceipt, error) {
	receipt := UsageReceipt{RuleID: input.RuleID, SubscriptionID: input.SubscriptionID, Epoch: input.Epoch, Sequence: input.Sequence}
	if !nodeIDPattern.MatchString(nodeID) || !billingUUID.MatchString(input.RuleID) || input.SubscriptionID != "" && !billingUUID.MatchString(input.SubscriptionID) || !idempotencyKey.MatchString(input.Epoch) || input.Sequence <= 0 || !billingBytePair(input.UploadBytes, input.DownloadBytes) || input.DevelopmentBypass && (input.SubscriptionID != "" || input.ConfigGeneration < 1) || !input.DevelopmentBypass && input.ConfigGeneration != 0 {
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
	if input.DevelopmentBypass {
		var payload []byte
		var checksum string
		err = tx.QueryRow(ctx, "SELECT payload,checksum FROM config_outbox WHERE node_id=$1 AND generation=$2", nodeID, input.ConfigGeneration).Scan(&payload, &checksum)
		if errors.Is(err, pgx.ErrNoRows) {
			return receipt, ErrForbidden
		}
		if err != nil {
			return receipt, err
		}
		if len(payload) > 2<<20 {
			return receipt, ErrForbidden
		}
		hash := sha256.Sum256(payload)
		if hex.EncodeToString(hash[:]) != checksum {
			return receipt, ErrForbidden
		}
		var config NodeConfig
		if err = json.Unmarshal(payload, &config); err != nil || (config.SchemaVersion != 1 && config.SchemaVersion != 2) || config.NodeID != nodeID || config.Generation != input.ConfigGeneration {
			return receipt, ErrForbidden
		}
		authorized := false
		matches := 0
		for _, rule := range config.Rules {
			if rule.ID == input.RuleID {
				matches++
				if rule.Status == "pending-agent" && rule.DevelopmentBypass {
					authorized = true
				}
			}
		}
		if !authorized || matches != 1 {
			return receipt, ErrForbidden
		}
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
		if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM rule_subscriptions p WHERE p.rule_id=$1 OR EXISTS(SELECT 1 FROM subscription_usage u WHERE u.subscription_id=p.id AND u.rule_id=$1))", input.RuleID).Scan(&subscribed); err != nil {
			return receipt, err
		}
		if subscribed && !input.DevelopmentBypass {
			return receipt, ErrInvalid
		}
	} else {
		err = tx.QueryRow(ctx, `SELECT u.upload_bytes,u.download_bytes FROM subscription_usage u JOIN rule_subscriptions p ON p.id=u.subscription_id WHERE u.subscription_id=$1 AND u.rule_id=$2 AND u.tenant_id=$3 AND u.node_id=$4`, input.SubscriptionID, input.RuleID, tenantID, nodeID).Scan(&periodUp, &periodDown)
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
		if _, err = tx.Exec(ctx, "UPDATE subscription_usage SET upload_bytes=$3,download_bytes=$4,updated_at=clock_timestamp() WHERE subscription_id=$1 AND rule_id=$2", input.SubscriptionID, input.RuleID, periodUp, periodDown); err != nil {
			return receipt, err
		}
	}
	if err = s.EnforceRuleQuota(ctx, tx, input.RuleID); err != nil {
		return receipt, err
	}
	return receipt, tx.Commit(ctx)
}

func billingBytePair(up, down int64) bool { return up >= 0 && down >= 0 && up <= math.MaxInt64-down }

// Revalidate membership against DB; no role or tenant supplied by the browser
// can widen scope. Viewers may read. Locks keep suspension/reassignment from
// crossing an already-authorized query. The outer HTTP layer verifies sessions.
func (s *Store) billingReadTx(ctx context.Context, who Identity) (pgx.Tx, error) {
	return s.billingReadTxWithOptions(ctx, who, pgx.TxOptions{})
}

func (s *Store) billingSnapshotReadTx(ctx context.Context, who Identity) (pgx.Tx, error) {
	return s.billingReadTxWithOptions(ctx, who, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
}

func (s *Store) billingReadTxWithOptions(ctx context.Context, who Identity, options pgx.TxOptions) (pgx.Tx, error) {
	if !billingUUID.MatchString(who.MemberID) || !billingUUID.MatchString(who.TenantID) {
		return nil, ErrUnauthorized
	}
	tx, err := s.pool.BeginTx(ctx, options)
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

const telemetryFreshAfter = 30 * time.Second

type telemetryWindow struct {
	duration time.Duration
	bucket   int64
}

var telemetryWindows = map[string]telemetryWindow{
	"1h":  {duration: time.Hour, bucket: 60},
	"24h": {duration: 24 * time.Hour, bucket: 15 * 60},
	"7d":  {duration: 7 * 24 * time.Hour, bucket: 2 * 60 * 60},
}

func telemetryRounded(value float64) int64 {
	if value >= float64(math.MaxInt64) {
		return math.MaxInt64
	}
	if value <= 0 {
		return 0
	}
	return int64(math.Round(value))
}

func applyTelemetryRate(value *RuleTelemetry, latest, previous time.Time, uploadDelta, downloadDelta int64, now time.Time) {
	value.RateState = "insufficient"
	age := now.Sub(latest)
	if age > telemetryFreshAfter {
		value.RateState = "stale"
		return
	}
	interval := latest.Sub(previous)
	if age < 0 || interval < time.Second || interval > time.Minute {
		return
	}
	seconds := interval.Seconds()
	upload := telemetryRounded(float64(uploadDelta) / seconds)
	download := telemetryRounded(float64(downloadDelta) / seconds)
	total := telemetryRounded(float64(uploadDelta+downloadDelta) / seconds)
	window := int64(math.Round(seconds))
	utilization := telemetryRounded(float64(total) * 8 * 10000 / (float64(value.BandwidthMbps) * 1_000_000))
	value.RateState = "fresh"
	value.UploadBytesPerSecond = &upload
	value.DownloadBytesPerSecond = &download
	value.TotalBytesPerSecond = &total
	value.UtilizationBasisPoints = &utilization
	value.RateWindowSeconds = &window
}

func ruleTelemetryRows(ctx context.Context, tx pgx.Tx, tenantID, ruleID string) ([]RuleTelemetry, error) {
	rows, err := tx.Query(ctx, `SELECT r.id::text,r.bandwidth_mbps,
	COALESCE(t.upload_bytes,0),COALESCE(t.download_bytes,0),t.rule_id IS NOT NULL,t.updated_at,
	l.upload_delta_bytes,l.download_delta_bytes,l.received_at,p.received_at
	FROM relay_rules r
	LEFT JOIN billing_usage_totals t ON t.rule_id=r.id AND t.tenant_id=r.tenant_id
	LEFT JOIN LATERAL (
	 SELECT epoch,sequence,upload_delta_bytes,download_delta_bytes,received_at
	 FROM billing_usage_reports
	 WHERE rule_id=r.id AND tenant_id=r.tenant_id
	 ORDER BY received_at DESC,epoch DESC,sequence DESC LIMIT 1
	) l ON true
	LEFT JOIN LATERAL (
	 SELECT received_at
	 FROM billing_usage_reports
	 WHERE rule_id=r.id AND tenant_id=r.tenant_id AND epoch=l.epoch AND sequence<l.sequence
	 ORDER BY sequence DESC LIMIT 1
	) p ON true
	WHERE r.tenant_id=$1 AND r.status<>'deleted' AND ($2='' OR r.id::text=$2)
	ORDER BY r.created_at,r.id`, tenantID, ruleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []RuleTelemetry{}
	now := time.Now().UTC()
	for rows.Next() {
		value := RuleTelemetry{RateState: "insufficient"}
		var uploadDelta, downloadDelta *int64
		var latest, previous *time.Time
		if err = rows.Scan(&value.RuleID, &value.BandwidthMbps, &value.UploadBytes, &value.DownloadBytes, &value.HasReports, &value.LastReportedAt, &uploadDelta, &downloadDelta, &latest, &previous); err != nil {
			return nil, err
		}
		value.TotalBytes = value.UploadBytes + value.DownloadBytes
		if latest != nil {
			value.LastReportedAt = latest
		}
		if latest != nil && previous != nil && uploadDelta != nil && downloadDelta != nil {
			applyTelemetryRate(&value, *latest, *previous, *uploadDelta, *downloadDelta, now)
		} else if latest != nil && now.Sub(*latest) > telemetryFreshAfter {
			value.RateState = "stale"
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

// RuleTelemetry returns only this tenant's undeleted rules. Recent rates are
// bounded receive-time estimates for display, never billing or health facts.
func (s *Store) RuleTelemetry(ctx context.Context, who Identity) ([]RuleTelemetry, error) {
	tx, err := s.billingReadTx(ctx, who)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	return ruleTelemetryRows(ctx, tx, who.TenantID, "")
}

// RuleTelemetryHistory aggregates immutable accepted deltas by platform receive
// time. Empty buckets are intentionally absent: no report is not evidence of zero
// traffic or a healthy data plane.
func (s *Store) RuleTelemetryHistory(ctx context.Context, who Identity, ruleID, window string) (RuleTelemetrySeries, error) {
	result := RuleTelemetrySeries{Window: window, Points: []UsagePoint{}}
	selected, ok := telemetryWindows[window]
	if !ok || !billingUUID.MatchString(ruleID) {
		return result, ErrInvalid
	}
	result.BucketSeconds = selected.bucket
	tx, err := s.billingSnapshotReadTx(ctx, who)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	items, err := ruleTelemetryRows(ctx, tx, who.TenantID, ruleID)
	if err != nil {
		return result, err
	}
	if len(items) == 0 {
		return result, ErrNotFound
	}
	result.Summary = items[0]
	// Package-local test synchronization point for proving the two statements
	// remain on one database snapshot while a report commits concurrently.
	if hook, ok := ctx.Value(telemetryHistorySnapshotHookKey{}).(func()); ok {
		hook()
	}
	rows, err := tx.Query(ctx, `SELECT date_bin(make_interval(secs => $3),received_at,TIMESTAMPTZ '2000-01-01 00:00:00+00'),
	SUM(upload_delta_bytes)::bigint,SUM(download_delta_bytes)::bigint
	FROM billing_usage_reports
	WHERE tenant_id=$1 AND rule_id=$2 AND received_at>=$4
	GROUP BY 1 ORDER BY 1`, who.TenantID, ruleID, selected.bucket, time.Now().UTC().Add(-selected.duration))
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var point UsagePoint
		if err = rows.Scan(&point.StartedAt, &point.UploadBytes, &point.DownloadBytes); err != nil {
			return result, err
		}
		point.TotalBytes = point.UploadBytes + point.DownloadBytes
		result.Points = append(result.Points, point)
	}
	return result, rows.Err()
}
