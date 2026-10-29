package platform

import (
	"context"
	"encoding/json"
	"testing"
)

func TestUnpaidRuleCannotEnterActiveNodeConfiguration(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	who, _ := register(t, s, "entitlement@example.com")
	r, err := s.CreateRule(ctx, who, "unpaid-rule-key", ruleInput("tcp"))
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.ClaimConfig(ctx, "hk-01", "entitlement-test")
	if err != nil || job == nil {
		t.Fatalf("claim: %v", err)
	}
	var cfg NodeConfig
	if err = json.Unmarshal(job.Payload, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rules) != 1 || cfg.Rules[0].ID != r.ID || cfg.Rules[0].Status != "paused" {
		t.Fatalf("unpaid config forwarded: %+v", cfg)
	}
}

func TestPaidSnapshotCarriesPeriodAndExhaustedOrRecreatedRuleIsPaused(t *testing.T) {
	s, _, who, rule, plan := subscriptionFixture(t)
	ctx := context.Background()
	if _, err := s.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: who.TenantID, Key: "entitlement-credit", AmountCents: 100, Reason: "test correction", Operator: "test"}); err != nil {
		t.Fatal(err)
	}
	period, err := s.PurchasePlan(ctx, who, "entitlement-purchase", PurchasePlanInput{RuleID: rule.ID, PriceVersionID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	read := func() NodeConfig {
		t.Helper()
		job, e := s.ClaimConfig(ctx, "hk-01", "entitlement-test")
		if e != nil || job == nil {
			t.Fatalf("snapshot claim: %v", e)
		}
		var cfg NodeConfig
		if e = json.Unmarshal(job.Payload, &cfg); e != nil {
			t.Fatal(e)
		}
		return cfg
	}
	cfg := read()
	if len(cfg.Rules) != 1 || cfg.Rules[0].Status != "pending-agent" || cfg.Rules[0].SubscriptionID != period.ID {
		t.Fatalf("paid period missing: %+v", cfg)
	}
	if _, err = s.ReportUsage(ctx, "hk-01", UsageReport{RuleID: rule.ID, SubscriptionID: period.ID, Epoch: "entitlement-epoch", Sequence: 1, UploadBytes: plan.QuotaBytes}); err != nil {
		t.Fatal(err)
	}
	cfg = read()
	if cfg.Rules[0].Status != "paused" {
		t.Fatal("exhausted rule still active in snapshot")
	}
	if err = s.DeleteRule(ctx, who, rule.ID); err != nil {
		t.Fatal(err)
	}
	newRule, err := s.CreateRule(ctx, who, "recreate-unpaid-rule", ruleInput("tcp"))
	if err != nil {
		t.Fatal(err)
	}
	cfg = read()
	if len(cfg.Rules) != 1 || cfg.Rules[0].ID != newRule.ID || cfg.Rules[0].Status != "paused" || cfg.Rules[0].SubscriptionID != "" {
		t.Fatalf("recreation bypassed payment: %+v", cfg)
	}
}

func TestUpgradeReissuesSnapshotWithKernelEntitlementMetadata(t *testing.T) {
	s, _, who, r, plan := subscriptionFixture(t)
	ctx := context.Background()
	if _, e := s.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: who.TenantID, Key: "upgrade-entitlement-credit", AmountCents: 100, Reason: "isolated fixture", Operator: "test"}); e != nil {
		t.Fatal(e)
	}
	if _, e := s.PurchasePlan(ctx, who, "upgrade-entitlement-buy", PurchasePlanInput{RuleID: r.ID, PriceVersionID: plan.ID}); e != nil {
		t.Fatal(e)
	}
	j, e := s.ClaimConfig(ctx, "hk-01", "upgrade-validator")
	if e != nil || j == nil {
		t.Fatal(e)
	}
	if e = s.ReportConfig(ctx, "hk-01", j.ID, ConfigReport{LeaseToken: j.LeaseToken, Checksum: j.Checksum, Outcome: "validated"}); e != nil {
		t.Fatal(e)
	}
	if _, e = s.pool.Exec(ctx, "DELETE FROM schema_migrations WHERE version=17"); e != nil {
		t.Fatal(e)
	}
	if e = s.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	next, e := s.ClaimConfig(ctx, "hk-01", "upgrade-validator")
	if e != nil || next == nil || next.Generation <= j.Generation {
		t.Fatalf("upgrade didn't reissue safe metadata: %+v %v", next, e)
	}
	var cfg NodeConfig
	if e = json.Unmarshal(next.Payload, &cfg); e != nil {
		t.Fatal(e)
	}
	if len(cfg.Rules) != 1 || cfg.Rules[0].QuotaBytes != plan.QuotaBytes || cfg.Rules[0].ExpiresAt == nil || cfg.Rules[0].SubscriptionID == "" {
		t.Fatal("kernel entitlement metadata missing")
	}
}
