package platform

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// Until the parent registers v5, apply only this migration to the test's own schema.
func billingTestStore(t *testing.T) *Store {
	t.Helper()
	s := testStore(t)
	var exists bool
	if err := s.pool.QueryRow(context.Background(), "SELECT to_regclass('billing_wallets') IS NOT NULL").Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		sql, err := os.ReadFile("migration5.sql")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.pool.Exec(context.Background(), string(sql)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.pool.QueryRow(context.Background(), "SELECT to_regclass('subscription_usage') IS NOT NULL").Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		sql, err := os.ReadFile("migration10.sql")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.pool.Exec(context.Background(), string(sql)); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestBillingWalletConcurrentDebitsReplayAndImmutability(t *testing.T) {
	s := billingTestStore(t)
	ctx := context.Background()
	who, _ := register(t, s, "wallet@example.com")
	input := WalletAdjustment{TenantID: who.TenantID, Key: "opening-credit", AmountCents: 100, Reason: "local correction", Operator: "test-operator"}
	first, err := s.AdjustWalletFromCLI(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	input.Reason = "changed reason"
	if _, err = s.AdjustWalletFromCLI(ctx, input); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting replay: %v", err)
	}
	input.Reason = "local correction"
	replay, err := s.AdjustWalletFromCLI(ctx, input)
	if err != nil || replay.ID != first.ID {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			debit := input
			debit.Key = fmt.Sprintf("debit-%08d", i)
			debit.AmountCents = -10
			_, e := s.AdjustWalletFromCLI(ctx, debit)
			results <- e
		}(i)
	}
	wg.Wait()
	close(results)
	succeeded := 0
	for e := range results {
		if e == nil {
			succeeded++
		} else if !errors.Is(e, ErrConflict) {
			t.Fatal(e)
		}
	}
	if succeeded != 10 {
		t.Fatalf("successful debits=%d", succeeded)
	}
	wallet, err := s.Wallet(ctx, who)
	if err != nil || wallet.BalanceCents != 0 || !wallet.Initialized || wallet.Currency != "USD" {
		t.Fatalf("wallet: %+v %v", wallet, err)
	}
	entries, err := s.WalletLedger(ctx, who, "", 0)
	if err != nil || len(entries) != 11 {
		t.Fatalf("ledger: %d %v", len(entries), err)
	}
	older, err := s.WalletLedger(ctx, who, "", entries[5].ID)
	if err != nil || len(older) != 5 || older[0].ID >= entries[5].ID || older[4].ID != first.ID {
		t.Fatalf("exclusive ledger cursor: %+v %v", older, err)
	}
	var sum int64
	for _, e := range entries {
		sum += e.AmountCents
	}
	if sum != 0 {
		t.Fatalf("ledger sum=%d", sum)
	}
	for _, sql := range []string{"UPDATE billing_ledger SET amount_cents=1", "DELETE FROM billing_ledger", "TRUNCATE billing_ledger"} {
		if _, err = s.pool.Exec(ctx, sql); err == nil {
			t.Fatalf("mutable ledger: %s", sql)
		}
	}
	// An exact old replay must succeed even though today's balance differs.
	replay, err = s.AdjustWalletFromCLI(ctx, input)
	if err != nil || replay.BalanceAfterCents != 100 {
		t.Fatalf("old replay: %+v %v", replay, err)
	}
}

func TestBillingWalletRuleReferencesIsolationOverflowAndAuditRollback(t *testing.T) {
	s := billingTestStore(t)
	ctx := context.Background()
	a, _ := register(t, s, "a-billing@example.com")
	b, _ := register(t, s, "b-billing@example.com")
	rule, err := s.CreateRule(ctx, a, "billing-rule", ruleInput("tcp"))
	if err != nil {
		t.Fatal(err)
	}
	input := WalletAdjustment{TenantID: a.TenantID, RuleID: rule.ID, Key: "rule-credit-1", AmountCents: math.MaxInt64, Reason: "correction", Operator: "local"}
	for _, delta := range []int64{0, math.MinInt64, -1} {
		invalid := input
		invalid.AmountCents = delta
		_, e := s.AdjustWalletFromCLI(ctx, invalid)
		want := ErrInvalid
		if delta == -1 {
			want = ErrConflict
		}
		if !errors.Is(e, want) {
			t.Fatalf("invalid/overdraw correction %d: %v", delta, e)
		}
	}
	if _, err = s.AdjustWalletFromCLI(ctx, input); err != nil {
		t.Fatal(err)
	}
	if scoped, e := s.WalletLedger(ctx, a, rule.ID, 0); e != nil || len(scoped) != 1 || scoped[0].RuleID != rule.ID {
		t.Fatalf("rule ledger reference: %+v %v", scoped, e)
	}
	input.Key = "overflow-credit"
	input.AmountCents = 1
	if _, err = s.AdjustWalletFromCLI(ctx, input); !errors.Is(err, ErrInvalid) {
		t.Fatalf("overflow: %v", err)
	}
	input.TenantID = b.TenantID
	if _, err = s.AdjustWalletFromCLI(ctx, input); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign rule: %v", err)
	}
	if rows, err := s.WalletLedger(ctx, b, "", 0); err != nil || len(rows) != 0 {
		t.Fatalf("tenant leak: %+v %v", rows, err)
	}
	if _, err = s.WalletLedger(ctx, b, rule.ID, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rule ledger leak: %v", err)
	}
	if _, err = s.Wallet(ctx, Identity{TenantID: a.TenantID, MemberID: b.MemberID}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("forged identity: %v", err)
	}
	if _, err = s.pool.Exec(ctx, "ALTER TABLE platform_audit_logs ADD CONSTRAINT billing_test_reject CHECK (action <> 'wallet.adjust') NOT VALID"); err != nil {
		t.Fatal(err)
	}
	input.TenantID = b.TenantID
	input.RuleID = ""
	input.Key = "audit-rollback"
	if _, err = s.AdjustWalletFromCLI(ctx, input); err == nil {
		t.Fatal("audit failure accepted")
	}
	wallet, err := s.Wallet(ctx, b)
	if err != nil || wallet.Initialized || wallet.BalanceCents != 0 {
		t.Fatalf("rollback: %+v %v", wallet, err)
	}
}

