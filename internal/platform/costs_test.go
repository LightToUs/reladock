package platform

import (
	"context"
	"fmt"
	"math"
	"os"
	"sync"
	"testing"
	"time"
)

func costStore(t *testing.T) *Store {
	t.Helper()
	s := testStore(t)
	ctx := context.Background()
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT to_regclass('node_cost_entries') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		sql, err := os.ReadFile("migration16.sql")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.pool.Exec(ctx, string(sql)); err != nil {
			t.Fatal(err)
		}
	}
	return s
}
func costInput(invoice string, amount int64) NodeCostInput {
	return NodeCostInput{NodeID: "hk-01", AmountCents: amount, InvoiceRef: invoice, ServiceFrom: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), ServiceTo: time.Date(2000, 2, 1, 0, 0, 0, 0, time.UTC)}
}
func costWindow() (time.Time, time.Time) {
	now := time.Now().UTC()
	return now.Add(-time.Hour), now.Add(time.Hour)
}

func TestCostsAdminPermissionReplayDedupAndImmutableHistory(t *testing.T) {
	s := costStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	owner, _ := register(t, s, "cost-owner@example.com")
	from, to := costWindow()
	input := costInput("INV-ONE", 120)
	if _, err := s.AdminRecordNodeCost(ctx, owner, "cost-idem-key", input); err != ErrForbidden {
		t.Fatalf("owner wrote costs: %v", err)
	}
	if _, err := s.AdminCashMargin(ctx, owner, from, to, 1); err != ErrForbidden {
		t.Fatalf("owner read margin: %v", err)
	}
	if _, err := s.AdminNodeCosts(WithAPIKey(ctx, "fake-key"), admin, from, to, 1); err != ErrForbidden {
		t.Fatalf("key read costs: %v", err)
	}
	if _, err := s.AdminRecordNodeCost(WithAPIKey(ctx, "fake-key"), admin, "cost-idem-key", input); err != ErrForbidden {
		t.Fatalf("key wrote costs: %v", err)
	}
	originalEmail := admin.Email
	admin.Email = "forged@example.com"
	var wg sync.WaitGroup
	entries := make(chan NodeCostEntry, 6)
	errs := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, e := s.AdminRecordNodeCost(ctx, admin, "cost-idem-key", input)
			entries <- v
			errs <- e
		}()
	}
	wg.Wait()
	close(entries)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var id string
	for entry := range entries {
		if id == "" {
			id = entry.ID
		}
		if entry.ID != id || entry.ActorID != admin.MemberID || entry.ActorEmail != originalEmail || entry.Currency != "USD" {
			t.Fatalf("replay/actor: %+v", entry)
		}
	}
	changed := input
	changed.AmountCents++
	if _, err := s.AdminRecordNodeCost(ctx, admin, "cost-idem-key", changed); err != ErrConflict {
		t.Fatalf("changed replay: %v", err)
	}
	if _, err := s.AdminRecordNodeCost(ctx, admin, "cost-other-key", input); err != ErrConflict {
		t.Fatalf("invoice duplicate: %v", err)
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM platform_audit_logs WHERE action='node-cost.record'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate audit: %d %v", count, err)
	}
	for _, sql := range []string{`UPDATE node_cost_entries SET amount_cents=1`, `DELETE FROM node_cost_entries`, `TRUNCATE node_cost_entries`} {
		if _, err := s.pool.Exec(ctx, sql); err == nil {
			t.Fatalf("immutable history accepted: %s", sql)
		}
	}
	if err := s.SetPlatformAdmin(ctx, originalEmail, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdminCashMargin(ctx, admin, from, to, 1); err != ErrForbidden {
		t.Fatalf("stale admin retained permission: %v", err)
	}
}

