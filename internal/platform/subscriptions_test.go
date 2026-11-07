package platform

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func subscriptionTestStore(t *testing.T) *Store {
	t.Helper()
	s := billingTestStore(t)
	var exists bool
	if err := s.pool.QueryRow(context.Background(), "SELECT to_regclass('plan_price_versions') IS NOT NULL").Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		sql, err := os.ReadFile("migration7.sql")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.pool.Exec(context.Background(), string(sql)); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func configuredPlan() PlanInput {
	return PlanInput{Code: "small-rule", Version: 1, Name: "Configured rule plan", PriceCents: 50, QuotaBytes: 100, BandwidthMbps: 100, PeriodSeconds: 3600, ResourceMode: ResourceModeShared}
}

func TestAdminCreatePlanAcceptsNumericCodeAndAssignsVersion(t *testing.T) {
	s := subscriptionTestStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	offering := publishedOffering(t, s, admin, "auto-numeric-version", "hk-01")
	input := configuredPlan()
	input.OfferingID = offering.ID
	input.Code = "001"
	input.Version = 0
	input.PriceCents = 10000

	created, err := s.AdminCreatePlan(ctx, admin, input)
	if err != nil {
		t.Fatal(err)
	}
	if created.Code != "001" || created.Version != 1 || created.PriceCents != 10000 {
		t.Fatalf("created plan = %+v", created)
	}
}

func TestAdminCreatePlanNormalizesCodeBeforeAutomaticVersioning(t *testing.T) {
	s := subscriptionTestStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	offering := publishedOffering(t, s, admin, "auto-normalized-version", "hk-01")
	input := configuredPlan()
	input.OfferingID = offering.ID
	input.Code = "  Plan-001  "
	input.Version = 0

	created, err := s.AdminCreatePlan(ctx, admin, input)
	if err != nil {
		t.Fatal(err)
	}
	if created.Code != "plan-001" || created.Version != 1 {
		t.Fatalf("created plan = %+v", created)
	}
}

func TestAdminCreatePlanAutoVersionReplaysLatestAndIncrementsChangedPrice(t *testing.T) {
	s := subscriptionTestStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	offering := publishedOffering(t, s, admin, "auto-version-replay", "hk-01")
	input := configuredPlan()
	input.OfferingID = offering.ID
	input.Code = "auto-version"
	input.Version = 0

	first, err := s.AdminCreatePlan(ctx, admin, input)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.AdminCreatePlan(ctx, admin, input)
	if err != nil || replay.ID != first.ID || replay.Version != 1 {
		t.Fatalf("replay = %+v, first = %+v, err = %v", replay, first, err)
	}
	input.PriceCents++
	second, err := s.AdminCreatePlan(ctx, admin, input)
	if err != nil || second.ID == first.ID || second.Version != 2 {
		t.Fatalf("second = %+v, first = %+v, err = %v", second, first, err)
	}
}

func TestAdminCreatePlanSerializesConcurrentAutomaticVersions(t *testing.T) {
	s := subscriptionTestStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	offering := publishedOffering(t, s, admin, "auto-version-concurrent", "hk-01")
	base := configuredPlan()
	base.OfferingID = offering.ID
	base.Code = "concurrent-version"
	base.Version = 0

	start := make(chan struct{})
	results := make(chan PlanPriceVersion, 2)
	errors := make(chan error, 2)
	var wg sync.WaitGroup
	for _, price := range []int64{100, 200} {
		wg.Add(1)
		go func(price int64) {
			defer wg.Done()
			<-start
			input := base
			input.PriceCents = price
			plan, err := s.AdminCreatePlan(ctx, admin, input)
			if err != nil {
				errors <- err
				return
			}
			results <- plan
		}(price)
	}
	close(start)
	wg.Wait()
	close(results)
	close(errors)
	for err := range errors {
		t.Fatal(err)
	}
	versions := map[int64]bool{}
	for plan := range results {
		versions[plan.Version] = true
	}
	if len(versions) != 2 || !versions[1] || !versions[2] {
		t.Fatalf("automatic versions = %v, want 1 and 2", versions)
	}
}

func TestAdminCreatePlanWaitsForAutomaticVersionCodeLock(t *testing.T) {
	s := subscriptionTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	admin, _ := adminIdentity(t, s)
	offering := publishedOffering(t, s, admin, "auto-version-lock", "hk-01")
	input := configuredPlan()
	input.OfferingID = offering.ID
	input.Code = "locked-version"
	input.Version = 0

	control, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Rollback(context.Background())
	if _, err = control.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "plan-version:"+input.Code); err != nil {
		t.Fatal(err)
	}
	name := "auto-plan-lock-" + newID()
	blockedStore := namedTestStore(t, s, name)
	done := make(chan struct {
		plan PlanPriceVersion
		err  error
	}, 1)
	go func() {
		plan, createErr := blockedStore.AdminCreatePlan(ctx, admin, input)
		done <- struct {
			plan PlanPriceVersion
			err  error
		}{plan: plan, err: createErr}
	}()
	waitTestLock(t, s, ctx, name)
	if err = control.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if result.err != nil || result.plan.Version != 1 {
			t.Fatalf("plan = %+v, err = %v", result.plan, result.err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestAdminCreatePlanAutomaticVersionAuditFailureRollsBack(t *testing.T) {
	s := subscriptionTestStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	offering := publishedOffering(t, s, admin, "auto-version-audit", "hk-01")
	input := configuredPlan()
	input.OfferingID = offering.ID
	input.Code = "audit-version"
	input.Version = 0
	if _, err := s.pool.Exec(ctx, `CREATE FUNCTION reject_plan_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='plan.create' THEN RAISE EXCEPTION 'reject plan audit'; END IF; RETURN NEW; END; $$; CREATE TRIGGER reject_plan_audit BEFORE INSERT ON platform_audit_logs FOR EACH ROW EXECUTE FUNCTION reject_plan_audit()`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdminCreatePlan(ctx, admin, input); err == nil {
		t.Fatal("automatic plan creation succeeded without audit")
	}
	var count int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM plan_price_versions WHERE code=$1", input.Code).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rolled back plan count = %d, err = %v", count, err)
	}
	if _, err := s.pool.Exec(ctx, "DROP TRIGGER reject_plan_audit ON platform_audit_logs; DROP FUNCTION reject_plan_audit()"); err != nil {
		t.Fatal(err)
	}
	created, err := s.AdminCreatePlan(ctx, admin, input)
	if err != nil || created.Version != 1 {
		t.Fatalf("plan after audit recovery = %+v, err = %v", created, err)
	}
}

func publishedOffering(t *testing.T, s *Store, admin Identity, code, nodeID string) LineOffering {
	t.Helper()
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, "UPDATE nodes SET status='ready' WHERE id=$1", nodeID); err != nil {
		t.Fatal(err)
	}
	offering, err := s.AdminCreateOffering(ctx, admin, LineOfferingInput{Code: code, Name: code, NodeID: nodeID, SourceMarket: "Source", DestinationMarket: "Destination"})
	if err != nil {
		t.Fatal(err)
	}
	offering, err = s.AdminSetOfferingState(ctx, admin, offering.ID, OfferingStateInput{ExpectedRevision: offering.Revision, State: OfferingStatePublished})
	if err != nil {
		t.Fatal(err)
	}
	return offering
}

func TestPurchasePlanAllowsUnprovisionedNodeWithoutClaimingReady(t *testing.T) {
	s := subscriptionTestStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	owner, _ := register(t, s, "unprovisioned-sale@example.com")
	offering, err := s.AdminCreateOffering(ctx, admin, LineOfferingInput{Code: "unprovisioned-sale", Name: "Unprovisioned sale", NodeID: "hk-01"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, "UPDATE line_offerings SET state='published' WHERE id=$1", offering.ID); err != nil {
		t.Fatal(err)
	}
	input := configuredPlan()
	input.Code = "unprovisioned-sale"
	input.OfferingID = offering.ID
	plan, err := s.AdminCreatePlan(ctx, admin, input)
	if err != nil {
		t.Fatal(err)
	}
	rule, err := s.CreateRule(ctx, owner, "unprovisioned-sale-rule", ruleInput("tcp"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: owner.TenantID, Key: "unprovisioned-sale-funds", AmountCents: 100, Reason: "test", Operator: "test"}); err != nil {
		t.Fatal(err)
	}
	period, err := s.PurchasePlan(ctx, owner, "unprovisioned-sale-purchase", PurchasePlanInput{RuleID: rule.ID, PriceVersionID: plan.ID})
	if err != nil || period.Status != "active" || period.EndsAt.Sub(period.StartsAt) != time.Hour {
		t.Fatalf("purchase period=%+v err=%v", period, err)
	}
	var nodeStatus string
	if err = s.pool.QueryRow(ctx, "SELECT status FROM nodes WHERE id='hk-01'").Scan(&nodeStatus); err != nil || nodeStatus != "unprovisioned" {
		t.Fatalf("node status=%q err=%v", nodeStatus, err)
	}
	if _, err = s.pool.Exec(ctx, "UPDATE nodes SET status='maintenance' WHERE id='hk-01'"); err != nil {
		t.Fatal(err)
	}
	replay, err := s.PurchasePlan(ctx, owner, "unprovisioned-sale-purchase", PurchasePlanInput{RuleID: rule.ID, PriceVersionID: plan.ID})
	if err != nil || replay.ID != period.ID {
		t.Fatalf("idempotent replay after maintenance=%+v err=%v", replay, err)
	}
	var balance int64
	if err = s.pool.QueryRow(ctx, "SELECT balance_cents FROM billing_wallets WHERE tenant_id=$1", owner.TenantID).Scan(&balance); err != nil || balance != 50 {
		t.Fatalf("balance=%d err=%v", balance, err)
	}
}

func TestPlanCatalogAndPurchaseRejectUnavailableNodeStatuses(t *testing.T) {
	s := subscriptionTestStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	owner, _ := register(t, s, "unavailable-node-sale@example.com")
	offering, err := s.AdminCreateOffering(ctx, admin, LineOfferingInput{Code: "unavailable-node-sale", Name: "Unavailable node sale", NodeID: "hk-01"})
	if err != nil {
		t.Fatal(err)
	}
	offering, err = s.AdminSetOfferingState(ctx, admin, offering.ID, OfferingStateInput{ExpectedRevision: offering.Revision, State: OfferingStatePublished})
	if err != nil {
		t.Fatal(err)
	}
	input := configuredPlan()
	input.Code = "unavailable-node-sale"
	input.OfferingID = offering.ID
	plan, err := s.AdminCreatePlan(ctx, admin, input)
	if err != nil {
		t.Fatal(err)
	}
	rule, err := s.CreateRule(ctx, owner, "unavailable-node-sale-rule", ruleInput("tcp"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: owner.TenantID, Key: "unavailable-node-sale-funds", AmountCents: 500, Reason: "test", Operator: "test"}); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"maintenance", "draining", "retired"} {
		t.Run(status, func(t *testing.T) {
			if _, updateErr := s.pool.Exec(ctx, "UPDATE nodes SET status=$1 WHERE id='hk-01'", status); updateErr != nil {
				t.Fatal(updateErr)
			}
			catalog, listErr := s.ListPlans(ctx, owner, rule.ID, 1)
			if listErr != nil || catalog.Total != 0 || len(catalog.Items) != 0 {
				t.Fatalf("catalog for %s node=%+v err=%v", status, catalog, listErr)
			}
			if _, purchaseErr := s.PurchasePlan(ctx, owner, "unavailable-node-"+status, PurchasePlanInput{RuleID: rule.ID, PriceVersionID: plan.ID}); !errors.Is(purchaseErr, ErrNodeNotAllocatable) {
				t.Fatalf("purchase for %s node: %v", status, purchaseErr)
			}
			var balance, periods int64
			if queryErr := s.pool.QueryRow(ctx, "SELECT balance_cents FROM billing_wallets WHERE tenant_id=$1", owner.TenantID).Scan(&balance); queryErr != nil {
				t.Fatal(queryErr)
			}
			if queryErr := s.pool.QueryRow(ctx, "SELECT count(*) FROM rule_subscriptions WHERE tenant_id=$1", owner.TenantID).Scan(&periods); queryErr != nil {
				t.Fatal(queryErr)
			}
			if balance != 500 || periods != 0 {
				t.Fatalf("%s failed purchase mutated balance=%d periods=%d", status, balance, periods)
			}
		})
	}
}

func TestPlanOfferingBindsCatalogAndPurchaseToExactNode(t *testing.T) {
	s := subscriptionTestStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	owner, _ := register(t, s, "offering-catalog@example.com")
	if err := s.AdminAddNode(ctx, admin, NodeInput{ID: "sg-catalog", Name: "Singapore", Region: "Singapore", ListenAddress: "0.0.0.0", CapacityMbps: 1000, ResourceMode: ResourceModeShared}); err != nil {
		t.Fatal(err)
	}
	hkOffering := publishedOffering(t, s, admin, "hk-catalog", "hk-01")
	sgOffering := publishedOffering(t, s, admin, "sg-catalog", "sg-catalog")
	hkRule, err := s.CreateRule(ctx, owner, "offering-hk-rule", ruleInput("tcp"))
	if err != nil {
		t.Fatal(err)
	}
	sgInput := ruleInput("udp")
	sgInput.NodeID = "sg-catalog"
	if _, err = s.CreateRule(ctx, owner, "offering-sg-rule", sgInput); err != nil {
		t.Fatal(err)
	}

	hkInput := configuredPlan()
	hkInput.Code = "hk-bound"
	hkInput.OfferingID = hkOffering.ID
	hkPlan, err := s.AdminCreatePlan(ctx, admin, hkInput)
	if err != nil {
		t.Fatal(err)
	}
	if hkPlan.Offering == nil || hkPlan.Offering.ID != hkOffering.ID || hkPlan.Offering.NodeID != "hk-01" || hkPlan.ResourceMode != ResourceModeShared {
		t.Fatalf("bound plan: %+v", hkPlan)
	}
	sgPlanInput := configuredPlan()
	sgPlanInput.Code = "sg-bound"
	sgPlanInput.OfferingID = sgOffering.ID
	sgPlan, err := s.AdminCreatePlan(ctx, admin, sgPlanInput)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdminCreatePlan(ctx, admin, configuredPlan()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unbound plan accepted: %v", err)
	}

	hkPlans, err := s.ListPlans(ctx, owner, hkRule.ID, 1)
	if err != nil || hkPlans.Total != 1 || len(hkPlans.Items) != 1 || hkPlans.Items[0].ID != hkPlan.ID {
		t.Fatalf("Hong Kong catalog: %+v %v", hkPlans, err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO plan_price_versions(id,code,version,name,price_cents,quota_bytes,bandwidth_mbps,period_seconds,resource_mode) VALUES($1,'legacy-unbound',1,'Legacy',1,1,1,60,'shared')`, newID()); err != nil {
		t.Fatal(err)
	}
	adminPlans, err := s.AdminPlans(ctx, admin, 1)
	if err != nil || adminPlans.Total != 3 {
		t.Fatalf("admin plans: %+v %v", adminPlans, err)
	}
	legacyFound := false
	for _, plan := range adminPlans.Items {
		if plan.Code == "legacy-unbound" {
			legacyFound = plan.Offering == nil
		}
	}
	if !legacyFound {
		t.Fatal("legacy unbound plan was not retained for administrators")
	}

	if _, err = s.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: owner.TenantID, Key: "offering-wallet-fund", AmountCents: 500, Reason: "test", Operator: "test"}); err != nil {
		t.Fatal(err)
	}
	var balanceBefore, ledgerBefore, periodsBefore, outboxBefore int64
	if err = s.pool.QueryRow(ctx, "SELECT balance_cents FROM billing_wallets WHERE tenant_id=$1", owner.TenantID).Scan(&balanceBefore); err != nil {
		t.Fatal(err)
	}
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM billing_ledger WHERE tenant_id=$1", owner.TenantID).Scan(&ledgerBefore); err != nil {
		t.Fatal(err)
	}
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM rule_subscriptions WHERE tenant_id=$1", owner.TenantID).Scan(&periodsBefore); err != nil {
		t.Fatal(err)
	}
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM config_outbox").Scan(&outboxBefore); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PurchasePlan(ctx, owner, "wrong-node-plan", PurchasePlanInput{RuleID: hkRule.ID, PriceVersionID: sgPlan.ID}); !errors.Is(err, ErrConflict) {
		t.Fatalf("other-node plan purchase: %v", err)
	}
	var balanceAfter, ledgerAfter, periodsAfter, outboxAfter int64
	if err = s.pool.QueryRow(ctx, "SELECT balance_cents FROM billing_wallets WHERE tenant_id=$1", owner.TenantID).Scan(&balanceAfter); err != nil {
		t.Fatal(err)
	}
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM billing_ledger WHERE tenant_id=$1", owner.TenantID).Scan(&ledgerAfter); err != nil {
		t.Fatal(err)
	}
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM rule_subscriptions WHERE tenant_id=$1", owner.TenantID).Scan(&periodsAfter); err != nil {
		t.Fatal(err)
	}
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM config_outbox").Scan(&outboxAfter); err != nil {
		t.Fatal(err)
	}
	if balanceAfter != balanceBefore || ledgerAfter != ledgerBefore || periodsAfter != periodsBefore || outboxAfter != outboxBefore {
		t.Fatalf("failed purchase mutated state balance %d/%d ledger %d/%d periods %d/%d outbox %d/%d", balanceBefore, balanceAfter, ledgerBefore, ledgerAfter, periodsBefore, periodsAfter, outboxBefore, outboxAfter)
	}
}

func subscriptionFixture(t *testing.T) (*Store, Identity, Identity, Rule, PlanPriceVersion) {
	t.Helper()
	s := subscriptionTestStore(t)
	admin, _ := adminIdentity(t, s)
	customer, _ := register(t, s, "subscriber@example.com")
	rule, err := s.CreateRule(context.Background(), customer, "subscriber-rule", ruleInput("tcp"))
	if err != nil {
		t.Fatal(err)
	}
	offering := publishedOffering(t, s, admin, "fixture-hk", "hk-01")
	input := configuredPlan()
	input.OfferingID = offering.ID
	plan, err := s.AdminCreatePlan(context.Background(), admin, input)
	if err != nil {
		t.Fatal(err)
	}
	return s, admin, customer, rule, plan
}

func TestPurchasePlanCanCreateUnboundEntitlementForLaterRuleCreation(t *testing.T) {
	s := subscriptionTestStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	owner, _ := register(t, s, "package-first@example.com")
	offering := publishedOffering(t, s, admin, "package-first-line", "hk-01")
	input := configuredPlan()
	input.OfferingID = offering.ID
	plan, err := s.AdminCreatePlan(ctx, admin, input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: owner.TenantID, Key: "package-first-funds", AmountCents: 500, Reason: "test", Operator: "test"}); err != nil {
		t.Fatal(err)
	}
	period, err := s.PurchasePlan(ctx, owner, "package-first-purchase", PurchasePlanInput{PriceVersionID: plan.ID})
	if err != nil || period.RuleID != "" {
		t.Fatalf("unbound purchase = %+v err=%v", period, err)
	}
	rule, err := s.CreateRule(ctx, owner, "package-first-rule", RuleInput{
		Name: "套餐先行规则", Protocol: "tcp", TargetHost: "192.0.2.10", TargetPort: 443,
		BandwidthMbps: plan.BandwidthMbps, NodeID: offering.NodeID, SubscriptionID: period.ID,
	})
	if err != nil {
		t.Fatalf("create from package: %v", err)
	}
	if rule.NodeID != offering.NodeID || rule.BandwidthMbps != plan.BandwidthMbps {
		t.Fatalf("rule did not inherit package: %+v", rule)
	}
	bound, err := s.RuleSubscriptions(ctx, owner, rule.ID, 0)
	if err != nil || len(bound) != 1 || bound[0].ID != period.ID || bound[0].RuleID != rule.ID {
		t.Fatalf("bound periods = %+v err=%v", bound, err)
	}
	ruleB, err := s.CreateRule(ctx, owner, "package-first-rule-b", RuleInput{
		Name: "套餐共享规则", Protocol: "tcp", TargetHost: "192.0.2.11", TargetPort: 443,
		BandwidthMbps: plan.BandwidthMbps, NodeID: offering.NodeID, SubscriptionID: period.ID,
	})
	if err != nil {
		t.Fatalf("create second rule from shared package: %v", err)
	}
	if _, err = s.pool.Exec(ctx, "UPDATE subscription_usage SET upload_bytes=60 WHERE subscription_id=$1 AND rule_id=$2", period.ID, rule.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, "UPDATE subscription_usage SET upload_bytes=50 WHERE subscription_id=$1 AND rule_id=$2", period.ID, ruleB.ID); err != nil {
		t.Fatal(err)
	}
	shared, err := s.RuleSubscriptions(ctx, owner, ruleB.ID, 0)
	if err != nil || len(shared) != 1 || shared[0].UsedBytes != 110 || shared[0].RemainingBytes != 0 || shared[0].Status != "quota-exhausted" {
		t.Fatalf("shared package aggregate = %+v err=%v", shared, err)
	}
}

func TestPurchaseWaitsForConcurrentOfferingPauseBeforeWalletDebit(t *testing.T) {
	s, _, owner, rule, plan := subscriptionFixture(t)
	ctx := context.Background()
	if _, err := s.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: owner.TenantID, Key: "pause-race-funds", AmountCents: 500, Reason: "test", Operator: "test"}); err != nil {
		t.Fatal(err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "UPDATE line_offerings SET state='paused',revision=revision+1 WHERE id=$1", plan.OfferingID); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, purchaseErr := s.PurchasePlan(ctx, owner, "pause-race-purchase", PurchasePlanInput{RuleID: rule.ID, PriceVersionID: plan.ID})
		result <- purchaseErr
	}()
	select {
	case purchaseErr := <-result:
		t.Fatalf("purchase did not wait for offering state lock: %v", purchaseErr)
	case <-time.After(300 * time.Millisecond):
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if purchaseErr := <-result; !errors.Is(purchaseErr, ErrConflict) {
		t.Fatalf("purchase after pause: %v", purchaseErr)
	}
	var balance int64
	if err = s.pool.QueryRow(ctx, "SELECT balance_cents FROM billing_wallets WHERE tenant_id=$1", owner.TenantID).Scan(&balance); err != nil || balance != 500 {
		t.Fatalf("balance=%d err=%v", balance, err)
	}
}

func TestPurchaseWaitsForConcurrentNodeMaintenanceBeforeWalletDebit(t *testing.T) {
	s, _, owner, rule, plan := subscriptionFixture(t)
	ctx := context.Background()
	if _, err := s.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: owner.TenantID, Key: "maintenance-race-funds", AmountCents: 500, Reason: "test", Operator: "test"}); err != nil {
		t.Fatal(err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err = lockNode(ctx, tx, rule.NodeID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, "UPDATE nodes SET status='maintenance' WHERE id=$1", rule.NodeID); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, purchaseErr := s.PurchasePlan(ctx, owner, "maintenance-race-purchase", PurchasePlanInput{RuleID: rule.ID, PriceVersionID: plan.ID})
		result <- purchaseErr
	}()
	select {
	case purchaseErr := <-result:
		t.Fatalf("purchase did not wait for node lock: %v", purchaseErr)
	case <-time.After(300 * time.Millisecond):
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if purchaseErr := <-result; !errors.Is(purchaseErr, ErrNodeNotAllocatable) {
		t.Fatalf("purchase after maintenance: %v", purchaseErr)
	}
	var balance int64
	if err = s.pool.QueryRow(ctx, "SELECT balance_cents FROM billing_wallets WHERE tenant_id=$1", owner.TenantID).Scan(&balance); err != nil || balance != 500 {
		t.Fatalf("balance=%d err=%v", balance, err)
	}
}

func TestSubscriptionsConfiguredPlanPermissionsAndImmutability(t *testing.T) {
	s := subscriptionTestStore(t)
	ctx := context.Background()
	owner, _ := register(t, s, "plan-owner@example.com")
	if _, err := s.AdminCreatePlan(ctx, owner, configuredPlan()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("owner created plan: %v", err)
	}
	if catalog, err := s.ListPlans(ctx, owner, "", 1); err != nil || catalog.Total != 0 {
		t.Fatalf("catalog without rule: %+v %v", catalog, err)
	}
	admin, _ := adminIdentity(t, s)
	offering := publishedOffering(t, s, admin, "configured-plans", "hk-01")
	input := configuredPlan()
	input.OfferingID = offering.ID
	first, err := s.AdminCreatePlan(ctx, admin, input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.AdminCreatePlan(ctx, admin, input)
	if err != nil || first.ID != second.ID {
		t.Fatalf("create replay: %+v %v", second, err)
	}
	changed := input
	changed.PriceCents++
	if _, err = s.AdminCreatePlan(ctx, admin, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed version accepted: %v", err)
	}
	changed.Version = 2
	if _, err = s.AdminCreatePlan(ctx, admin, changed); err != nil {
		t.Fatal(err)
	}
	plans, err := s.AdminPlans(ctx, admin, 1)
	if err != nil || plans.Total != 2 {
		t.Fatalf("plan versions: %+v %v", plans, err)
	}
	for _, sql := range []string{"UPDATE plan_price_versions SET price_cents=1", "DELETE FROM plan_price_versions", "TRUNCATE plan_price_versions CASCADE"} {
		if _, err = s.pool.Exec(ctx, sql); err == nil {
			t.Fatalf("mutable plan: %s", sql)
		}
	}
	for _, bad := range []PlanInput{
		{Code: "invalid"},
		{OfferingID: offering.ID, Code: "bad", Version: 1, Name: "bad", PriceCents: 1, QuotaBytes: 0, BandwidthMbps: 1, PeriodSeconds: 1},
		{OfferingID: offering.ID, Code: "bad", Version: 1, Name: "bad", PriceCents: 1, QuotaBytes: 1, BandwidthMbps: 1, PeriodSeconds: 0},
	} {
		if _, err = s.AdminCreatePlan(ctx, admin, bad); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid plan: %v", err)
		}
	}
}

func TestPlanResourceModeValidationReplayAndImmutability(t *testing.T) {
	s := subscriptionTestStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	offering := publishedOffering(t, s, admin, "mode-shared", "hk-01")
	input := configuredPlan()
	input.OfferingID = offering.ID
	input.ResourceMode = ""
	created, err := s.AdminCreatePlan(ctx, admin, input)
	if err != nil || created.ResourceMode != ResourceModeShared {
		t.Fatalf("derived shared mode: %+v %v", created, err)
	}
	replay := input
	replay.Code = "mode-replay"
	created, err = s.AdminCreatePlan(ctx, admin, replay)
	if err != nil {
		t.Fatal(err)
	}
	replay.ResourceMode = ResourceModeDedicatedTenant
	if _, err = s.AdminCreatePlan(ctx, admin, replay); !errors.Is(err, ErrConflict) {
		t.Fatalf("mode-changing replay: %v", err)
	}
	if _, err = s.pool.Exec(ctx, "UPDATE plan_price_versions SET resource_mode='dedicated-rule' WHERE id=$1", created.ID); err == nil {
		t.Fatal("immutable plan resource mode was updated")
	}
	plans, err := s.AdminPlans(ctx, admin, 1)
	if err != nil || plans.Total != 2 {
		t.Fatalf("admin catalog: %+v %v", plans, err)
	}
	for _, plan := range plans.Items {
		if plan.ResourceMode == "" {
			t.Fatalf("catalog omitted mode: %+v", plan)
		}
	}
}

func TestPlanCatalogForRuleFiltersAndAuthorizes(t *testing.T) {
	s := subscriptionTestStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	owner, _ := register(t, s, "catalog-owner@example.com")
	other, _ := register(t, s, "catalog-other@example.com")
	sharedRule, err := s.CreateRule(ctx, owner, "catalog-shared-rule", ruleInput("tcp"))
	if err != nil {
		t.Fatal(err)
	}
	node := NodeInput{ID: "catalog-tenant", Name: "Catalog tenant", Region: "Test", ListenAddress: "0.0.0.0", CapacityMbps: 1000, ResourceMode: ResourceModeDedicatedTenant, DedicatedTenantID: owner.TenantID}
	if err = s.AdminAddNode(ctx, admin, node); err != nil {
		t.Fatal(err)
	}
	dedicatedInput := ruleInput("udp")
	dedicatedInput.NodeID = node.ID
	dedicatedRule, err := s.CreateRule(ctx, owner, "catalog-dedicated-rule", dedicatedInput)
	if err != nil {
		t.Fatal(err)
	}
	sharedOffering := publishedOffering(t, s, admin, "catalog-shared", "hk-01")
	dedicatedOffering := publishedOffering(t, s, admin, "catalog-tenant", node.ID)
	for _, item := range []struct {
		code       string
		offeringID string
	}{{"catalog-shared-plan", sharedOffering.ID}, {"catalog-tenant-plan", dedicatedOffering.ID}} {
		input := configuredPlan()
		input.Code = item.code
		input.OfferingID = item.offeringID
		input.ResourceMode = ""
		if _, err = s.AdminCreatePlan(ctx, admin, input); err != nil {
			t.Fatal(err)
		}
	}
	shared, err := s.ListPlans(ctx, owner, sharedRule.ID, 1)
	if err != nil || shared.Total != 1 || shared.Items[0].ResourceMode != ResourceModeShared {
		t.Fatalf("shared catalog: %+v %v", shared, err)
	}
	dedicated, err := s.ListPlans(ctx, owner, dedicatedRule.ID, 1)
	if err != nil || dedicated.Total != 1 || dedicated.Items[0].ResourceMode != ResourceModeDedicatedTenant {
		t.Fatalf("dedicated catalog: %+v %v", dedicated, err)
	}
	if _, err = s.ListPlans(ctx, other, dedicatedRule.ID, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign rule catalog: %v", err)
	}
}

func TestPurchasePlanResourceModeMismatchIsAtomic(t *testing.T) {
	s, admin, who, rule, _ := subscriptionFixture(t)
	ctx := context.Background()
	if err := s.AdminAddNode(ctx, admin, NodeInput{ID: "forged-dedicated", Name: "Dedicated", Region: "Test", ListenAddress: "0.0.0.0", CapacityMbps: 1000, ResourceMode: ResourceModeDedicatedRule, DedicatedTenantID: who.TenantID}); err != nil {
		t.Fatal(err)
	}
	offering := publishedOffering(t, s, admin, "forged-dedicated", "forged-dedicated")
	input := configuredPlan()
	input.Code = "forged-dedicated-plan"
	input.OfferingID = offering.ID
	input.ResourceMode = ""
	plan, err := s.AdminCreatePlan(ctx, admin, input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: who.TenantID, Key: "mode-mismatch-funds", AmountCents: 500, Reason: "test", Operator: "test"}); err != nil {
		t.Fatal(err)
	}
	var balance, ledgerCount, subscriptionCount, desiredVersion, generation int64
	var status string
	if err = s.pool.QueryRow(ctx, "SELECT balance_cents FROM billing_wallets WHERE tenant_id=$1", who.TenantID).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM billing_ledger WHERE tenant_id=$1 AND kind='subscription-debit'", who.TenantID).Scan(&ledgerCount); err != nil {
		t.Fatal(err)
	}
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM rule_subscriptions WHERE rule_id=$1", rule.ID).Scan(&subscriptionCount); err != nil {
		t.Fatal(err)
	}
	if err = s.pool.QueryRow(ctx, "SELECT status,desired_version FROM relay_rules WHERE id=$1", rule.ID).Scan(&status, &desiredVersion); err != nil {
		t.Fatal(err)
	}
	if err = s.pool.QueryRow(ctx, "SELECT desired_generation FROM nodes WHERE id=$1", rule.NodeID).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PurchasePlan(ctx, who, "mode-mismatch-buy", PurchasePlanInput{RuleID: rule.ID, PriceVersionID: plan.ID}); !errors.Is(err, ErrConflict) {
		t.Fatalf("mismatched purchase: %v", err)
	}
	var afterBalance, afterLedger, afterSubscriptions, afterVersion, afterGeneration int64
	var afterStatus string
	s.pool.QueryRow(ctx, "SELECT balance_cents FROM billing_wallets WHERE tenant_id=$1", who.TenantID).Scan(&afterBalance)
	s.pool.QueryRow(ctx, "SELECT count(*) FROM billing_ledger WHERE tenant_id=$1 AND kind='subscription-debit'", who.TenantID).Scan(&afterLedger)
	s.pool.QueryRow(ctx, "SELECT count(*) FROM rule_subscriptions WHERE rule_id=$1", rule.ID).Scan(&afterSubscriptions)
	s.pool.QueryRow(ctx, "SELECT status,desired_version FROM relay_rules WHERE id=$1", rule.ID).Scan(&afterStatus, &afterVersion)
	s.pool.QueryRow(ctx, "SELECT desired_generation FROM nodes WHERE id=$1", rule.NodeID).Scan(&afterGeneration)
	if afterBalance != balance || afterLedger != ledgerCount || afterSubscriptions != subscriptionCount || afterStatus != status || afterVersion != desiredVersion || afterGeneration != generation {
		t.Fatalf("mismatch mutated state balance=%d/%d ledger=%d/%d subscriptions=%d/%d rule=%s:%d/%s:%d generation=%d/%d", balance, afterBalance, ledgerCount, afterLedger, subscriptionCount, afterSubscriptions, status, desiredVersion, afterStatus, afterVersion, generation, afterGeneration)
	}
}

func TestSubscriptionsConcurrentPurchaseIsOneRuleDebitAndDelivery(t *testing.T) {
	s, _, who, rule, plan := subscriptionFixture(t)
	ctx := context.Background()
	if _, err := s.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: who.TenantID, Key: "subscription-funds", AmountCents: 500, Reason: "local correction", Operator: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReportUsage(ctx, "hk-01", UsageReport{RuleID: rule.ID, Epoch: "subscription-epoch", Sequence: 1, UploadBytes: 20, DownloadBytes: 30}); err != nil {
		t.Fatal(err)
	}
	var generation int64
	if err := s.pool.QueryRow(ctx, "SELECT desired_generation FROM nodes WHERE id='hk-01'").Scan(&generation); err != nil {
		t.Fatal(err)
	}
	input := PurchasePlanInput{RuleID: rule.ID, PriceVersionID: plan.ID}
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan RuleSubscription, 12)
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			p, e := s.PurchasePlan(ctx, who, "purchase-once", input)
			results <- p
			errs <- e
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	var id string
	for p := range results {
		if id == "" {
			id = p.ID
		}
		if p.ID != id || p.BaseUploadBytes != 20 || p.BaseDownloadBytes != 30 || p.UsedBytes != 0 || p.RemainingBytes != 100 || p.Plan.ID != plan.ID {
			t.Fatalf("purchase: %+v", p)
		}
	}
	wallet, err := s.Wallet(ctx, who)
	if err != nil || wallet.BalanceCents != 450 {
		t.Fatalf("wallet: %+v %v", wallet, err)
	}
	ledger, err := s.WalletLedger(ctx, who, rule.ID, 0)
	if err != nil || len(ledger) != 1 || ledger[0].Kind != "subscription-debit" || ledger[0].AmountCents != -50 {
		t.Fatalf("debit: %+v %v", ledger, err)
	}
	var current int64
	var bandwidth int
	if err = s.pool.QueryRow(ctx, "SELECT desired_generation FROM nodes WHERE id='hk-01'").Scan(&current); err != nil || current != generation+1 {
		t.Fatalf("delivery: %d %v", current, err)
	}
	if err = s.pool.QueryRow(ctx, "SELECT bandwidth_mbps FROM relay_rules WHERE id=$1", rule.ID).Scan(&bandwidth); err != nil || bandwidth != 100 {
		t.Fatalf("bandwidth: %d %v", bandwidth, err)
	}
	if _, err = s.PurchasePlan(ctx, who, "early-renewal", input); !errors.Is(err, ErrConflict) {
		t.Fatalf("early renewal: %v", err)
	}
	other, err := s.CreateRule(ctx, who, "other-subscription-rule", ruleInput("udp"))
	if err != nil {
		t.Fatal(err)
	}
	input.RuleID = other.ID
	if _, err = s.PurchasePlan(ctx, who, "purchase-once", input); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting purchase replay: %v", err)
	}
}

func TestSubscriptionsInsufficientIsolationCapacityAndOutboxRollback(t *testing.T) {
	s, _, who, rule, plan := subscriptionFixture(t)
	ctx := context.Background()
	input := PurchasePlanInput{RuleID: rule.ID, PriceVersionID: plan.ID}
	if _, err := s.PurchasePlan(ctx, who, "insufficient-funds", input); !errors.Is(err, ErrInsufficientBalance) || err.Error() != "钱包余额不足，请先充值或联系平台管理员" {
		t.Fatalf("insufficient: %v", err)
	}
	var count int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM rule_subscriptions").Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed purchase persisted: %d %v", count, err)
	}
	if _, err := s.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: who.TenantID, Key: "low-balance-funds", AmountCents: 10, Reason: "correction", Operator: "test"}); err != nil {
		t.Fatal(err)
	}
	var balanceBefore, ledgerBefore, subscriptionsBefore, outboxBefore int64
	var ruleStatusBefore string
	if err := s.pool.QueryRow(ctx, "SELECT balance_cents FROM billing_wallets WHERE tenant_id=$1", who.TenantID).Scan(&balanceBefore); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM billing_ledger WHERE tenant_id=$1", who.TenantID).Scan(&ledgerBefore); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM rule_subscriptions WHERE tenant_id=$1", who.TenantID).Scan(&subscriptionsBefore); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, "SELECT status FROM relay_rules WHERE id=$1", rule.ID).Scan(&ruleStatusBefore); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM config_outbox").Scan(&outboxBefore); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PurchasePlan(ctx, who, "low-balance-purchase", input); !errors.Is(err, ErrInsufficientBalance) || err.Error() != "钱包余额不足，请先充值或联系平台管理员" {
		t.Fatalf("low balance: %v", err)
	}
	var balanceAfter, ledgerAfter, subscriptionsAfter, outboxAfter int64
	var ruleStatusAfter string
	if err := s.pool.QueryRow(ctx, "SELECT balance_cents FROM billing_wallets WHERE tenant_id=$1", who.TenantID).Scan(&balanceAfter); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM billing_ledger WHERE tenant_id=$1", who.TenantID).Scan(&ledgerAfter); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM rule_subscriptions WHERE tenant_id=$1", who.TenantID).Scan(&subscriptionsAfter); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, "SELECT status FROM relay_rules WHERE id=$1", rule.ID).Scan(&ruleStatusAfter); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM config_outbox").Scan(&outboxAfter); err != nil {
		t.Fatal(err)
	}
	if balanceAfter != balanceBefore || ledgerAfter != ledgerBefore || subscriptionsAfter != subscriptionsBefore || ruleStatusAfter != ruleStatusBefore || outboxAfter != outboxBefore {
		t.Fatalf("low-balance purchase mutated state balance=%d/%d ledger=%d/%d subscriptions=%d/%d rule=%s/%s outbox=%d/%d", balanceBefore, balanceAfter, ledgerBefore, ledgerAfter, subscriptionsBefore, subscriptionsAfter, ruleStatusBefore, ruleStatusAfter, outboxBefore, outboxAfter)
	}
	if _, err := s.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: who.TenantID, Key: "low-balance-clear", AmountCents: -10, Reason: "test cleanup", Operator: "test"}); err != nil {
		t.Fatal(err)
	}
	other, _ := register(t, s, "foreign-subscription@example.com")
	if _, err := s.PurchasePlan(ctx, other, "foreign-purchase", input); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign purchase: %v", err)
	}
	if _, err := s.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: who.TenantID, Key: "rollback-funds", AmountCents: 100, Reason: "correction", Operator: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, "UPDATE nodes SET capacity_mbps=50 WHERE id='hk-01'"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PurchasePlan(ctx, who, "capacity-purchase", input); !errors.Is(err, ErrInvalid) {
		t.Fatalf("node capacity: %v", err)
	}
	if _, err := s.pool.Exec(ctx, "UPDATE nodes SET capacity_mbps=1000 WHERE id='hk-01'; ALTER TABLE config_outbox ADD CONSTRAINT subscription_fail CHECK(false) NOT VALID"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PurchasePlan(ctx, who, "outbox-rollback", input); err == nil {
		t.Fatal("outbox failure accepted")
	}
	wallet, err := s.Wallet(ctx, who)
	if err != nil || wallet.BalanceCents != 100 {
		t.Fatalf("partial debit: %+v %v", wallet, err)
	}
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM rule_subscriptions").Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial purchase: %d %v", count, err)
	}
	ledger, err := s.WalletLedger(ctx, who, rule.ID, 0)
	if err != nil || len(ledger) != 0 {
		t.Fatalf("partial ledger: %+v %v", ledger, err)
	}
}

// Parent ReportUsage hooks this function into the same transaction, after totals.
func enforceQuotaTest(t *testing.T, s *Store, ruleID string) {
	t.Helper()
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var node string
	if err = tx.QueryRow(ctx, "SELECT node_id FROM relay_rules WHERE id=$1", ruleID).Scan(&node); err != nil {
		t.Fatal(err)
	}
	if err = lockNode(ctx, tx, node); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, "SELECT id FROM relay_rules WHERE id=$1 FOR UPDATE", ruleID); err != nil {
		t.Fatal(err)
	}
	if err = s.EnforceRuleQuota(ctx, tx, ruleID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSubscriptionsQuotaRenewalKeepsHistoryAndOtherRuleIndependent(t *testing.T) {
	s, admin, who, r1, plan := subscriptionFixture(t)
	ctx := context.Background()
	r2, err := s.CreateRule(ctx, who, "second-quota-rule", ruleInput("udp"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: who.TenantID, Key: "quota-funds", AmountCents: 500, Reason: "correction", Operator: "test"}); err != nil {
		t.Fatal(err)
	}
	p1, err := s.PurchasePlan(ctx, who, "quota-purchase-1", PurchasePlanInput{RuleID: r1.ID, PriceVersionID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	p2, err := s.PurchasePlan(ctx, who, "quota-purchase-2", PurchasePlanInput{RuleID: r2.ID, PriceVersionID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	report := UsageReport{RuleID: r1.ID, SubscriptionID: p1.ID, Epoch: "quota-epoch", Sequence: 1, UploadBytes: 70, DownloadBytes: 40}
	if _, err = s.ReportUsage(ctx, "hk-01", report); err != nil {
		t.Fatal(err)
	}
	enforceQuotaTest(t, s, r1.ID)
	var status string
	var applied int64
	if err = s.pool.QueryRow(ctx, "SELECT status,applied_version FROM relay_rules WHERE id=$1", r1.ID).Scan(&status, &applied); err != nil || status != "paused" || applied != 0 {
		t.Fatalf("quota pause: %s %d %v", status, applied, err)
	}
	if err = s.pool.QueryRow(ctx, "SELECT status FROM relay_rules WHERE id=$1", r2.ID).Scan(&status); err != nil || status != "pending-agent" {
		t.Fatalf("shared quota: %s %v", status, err)
	}
	periods, err := s.RuleSubscriptions(ctx, who, r1.ID, 0)
	if err != nil || len(periods) != 1 || periods[0].UsedBytes != 110 || periods[0].RemainingBytes != 0 || periods[0].Status != "quota-exhausted" {
		t.Fatalf("quota: %+v %v", periods, err)
	}
	newInput := configuredPlan()
	newInput.OfferingID = plan.OfferingID
	newInput.Version = 2
	newInput.BandwidthMbps = 200
	newPlan, err := s.AdminCreatePlan(ctx, admin, newInput)
	if err != nil {
		t.Fatal(err)
	}
	renewed, err := s.PurchasePlan(ctx, who, "quota-renewal-1", PurchasePlanInput{RuleID: r1.ID, PriceVersionID: newPlan.ID})
	if err != nil || renewed.ID == p1.ID || renewed.UsedBytes != 0 {
		t.Fatalf("renewal: %+v %v", renewed, err)
	}
	if _, err = s.PurchasePlan(ctx, who, "quota-purchase-1", PurchasePlanInput{RuleID: r1.ID, PriceVersionID: plan.ID}); err != nil {
		t.Fatal(err)
	}
	var bandwidth int
	if err = s.pool.QueryRow(ctx, "SELECT bandwidth_mbps FROM relay_rules WHERE id=$1", r1.ID).Scan(&bandwidth); err != nil || bandwidth != 200 {
		t.Fatalf("stale replay overwrote new bandwidth: %d %v", bandwidth, err)
	}
	report.Sequence = 2
	report.UploadBytes = 75
	if _, err = s.ReportUsage(ctx, "hk-01", report); err != nil {
		t.Fatal(err)
	}
	periods, err = s.RuleSubscriptions(ctx, who, r1.ID, 0)
	if err != nil || len(periods) != 2 || periods[0].UsedBytes != 0 || periods[1].UsedBytes != 115 || !periods[1].Closed {
		t.Fatalf("period preservation: %+v %v", periods, err)
	}
	if _, err = s.ReportUsage(ctx, "hk-01", UsageReport{RuleID: r1.ID, SubscriptionID: renewed.ID, Epoch: "renewed-quota-epoch", Sequence: 1, UploadBytes: 5}); err != nil {
		t.Fatal(err)
	}
	periods, err = s.RuleSubscriptions(ctx, who, r1.ID, 0)
	if err != nil || periods[0].UsedBytes != 5 || periods[1].UsedBytes != 115 {
		t.Fatalf("new epoch attribution: %+v %v", periods, err)
	}
	otherPeriods, err := s.RuleSubscriptions(ctx, who, r2.ID, 0)
	if err != nil || len(otherPeriods) != 1 || otherPeriods[0].ID != p2.ID || otherPeriods[0].UsedBytes != 0 || otherPeriods[0].RemainingBytes != plan.QuotaBytes || otherPeriods[0].Status != "active" {
		t.Fatalf("other rule entitlement changed: %+v %v", otherPeriods, err)
	}
}

func TestSubscriptionsResumeGuardReadsLatestPeriod(t *testing.T) {
	s, _, who, rule, plan := subscriptionFixture(t)
	ctx := context.Background()
	check := func(want error) {
		t.Helper()
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if err = lockNode(ctx, tx, "hk-01"); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, "SELECT id FROM relay_rules WHERE id=$1 FOR UPDATE", rule.ID); err != nil {
			t.Fatal(err)
		}
		err = CheckRuleResumeAllowed(ctx, tx, rule.ID)
		if !errors.Is(err, want) {
			t.Fatalf("resume guard: %v want %v", err, want)
		}
	}
	check(nil)
	if _, err := s.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: who.TenantID, Key: "resume-funds", AmountCents: 200, Reason: "correction", Operator: "test"}); err != nil {
		t.Fatal(err)
	}
	p, err := s.PurchasePlan(ctx, who, "resume-purchase", PurchasePlanInput{RuleID: rule.ID, PriceVersionID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	check(nil)
	if _, err = s.pool.Exec(ctx, "UPDATE rule_subscriptions SET starts_at=starts_at-interval '2 hours',ends_at=ends_at-interval '2 hours' WHERE id=$1", p.ID); err != nil {
		t.Fatal(err)
	}
	check(ErrConflict)
	if _, err = s.ResumeRule(ctx, who, rule.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("expired rule resumed through integrated hook: %v", err)
	}
	renewed, err := s.PurchasePlan(ctx, who, "resume-renewal", PurchasePlanInput{RuleID: rule.ID, PriceVersionID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	check(nil)
	if _, err = s.ReportUsage(ctx, "hk-01", UsageReport{RuleID: rule.ID, SubscriptionID: renewed.ID, Epoch: "resume-epoch", Sequence: 1, UploadBytes: 100}); err != nil {
		t.Fatal(err)
	}
	check(ErrConflict)
	if _, err = s.ResumeRule(ctx, who, rule.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("exhausted rule resumed through integrated hook: %v", err)
	}
}

func TestSubscriptionsExpiryWorkerRechecksAndPauses(t *testing.T) {
	s, admin, who, rule, existingPlan := subscriptionFixture(t)
	ctx := context.Background()
	input := configuredPlan()
	input.OfferingID = existingPlan.OfferingID
	input.Code = "short-period"
	input.PeriodSeconds = 3600
	plan, err := s.AdminCreatePlan(ctx, admin, input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: who.TenantID, Key: "expiry-funds", AmountCents: 100, Reason: "correction", Operator: "test"}); err != nil {
		t.Fatal(err)
	}
	period, err := s.PurchasePlan(ctx, who, "expiring-purchase", PurchasePlanInput{RuleID: rule.ID, PriceVersionID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	if delta := period.EndsAt.Sub(period.StartsAt); delta != time.Hour {
		t.Fatalf("configured duration: %v", delta)
	}
	if n, err := s.EnforceExpiredSubscriptions(ctx, 10); err != nil || n != 0 {
		t.Fatalf("premature expiry: %d %v", n, err)
	}
	// Expiry is persisted data; move this isolated fixture into the past without
	// sleeping or changing any shared clock/production code.
	if _, err = s.pool.Exec(ctx, "UPDATE rule_subscriptions SET starts_at=starts_at-interval '2 hours',ends_at=ends_at-interval '2 hours' WHERE id=$1", period.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := s.EnforceExpiredSubscriptions(ctx, 10); err != nil || n != 1 {
		t.Fatalf("expiry: %d %v", n, err)
	}
	if n, err := s.EnforceExpiredSubscriptions(ctx, 10); err != nil || n != 0 {
		t.Fatalf("repeat expiry: %d %v", n, err)
	}
}

func TestSubscriptionsPurchaseWithLongMemberEmail(t *testing.T) {
	s, _, _, _, plan := subscriptionFixture(t)
	ctx := context.Background()
	who, _, err := s.Register(ctx, "Long email", strings.Repeat("x", 115)+"@example.com", "long-test-password-42")
	if err != nil {
		t.Fatal(err)
	}
	rule, err := s.CreateRule(ctx, who, "long-email-rule", ruleInput("tcp"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: who.TenantID, Key: "long-email-funds", AmountCents: 50, Reason: "correction", Operator: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PurchasePlan(ctx, who, "long-email-purchase", PurchasePlanInput{RuleID: rule.ID, PriceVersionID: plan.ID}); err != nil {
		t.Fatal(err)
	}
}

func TestSubscriptionsCompetingRulesCannotOverdrawSharedMoney(t *testing.T) {
	s, _, who, r1, plan := subscriptionFixture(t)
	ctx := context.Background()
	if err := s.AddNode(ctx, "sg-02", "Second node", "Singapore", "0.0.0.0"); err != nil {
		t.Fatal(err)
	}
	input := ruleInput("udp")
	input.NodeID = "sg-02"
	r2, err := s.CreateRule(ctx, who, "concurrent-money-rule", input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: who.TenantID, Key: "competing-funds", AmountCents: 50, Reason: "correction", Operator: "test"}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, r := range []Rule{r1, r2} {
		wg.Add(1)
		go func(rule Rule) {
			defer wg.Done()
			<-start
			_, e := s.PurchasePlan(ctx, who, "competing-"+rule.ID, PurchasePlanInput{RuleID: rule.ID, PriceVersionID: plan.ID})
			errs <- e
		}(r)
	}
	close(start)
	wg.Wait()
	close(errs)
	success, conflicts := 0, 0
	for e := range errs {
		if e == nil {
			success++
		} else if errors.Is(e, ErrConflict) {
			conflicts++
		} else {
			t.Fatal(e)
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatalf("purchases=%d conflicts=%d", success, conflicts)
	}
	wallet, err := s.Wallet(ctx, who)
	if err != nil || wallet.BalanceCents != 0 {
		t.Fatalf("overdraw: %+v %v", wallet, err)
	}
	var count int
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM billing_ledger WHERE kind='subscription-debit'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate charge: %d %v", count, err)
	}
}

func TestSubscriptionsPurchaseCompatibleWithWalletCorrectionForeignKey(t *testing.T) {
	s, _, who, rule, plan := subscriptionFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: who.TenantID, Key: "lock-order-funds", AmountCents: 100, Reason: "correction", Operator: "test"}); err != nil {
		t.Fatal(err)
	}
	// A local correction locks its wallet before its ledger FK takes KEY SHARE
	// on the rule. Hold the first lock and observe purchase waiting on that wallet.
	correction, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer correction.Rollback(context.Background())
	var balance int64
	var pid int
	if err = correction.QueryRow(ctx, "SELECT balance_cents FROM billing_wallets WHERE tenant_id=$1 FOR UPDATE", who.TenantID).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	if err = correction.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, e := s.PurchasePlan(ctx, who, "lock-order-purchase", PurchasePlanInput{RuleID: rule.ID, PriceVersionID: plan.ID})
		result <- e
	}()
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
			t.Fatalf("purchase didn't lock wallet: %v", e)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
	fkCtx, cancelFK := context.WithTimeout(ctx, time.Second)
	defer cancelFK()
	var id string
	if err = correction.QueryRow(fkCtx, "SELECT id::text FROM relay_rules WHERE id=$1 FOR KEY SHARE", rule.ID).Scan(&id); err != nil {
		t.Fatalf("wallet-to-ledger FK cannot complete while purchase waits: %v", err)
	}
	if err = correction.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-result; err != nil {
		t.Fatal(err)
	}
}

func TestSubscriptionsLateBufferedUsageStaysWithExplicitClosedPeriod(t *testing.T) {
	s, _, who, rule, plan := subscriptionFixture(t)
	ctx := context.Background()
	if _, err := s.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: who.TenantID, Key: "period-attribution-funds", AmountCents: 500, Reason: "correction", Operator: "test"}); err != nil {
		t.Fatal(err)
	}
	old, err := s.PurchasePlan(ctx, who, "old-period-purchase", PurchasePlanInput{RuleID: rule.ID, PriceVersionID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	report := UsageReport{RuleID: rule.ID, SubscriptionID: old.ID, Epoch: "old-bound-epoch", Sequence: 1, UploadBytes: 60, DownloadBytes: 20}
	if _, err = s.ReportUsage(ctx, "hk-01", report); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, "UPDATE rule_subscriptions SET starts_at=starts_at-interval '2 hours',ends_at=ends_at-interval '2 hours' WHERE id=$1", old.ID); err != nil {
		t.Fatal(err)
	}
	renewed, err := s.PurchasePlan(ctx, who, "new-period-purchase", PurchasePlanInput{RuleID: rule.ID, PriceVersionID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	current := UsageReport{RuleID: rule.ID, SubscriptionID: renewed.ID, Epoch: "new-bound-epoch", Sequence: 1, UploadBytes: 10, DownloadBytes: 5}
	if _, err = s.ReportUsage(ctx, "hk-01", current); err != nil {
		t.Fatal(err)
	}
	// The active epoch now belongs to the new period. Concurrent old-WAL retries
	// still add exactly one old-period delta, never consuming current quota.
	report.Sequence = 2
	report.UploadBytes = 100
	report.DownloadBytes = 70
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	start := make(chan struct{})
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; _, e := s.ReportUsage(ctx, "hk-01", report); errs <- e }()
	}
	close(start)
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	periods, err := s.RuleSubscriptions(ctx, who, rule.ID, 0)
	if err != nil || len(periods) != 2 || periods[0].ID != renewed.ID || periods[0].UsedBytes != 15 || periods[0].RemainingBytes != 85 || periods[1].UsedBytes != 170 || !periods[1].Closed {
		t.Fatalf("late report crossed periods: %+v %v", periods, err)
	}
	usage, err := s.RuleUsage(ctx, who, rule.ID)
	if err != nil || usage.TotalBytes != 185 {
		t.Fatalf("rule lifetime usage: %+v %v", usage, err)
	}
	if n, err := s.EnforceExpiredSubscriptions(ctx, 100); err != nil || n != 0 {
		t.Fatalf("old usage exhausted new period: %d %v", n, err)
	}
	var status, activeEpoch string
	if err = s.pool.QueryRow(ctx, "SELECT r.status,u.active_epoch FROM relay_rules r JOIN billing_usage_totals u ON u.rule_id=r.id WHERE r.id=$1", rule.ID).Scan(&status, &activeEpoch); err != nil || status != "pending-agent" || activeEpoch != current.Epoch {
		t.Fatalf("late report changed current delivery: %s %s %v", status, activeEpoch, err)
	}
	changed := report
	changed.SubscriptionID = renewed.ID
	if _, err = s.ReportUsage(ctx, "hk-01", changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("replay reassigned period: %v", err)
	}
	changed.Sequence = 3
	if _, err = s.ReportUsage(ctx, "hk-01", changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("epoch reassigned period: %v", err)
	}
	changed = report
	changed.SubscriptionID = ""
	if _, err = s.ReportUsage(ctx, "hk-01", changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("implicit old replay: %v", err)
	}
	changed.Epoch = "missing-period-epoch"
	if _, err = s.ReportUsage(ctx, "hk-01", changed); !errors.Is(err, ErrInvalid) {
		t.Fatalf("implicit new epoch: %v", err)
	}
	// SQL also protects the epoch binding, beyond application validation.
	if _, err = s.pool.Exec(ctx, "UPDATE billing_usage_epochs SET subscription_id=$1 WHERE rule_id=$2 AND epoch=$3", renewed.ID, rule.ID, report.Epoch); err == nil {
		t.Fatal("SQL allowed epoch rebinding")
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO billing_usage_reports(rule_id,tenant_id,node_id,epoch,sequence,upload_bytes,download_bytes,upload_delta_bytes,download_delta_bytes,subscription_id) VALUES($1,$2,'hk-01',$3,3,100,70,0,0,$4)`, rule.ID, who.TenantID, report.Epoch, renewed.ID); err == nil {
		t.Fatal("SQL allowed report/epoch period mismatch")
	}
	otherRule, err := s.CreateRule(ctx, who, "other-period-rule", ruleInput("udp"))
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := s.PurchasePlan(ctx, who, "foreign-period-purchase", PurchasePlanInput{RuleID: otherRule.ID, PriceVersionID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	changed = current
	changed.SubscriptionID = foreign.ID
	changed.Epoch = "foreign-period-epoch"
	if _, err = s.ReportUsage(ctx, "hk-01", changed); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign rule period accepted: %v", err)
	}
	if _, err = s.ReportUsage(ctx, "sg-02", current); !errors.Is(err, ErrForbidden) {
		t.Fatalf("foreign node period accepted: %v", err)
	}
}

func TestSubscriptionsPeriodUsageAndQuotaPauseRollBackTogether(t *testing.T) {
	s, _, who, rule, plan := subscriptionFixture(t)
	ctx := context.Background()
	if _, err := s.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: who.TenantID, Key: "period-rollback-funds", AmountCents: 100, Reason: "correction", Operator: "test"}); err != nil {
		t.Fatal(err)
	}
	period, err := s.PurchasePlan(ctx, who, "period-rollback-purchase", PurchasePlanInput{RuleID: rule.ID, PriceVersionID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, "ALTER TABLE config_outbox ADD CONSTRAINT period_quota_failure CHECK(false) NOT VALID"); err != nil {
		t.Fatal(err)
	}
	input := UsageReport{RuleID: rule.ID, SubscriptionID: period.ID, Epoch: "atomic-period-epoch", Sequence: 1, UploadBytes: 100}
	if _, err = s.ReportUsage(ctx, "hk-01", input); err == nil {
		t.Fatal("quota enqueue failure committed usage")
	}
	periods, err := s.RuleSubscriptions(ctx, who, rule.ID, 0)
	if err != nil || periods[0].UsedBytes != 0 || periods[0].EnforcementRequested {
		t.Fatalf("period partial commit: %+v %v", periods, err)
	}
	usage, err := s.RuleUsage(ctx, who, rule.ID)
	if err != nil || usage.HasReports {
		t.Fatalf("rule partial commit: %+v %v", usage, err)
	}
	if _, err = s.pool.Exec(ctx, "ALTER TABLE config_outbox DROP CONSTRAINT period_quota_failure"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReportUsage(ctx, "hk-01", input); err != nil {
		t.Fatal(err)
	}
	periods, err = s.RuleSubscriptions(ctx, who, rule.ID, 0)
	if err != nil || periods[0].UsedBytes != 100 || periods[0].Status != "quota-exhausted" {
		t.Fatalf("retry: %+v %v", periods, err)
	}
}

func TestSubscriptionsMigration10DoesNotGuessHistoricalPeriod(t *testing.T) {
	s := subscriptionTestStore(t)
	ctx := context.Background()
	sql, err := os.ReadFile("migration10.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, consumed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unpaid-history-preserved", true: "ambiguous-paid-history-refused"}[consumed], func(t *testing.T) {
			// A separate transactional namespace models v9 data without downgrading the
			// current test schema or editing any existing migration.
			tx, e := s.pool.Begin(ctx)
			if e != nil {
				t.Fatal(e)
			}
			defer tx.Rollback(ctx)
			schema := "period_upgrade_" + strings.ReplaceAll(newID(), "-", "")
			if _, e = tx.Exec(ctx, "CREATE SCHEMA "+schema); e != nil {
				t.Fatal(e)
			}
			if _, e = tx.Exec(ctx, "SET LOCAL search_path TO "+schema); e != nil {
				t.Fatal(e)
			}
			for _, source := range []string{schemaSQL, migration2SQL, migration3SQL, migration4SQL, migration5SQL, migration6SQL, migration7SQL, migration8SQL, migration9SQL} {
				if _, e = tx.Exec(ctx, source); e != nil {
					t.Fatal(e)
				}
			}
			tenant, rule, plan, period := newID(), newID(), newID(), newID()
			if _, e = tx.Exec(ctx, "INSERT INTO tenants(id,name) VALUES($1,'upgrade')", tenant); e != nil {
				t.Fatal(e)
			}
			if _, e = tx.Exec(ctx, `INSERT INTO relay_rules(id,tenant_id,node_id,name,protocol,target_host,target_port,bandwidth_mbps,listen_port,status) VALUES($1,$2,'hk-01','upgrade','tcp','192.0.2.1',443,100,20000,'pending-agent')`, rule, tenant); e != nil {
				t.Fatal(e)
			}
			if _, e = tx.Exec(ctx, "INSERT INTO plan_price_versions(id,code,version,name,price_cents,quota_bytes,bandwidth_mbps,period_seconds) VALUES($1,'upgrade',1,'upgrade',1,100,100,3600)", plan); e != nil {
				t.Fatal(e)
			}
			if _, e = tx.Exec(ctx, "INSERT INTO billing_wallets(tenant_id,balance_cents) VALUES($1,0)", tenant); e != nil {
				t.Fatal(e)
			}
			var ledger int64
			if e = tx.QueryRow(ctx, `INSERT INTO billing_ledger(tenant_id,rule_id,kind,amount_cents,balance_after_cents,idempotency_key,reason,operator_name,price_version_id) VALUES($1,$2,'subscription-debit',-1,0,'upgrade-purchase','upgrade','operator',$3) RETURNING id`, tenant, rule, plan).Scan(&ledger); e != nil {
				t.Fatal(e)
			}
			baseline := int64(10)
			if consumed {
				baseline = 0
			}
			if _, e = tx.Exec(ctx, `INSERT INTO rule_subscriptions(id,tenant_id,rule_id,price_version_id,ledger_id,idempotency_key,starts_at,ends_at,base_upload_bytes,base_download_bytes) VALUES($1,$2,$3,$4,$5,'upgrade-period',now(),now()+interval '1 hour',$6,0)`, period, tenant, rule, plan, ledger, baseline); e != nil {
				t.Fatal(e)
			}
			if _, e = tx.Exec(ctx, `INSERT INTO billing_usage_totals(rule_id,tenant_id,node_id,active_epoch,upload_bytes,download_bytes) VALUES($1,$2,'hk-01','upgrade-epoch',10,0);`, rule, tenant); e != nil {
				t.Fatal(e)
			}
			if _, e = tx.Exec(ctx, `INSERT INTO billing_usage_epochs(rule_id,tenant_id,node_id,epoch,last_sequence,upload_bytes,download_bytes) VALUES($1,$2,'hk-01','upgrade-epoch',1,10,0)`, rule, tenant); e != nil {
				t.Fatal(e)
			}
			if _, e = tx.Exec(ctx, `INSERT INTO billing_usage_reports(rule_id,tenant_id,node_id,epoch,sequence,upload_bytes,download_bytes,upload_delta_bytes,download_delta_bytes) VALUES($1,$2,'hk-01','upgrade-epoch',1,10,0,10,0)`, rule, tenant); e != nil {
				t.Fatal(e)
			}
			_, e = tx.Exec(ctx, string(sql))
			if consumed {
				if e == nil || !strings.Contains(e.Error(), "unattributed paid usage") {
					t.Fatalf("ambiguous history not refused: %v", e)
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			var up int64
			var unbound bool
			if e = tx.QueryRow(ctx, "SELECT upload_bytes FROM subscription_usage WHERE subscription_id=$1", period).Scan(&up); e != nil || up != 0 {
				t.Fatalf("guessed paid usage: %d %v", up, e)
			}
			if e = tx.QueryRow(ctx, "SELECT subscription_id IS NULL FROM billing_usage_reports WHERE rule_id=$1", rule).Scan(&unbound); e != nil || !unbound {
				t.Fatalf("old report reassigned: %v %v", unbound, e)
			}
		})
	}
}