func TestBillingUsageReplayEpochOwnershipAndRuleIsolation(t *testing.T) {
	s := billingTestStore(t)
	ctx := context.Background()
	a, _ := register(t, s, "usage-a@example.com")
	b, _ := register(t, s, "usage-b@example.com")
	r1, err := s.CreateRule(ctx, a, "usage-rule-1", ruleInput("tcp"))
	if err != nil {
		t.Fatal(err)
	}
	r2, err := s.CreateRule(ctx, a, "usage-rule-2", ruleInput("udp"))
	if err != nil {
		t.Fatal(err)
	}
	input := UsageReport{RuleID: r1.ID, Epoch: "epoch-0001", Sequence: 1, UploadBytes: 100, DownloadBytes: 200}
	if _, err = s.ReportUsage(ctx, "other-node", input); !errors.Is(err, ErrForbidden) {
		t.Fatalf("wrong node: %v", err)
	}
	first, err := s.ReportUsage(ctx, "hk-01", input)
	if err != nil || first.UploadDeltaBytes != 100 || first.DownloadDeltaBytes != 200 {
		t.Fatalf("first: %+v %v", first, err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ack, e := s.ReportUsage(ctx, "hk-01", input)
			if e == nil && (!ack.Duplicate || ack.UploadDeltaBytes != 100) {
				e = fmt.Errorf("bad replay ack: %+v", ack)
			}
			results <- e
		}()
	}
	wg.Wait()
	close(results)
	for e := range results {
		if e != nil {
			t.Fatal(e)
		}
	}
	changed := input
	changed.DownloadBytes++
	if _, err = s.ReportUsage(ctx, "hk-01", changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict replay: %v", err)
	}
	next := input
	next.Sequence = 3
	next.UploadBytes = 130
	next.DownloadBytes = 250
	if _, err = s.ReportUsage(ctx, "hk-01", next); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []UsageReport{
		{RuleID: r1.ID, Epoch: input.Epoch, Sequence: 2, UploadBytes: 140, DownloadBytes: 260},
		{RuleID: r1.ID, Epoch: input.Epoch, Sequence: 4, UploadBytes: 129, DownloadBytes: 250},
		{RuleID: r1.ID, Epoch: input.Epoch, Sequence: 4, UploadBytes: 130, DownloadBytes: 249},
	} {
		if _, err = s.ReportUsage(ctx, "hk-01", bad); !errors.Is(err, ErrConflict) {
			t.Fatalf("backwards report accepted: %+v %v", bad, err)
		}
	}
	reset := UsageReport{RuleID: r1.ID, Epoch: "epoch-0002", Sequence: 1, UploadBytes: 5, DownloadBytes: 7}
	if _, err = s.ReportUsage(ctx, "hk-01", reset); err != nil {
		t.Fatal(err)
	}
	next.Sequence = 4
	next.UploadBytes = 140
	if _, err = s.ReportUsage(ctx, "hk-01", next); !errors.Is(err, ErrConflict) {
		t.Fatalf("retired epoch: %v", err)
	}
	if ack, err := s.ReportUsage(ctx, "hk-01", input); err != nil || !ack.Duplicate {
		t.Fatalf("retired exact replay: %+v %v", ack, err)
	}
	input.RuleID = r2.ID
	input.UploadBytes = 10
	input.DownloadBytes = 20
	if _, err = s.ReportUsage(ctx, "hk-01", input); err != nil {
		t.Fatal(err)
	}
	usage, err := s.RuleUsage(ctx, a, r1.ID)
	if err != nil || usage.UploadBytes != 135 || usage.DownloadBytes != 257 || usage.TotalBytes != 392 {
		t.Fatalf("r1 totals: %+v %v", usage, err)
	}
	usage, err = s.RuleUsage(ctx, a, r2.ID)
	if err != nil || usage.TotalBytes != 30 {
		t.Fatalf("r2 totals: %+v %v", usage, err)
	}
	if _, err = s.RuleUsage(ctx, b, r1.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tenant leak: %v", err)
	}
	if wallet, err := s.Wallet(ctx, a); err != nil || wallet.Initialized {
		t.Fatalf("usage fabricated billing: %+v %v", wallet, err)
	}
	if rows, err := s.WalletLedger(ctx, a, r1.ID, 0); err != nil || len(rows) != 0 {
		t.Fatalf("automatic charge: %+v %v", rows, err)
	}
}