func TestCostsAuditFailureAndInvalidInputsDoNotPersist(t *testing.T) {
	s := costStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	for _, mutate := range []func(*NodeCostInput){func(v *NodeCostInput) { v.AmountCents = 0 }, func(v *NodeCostInput) { v.AmountCents = -1 }, func(v *NodeCostInput) { v.ServiceFrom = time.Time{} }, func(v *NodeCostInput) { v.ServiceTo = v.ServiceFrom }, func(v *NodeCostInput) { v.InvoiceRef = " " }, func(v *NodeCostInput) { v.InvoiceRef = "bad\ninvoice" }, func(v *NodeCostInput) { v.NodeID = "bad/path" }} {
		input := costInput("INVALID", 20)
		mutate(&input)
		if _, err := s.AdminRecordNodeCost(ctx, admin, "invalid-cost-key", input); err != ErrInvalid {
			t.Fatalf("invalid cost: %+v %v", input, err)
		}
	}
	if _, err := s.pool.Exec(ctx, `ALTER TABLE platform_audit_logs ADD CONSTRAINT fail_cost CHECK(action<>'node-cost.record') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdminRecordNodeCost(ctx, admin, "rollback-cost-key", costInput("ROLLBACK", 100)); err == nil {
		t.Fatal("audit failure accepted")
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM node_cost_entries`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed cost leaked: %d %v", count, err)
	}
}