func TestBillingUsageBoundsAndAtomicRollback(t *testing.T) {
	s := billingTestStore(t)
	ctx := context.Background()
	who, _ := register(t, s, "bounds@example.com")
	rule, err := s.CreateRule(ctx, who, "bounds-rule", ruleInput("tcp"))
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []UsageReport{
		{RuleID: rule.ID, Epoch: "epoch-0001", Sequence: 0},
		{RuleID: rule.ID, Epoch: "epoch-0001", Sequence: 1, UploadBytes: -1},
		{RuleID: rule.ID, Epoch: "epoch-0001", Sequence: 1, DownloadBytes: -1},
		{RuleID: rule.ID, Epoch: "epoch-0001", Sequence: 1, UploadBytes: math.MaxInt64, DownloadBytes: 1},
		{RuleID: rule.ID, Epoch: "", Sequence: 1},
	} {
		if _, err = s.ReportUsage(ctx, "hk-01", input); !errors.Is(err, ErrInvalid) {
			t.Fatalf("bounds: %+v %v", input, err)
		}
	}
	input := UsageReport{RuleID: rule.ID, Epoch: "epoch-0001", Sequence: 1, UploadBytes: math.MaxInt64}
	if _, err = s.ReportUsage(ctx, "hk-01", input); err != nil {
		t.Fatal(err)
	}
	input.Epoch = "epoch-0002"
	input.UploadBytes = 1
	if _, err = s.ReportUsage(ctx, "hk-01", input); !errors.Is(err, ErrInvalid) {
		t.Fatalf("aggregate overflow: %v", err)
	}
	var count int
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM billing_usage_reports").Scan(&count); err != nil || count != 1 {
		t.Fatalf("failed report persisted: %d %v", count, err)
	}
	input.UploadBytes = 0
	if _, err = s.ReportUsage(ctx, "hk-01", input); err != nil {
		t.Fatalf("overflow corrupted epoch: %v", err)
	}
	usage, err := s.RuleUsage(ctx, who, rule.ID)
	if err != nil || usage.TotalBytes != math.MaxInt64 {
		t.Fatalf("totals: %+v %v", usage, err)
	}
}

// Removing the shared first-write locks would double-credit or double-count
// these simultaneous identical/conflicting initial operations.
func TestBillingConcurrentFirstWrites(t *testing.T) {
	s := billingTestStore(t)
	ctx := context.Background()
	who, _ := register(t, s, "first-write@example.com")
	rule, err := s.CreateRule(ctx, who, "first-write-rule", ruleInput("tcp"))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, e := s.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: who.TenantID, Key: "concurrent-credit", AmountCents: 10, Reason: "correction", Operator: "local"})
			errs <- e
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	wallet, err := s.Wallet(ctx, who)
	if err != nil || wallet.BalanceCents != 10 {
		t.Fatalf("concurrent wallet: %+v %v", wallet, err)
	}
	type result struct {
		ack UsageReceipt
		err error
	}
	results := make(chan result, 16)
	start = make(chan struct{})
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			up := int64(10)
			if i%2 == 1 {
				up = 20
			}
			ack, e := s.ReportUsage(ctx, "hk-01", UsageReport{RuleID: rule.ID, Epoch: "first-epoch", Sequence: 1, UploadBytes: up, DownloadBytes: 1})
			results <- result{ack, e}
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	accepted, conflicts, unique := 0, 0, 0
	for r := range results {
		if r.err == nil {
			accepted++
			if !r.ack.Duplicate {
				unique++
			}
		} else if errors.Is(r.err, ErrConflict) {
			conflicts++
		} else {
			t.Fatal(r.err)
		}
	}
	if accepted != 8 || conflicts != 8 || unique != 1 {
		t.Fatalf("accepted=%d conflicts=%d unique=%d", accepted, conflicts, unique)
	}
	usage, err := s.RuleUsage(ctx, who, rule.ID)
	if err != nil || (usage.TotalBytes != 11 && usage.TotalBytes != 21) {
		t.Fatalf("concurrent usage: %+v %v", usage, err)
	}
	var reports int
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM billing_usage_reports WHERE tenant_id=$1 AND rule_id=$2", who.TenantID, rule.ID).Scan(&reports); err != nil || reports != 1 {
		t.Fatalf("reports: %d %v", reports, err)
	}
}

func TestBillingUsageInsertFailureRollsBackEpochAndTotals(t *testing.T) {
	s := billingTestStore(t)
	ctx := context.Background()
	who, _ := register(t, s, "usage-rollback@example.com")
	rule, err := s.CreateRule(ctx, who, "usage-rollback-rule", ruleInput("tcp"))
	if err != nil {
		t.Fatal(err)
	}
	input := UsageReport{RuleID: rule.ID, Epoch: "rollback-epoch", Sequence: 1, UploadBytes: 5, DownloadBytes: 10}
	if _, err = s.ReportUsage(ctx, "hk-01", input); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, "ALTER TABLE billing_usage_reports ADD CONSTRAINT test_report_failure CHECK (sequence <> 2) NOT VALID"); err != nil {
		t.Fatal(err)
	}
	input.Sequence = 2
	input.UploadBytes = 15
	if _, err = s.ReportUsage(ctx, "hk-01", input); err == nil {
		t.Fatal("forced insert failure committed")
	}
	usage, err := s.RuleUsage(ctx, who, rule.ID)
	if err != nil || usage.TotalBytes != 15 {
		t.Fatalf("partial total: %+v %v", usage, err)
	}
	if _, err = s.pool.Exec(ctx, "ALTER TABLE billing_usage_reports DROP CONSTRAINT test_report_failure"); err != nil {
		t.Fatal(err)
	}
	ack, err := s.ReportUsage(ctx, "hk-01", input)
	if err != nil || ack.UploadDeltaBytes != 10 {
		t.Fatalf("epoch not rolled back: %+v %v", ack, err)
	}
}