func TestCostsCashMarginUsesPurchasesNotTopupsAndKeepsDeletedRule(t *testing.T) {
	s := costStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	owner, _ := register(t, s, "cost-margin@example.com")
	rule, err := s.CreateRule(ctx, owner, "cost-revenue-rule", ruleInput("tcp"))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := s.AdminCreatePlan(ctx, admin, configuredPlan())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: owner.TenantID, Key: "cost-funding-key", AmountCents: 500, Reason: "isolated fixture", Operator: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PurchasePlan(ctx, owner, "cost-purchase-key", PurchasePlanInput{RuleID: rule.ID, PriceVersionID: plan.ID}); err != nil {
		t.Fatal(err)
	}
	// A top-up is wallet funding, not revenue. It must not affect this report.
	if _, err = s.pool.Exec(ctx, `INSERT INTO billing_ledger(tenant_id,kind,amount_cents,balance_after_cents,idempotency_key,reason,operator_name) VALUES($1,'payment-credit',10000,10450,'test-topup-credit','test fixture','test')`, owner.TenantID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO nodes(id,name,status,listen_address) VALUES('sg-02','Other','unprovisioned','0.0.0.0')`); err != nil {
		t.Fatal(err)
	}
	from, to := costWindow()
	report, err := s.AdminCashMargin(ctx, admin, from, to, 1)
	if err != nil || report.Totals.RevenueCents != 50 || report.Totals.HasCost || report.Totals.CostCents != nil || report.Totals.MarginCents != nil {
		t.Fatalf("unfilled costs/top-up revenue: %+v %v", report, err)
	}
	if _, err = s.AdminRecordNodeCost(ctx, admin, "cost-hk-invoice", costInput("HK-INVOICE", 25)); err != nil {
		t.Fatal(err)
	}
	report, err = s.AdminCashMargin(ctx, admin, from, to, 1)
	if err != nil || !report.Totals.HasCost || report.Totals.CostCoverageComplete || report.Totals.MarginCents != nil || *report.Totals.CostCents != 25 {
		t.Fatalf("partial cost coverage: %+v %v", report, err)
	}
	second := costInput("SG-INVOICE", 10)
	second.NodeID = "sg-02"
	if _, err = s.AdminRecordNodeCost(ctx, admin, "cost-sg-invoice", second); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteRule(ctx, owner, rule.ID); err != nil {
		t.Fatal(err)
	}
	report, err = s.AdminCashMargin(ctx, admin, from, to, 1)
	if err != nil || report.Basis != "cash-basis" || report.TimeBasis != "recorded-at" || report.Totals.RevenueCents != 50 || !report.Totals.CostCoverageComplete || report.Totals.MarginCents == nil || *report.Totals.MarginCents != 15 || len(report.Nodes.Items) != 2 {
		t.Fatalf("cash report/history: %+v %v", report, err)
	}
	emptyPage, err := s.AdminCashMargin(ctx, admin, from, to, 2)
	if err != nil || len(emptyPage.Nodes.Items) != 0 || emptyPage.Nodes.Total != 2 || emptyPage.Totals.MarginCents == nil || *emptyPage.Totals.MarginCents != 15 {
		t.Fatalf("totals depend on node page: %+v %v", emptyPage, err)
	}
}

func TestCostsPeriodUsesRecordedAtHalfOpenNotServicePeriod(t *testing.T) {
	s := costStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	entry, err := s.AdminRecordNodeCost(ctx, admin, "cost-time-boundary", costInput("OLD-SERVICE-PERIOD", 150))
	if err != nil {
		t.Fatal(err)
	}
	report, err := s.AdminCashMargin(ctx, admin, entry.RecordedAt, entry.RecordedAt.Add(time.Microsecond), 1)
	if err != nil || report.Totals.CostCents == nil || *report.Totals.CostCents != 150 {
		t.Fatalf("inclusive start or service filter: %+v %v", report, err)
	}
	report, err = s.AdminCashMargin(ctx, admin, entry.RecordedAt.Add(-time.Microsecond), entry.RecordedAt, 1)
	if err != nil || report.Totals.HasCost || report.Totals.CostCents != nil {
		t.Fatalf("exclusive end: %+v %v", report, err)
	}
	list, err := s.AdminNodeCosts(ctx, admin, entry.RecordedAt, entry.RecordedAt.Add(time.Second), 1)
	if err != nil || list.Total != 1 || len(list.Items) != 1 {
		t.Fatalf("recorded list: %+v %v", list, err)
	}
	if _, err = s.AdminCashMargin(ctx, admin, time.Time{}, entry.RecordedAt, 1); err != ErrInvalid {
		t.Fatalf("missing explicit start: %v", err)
	}
	if _, err = s.AdminCashMargin(ctx, admin, entry.RecordedAt, entry.RecordedAt, 1); err != ErrInvalid {
		t.Fatalf("empty period: %v", err)
	}
}

func TestCostsNumericAggregationRejectsOverflow(t *testing.T) {
	s := costStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	from, to := costWindow()
	for i := 0; i < 2; i++ {
		if _, err := s.AdminRecordNodeCost(ctx, admin, fmt.Sprintf("cost-overflow-%d", i), costInput(fmt.Sprintf("HUGE-%d", i), math.MaxInt64)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AdminCashMargin(ctx, admin, from, to, 1); err != ErrInvalid {
		t.Fatalf("numeric sum overflow wrapped/succeeded: %v", err)
	}
}

func TestCostsPlatformOverflowAcrossNodesAndActualPurchaseRevenue(t *testing.T) {
	t.Run("platform-cost-total", func(t *testing.T) {
		s := costStore(t)
		ctx := context.Background()
		admin, _ := adminIdentity(t, s)
		from, to := costWindow()
		if _, err := s.pool.Exec(ctx, `INSERT INTO nodes(id,name,status,listen_address) VALUES('sg-02','Other','unprovisioned','0.0.0.0')`); err != nil {
			t.Fatal(err)
		}
		for i, node := range []string{"hk-01", "sg-02"} {
			input := costInput(fmt.Sprintf("TOTAL-%d", i), math.MaxInt64)
			input.NodeID = node
			if _, err := s.AdminRecordNodeCost(ctx, admin, fmt.Sprintf("total-cost-%d", i), input); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.AdminCashMargin(ctx, admin, from, to, 2); err != ErrInvalid {
			t.Fatalf("off-page platform sum overflow: %v", err)
		}
	})
	t.Run("actual-purchase-revenue", func(t *testing.T) {
		s := costStore(t)
		ctx := context.Background()
		admin, _ := adminIdentity(t, s)
		from, to := costWindow()
		input := configuredPlan()
		input.Code = "max-revenue"
		input.PriceCents = math.MaxInt64
		plan, err := s.AdminCreatePlan(ctx, admin, input)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			who, _ := register(t, s, fmt.Sprintf("cost-max-%d@example.com", i))
			rule, e := s.CreateRule(ctx, who, "cost-max-rule", ruleInput("tcp"))
			if e != nil {
				t.Fatal(e)
			}
			if _, e = s.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: who.TenantID, Key: "cost-max-funding", AmountCents: math.MaxInt64, Reason: "isolated fixture", Operator: "test"}); e != nil {
				t.Fatal(e)
			}
			if _, e = s.PurchasePlan(ctx, who, "cost-max-purchase", PurchasePlanInput{RuleID: rule.ID, PriceVersionID: plan.ID}); e != nil {
				t.Fatal(e)
			}
			report, e := s.AdminCashMargin(ctx, admin, from, to, 1)
			if i == 0 {
				if e != nil || report.Totals.RevenueCents != math.MaxInt64 {
					t.Fatalf("valid int64 boundary: %+v %v", report, e)
				}
			} else if e != ErrInvalid {
				t.Fatalf("revenue overflow wrapped/succeeded: %v", e)
			}
		}
	})
}