func TestBillingUsageCertificateRotationWaitsForNodeLock(t *testing.T) {
	s := billingTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	who, _ := register(t, s, "usage-rotation@example.com")
	rule, err := s.CreateRule(ctx, who, "rotation-rule", ruleInput("tcp"))
	if err != nil {
		t.Fatal(err)
	}
	oldFingerprint, newFingerprint := strings.Repeat("a", 64), strings.Repeat("b", 64)
	if _, err = s.pool.Exec(ctx, "UPDATE nodes SET certificate_fingerprint=$1 WHERE id='hk-01'", oldFingerprint); err != nil {
		t.Fatal(err)
	}
	// Simulate the credential-changing portion of rotation under its actual lock.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if err = lockNode(ctx, tx, "hk-01"); err != nil {
		t.Fatal(err)
	}
	var pid int
	if err = tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, "UPDATE nodes SET certificate_fingerprint=$1 WHERE id='hk-01'", newFingerprint); err != nil {
		t.Fatal(err)
	}
	input := UsageReport{RuleID: rule.ID, Epoch: "rotation-epoch", Sequence: 1, UploadBytes: 9}
	result := make(chan error, 1)
	go func() { _, e := s.ReportUsage(ctx, "hk-01", input, oldFingerprint); result <- e }()
	// Observe the real lock wait; no scheduling-dependent sleep assertion.
	for {
		var blocked bool
		if err = s.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE $1::int=ANY(pg_blocking_pids(pid)))", pid).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case e := <-result:
			t.Fatalf("usage bypassed rotation lock: %v", e)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-result; !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("old credential accepted: %v", err)
	}
	usage, err := s.RuleUsage(ctx, who, rule.ID)
	if err != nil || usage.HasReports {
		t.Fatalf("rejected report persisted: %+v %v", usage, err)
	}
	if _, err = s.ReportUsage(ctx, "hk-01", input, newFingerprint); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReportUsage(ctx, "hk-01", input, newFingerprint, newFingerprint); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("extra credential accepted: %v", err)
	}
	if _, err = s.ReportUsage(ctx, "hk-01", input, ""); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("empty credential accepted: %v", err)
	}
}

func TestBillingLegacyExactReplayAfterFirstSubscriptionPurchase(t *testing.T) {
	s, _, who, rule, plan := subscriptionFixture(t)
	ctx := context.Background()
	legacy := UsageReport{RuleID: rule.ID, Epoch: "prepaid-legacy-epoch", Sequence: 1, UploadBytes: 7, DownloadBytes: 4}
	first, err := s.ReportUsage(ctx, "hk-01", legacy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: who.TenantID, Key: "legacy-upgrade-funds", AmountCents: 100, Reason: "correction", Operator: "test"}); err != nil {
		t.Fatal(err)
	}
	period, err := s.PurchasePlan(ctx, who, "legacy-first-purchase", PurchasePlanInput{RuleID: rule.ID, PriceVersionID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	ack, err := s.ReportUsage(ctx, "hk-01", legacy)
	if err != nil || !ack.Duplicate || ack.SubscriptionID != "" || ack.UploadDeltaBytes != first.UploadDeltaBytes || ack.DownloadDeltaBytes != first.DownloadDeltaBytes {
		t.Fatalf("legacy WAL cannot drain: %+v %v", ack, err)
	}
	changed := legacy
	changed.UploadBytes++
	if _, err = s.ReportUsage(ctx, "hk-01", changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed legacy replay: %v", err)
	}
	changed = legacy
	changed.SubscriptionID = period.ID
	if _, err = s.ReportUsage(ctx, "hk-01", changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("legacy replay rebound: %v", err)
	}
	changed.Sequence = 2
	if _, err = s.ReportUsage(ctx, "hk-01", changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("legacy epoch rebound: %v", err)
	}
	changed = legacy
	changed.Sequence = 2
	changed.UploadBytes = 8
	if _, err = s.ReportUsage(ctx, "hk-01", changed); !errors.Is(err, ErrInvalid) {
		t.Fatalf("new unbound report accepted: %v", err)
	}
	changed.Epoch = "new-unbound-paid-epoch"
	if _, err = s.ReportUsage(ctx, "hk-01", changed); !errors.Is(err, ErrInvalid) {
		t.Fatalf("new unbound epoch accepted: %v", err)
	}
	periods, err := s.RuleSubscriptions(ctx, who, rule.ID, 0)
	if err != nil || len(periods) != 1 || periods[0].UsedBytes != 0 {
		t.Fatalf("legacy usage implicitly charged: %+v %v", periods, err)
	}
	usage, err := s.RuleUsage(ctx, who, rule.ID)
	if err != nil || usage.TotalBytes != 11 {
		t.Fatalf("legacy replay counted twice: %+v %v", usage, err)
	}
}
