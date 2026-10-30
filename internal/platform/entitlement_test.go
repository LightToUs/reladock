package platform

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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

func TestDevelopmentBypassDeliversUnsubscribedRuleWithoutQuotaOrExpiry(t *testing.T) {
	s := testStore(t)
	s.allowUnentitledRules = true
	ctx := context.Background()
	who, _ := register(t, s, "development-bypass@example.com")
	r, err := s.CreateRule(ctx, who, "development-bypass-rule", ruleInput("tcp"))
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.ClaimConfig(ctx, "hk-01", "development-bypass-test")
	if err != nil || job == nil {
		t.Fatalf("claim: %v", err)
	}
	var cfg NodeConfig
	if err = json.Unmarshal(job.Payload, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rules) != 1 || cfg.Rules[0].ID != r.ID || cfg.Rules[0].Status != "pending-agent" || !cfg.Rules[0].DevelopmentBypass {
		t.Fatalf("development rule was not enabled explicitly: %+v", cfg)
	}
	if cfg.Rules[0].SubscriptionID != "" || cfg.Rules[0].ExpiresAt != nil || cfg.Rules[0].QuotaBytes != 0 {
		t.Fatalf("development bypass fabricated a paid entitlement: %+v", cfg.Rules[0])
	}
	if _, err = s.PauseRule(ctx, who, r.ID); err != nil {
		t.Fatal(err)
	}
	job, err = s.ClaimConfig(ctx, "hk-01", "development-bypass-test")
	if err != nil || job == nil {
		t.Fatalf("paused claim: %v", err)
	}
	cfg = NodeConfig{}
	if err = json.Unmarshal(job.Payload, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Rules[0].Status != "paused" || cfg.Rules[0].DevelopmentBypass {
		t.Fatalf("development bypass overrode manual pause: %+v", cfg.Rules[0])
	}
}

func TestDevelopmentBypassDoesNotBindUsageToExistingPlan(t *testing.T) {
	s, _, who, rule, plan := subscriptionFixture(t)
	s.allowUnentitledRules = true
	ctx := context.Background()
	if _, err := s.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: who.TenantID, Key: "development-existing-credit", AmountCents: 100, Reason: "development fixture", Operator: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PurchasePlan(ctx, who, "development-existing-purchase", PurchasePlanInput{RuleID: rule.ID, PriceVersionID: plan.ID}); err != nil {
		t.Fatal(err)
	}
	job, err := s.ClaimConfig(ctx, "hk-01", "development-existing-test")
	if err != nil || job == nil {
		t.Fatalf("claim: %v", err)
	}
	var cfg NodeConfig
	if err = json.Unmarshal(job.Payload, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rules) != 1 || !cfg.Rules[0].DevelopmentBypass || cfg.Rules[0].SubscriptionID != "" {
		t.Fatalf("development usage stayed bound to paid plan: %+v", cfg)
	}
	s.allowUnentitledRules = false
	developmentUsage := UsageReport{RuleID: rule.ID, Epoch: "development-unbound-epoch", Sequence: 1, UploadBytes: 10, DevelopmentBypass: true, ConfigGeneration: cfg.Generation}
	if _, err = s.pool.Exec(ctx, "UPDATE config_outbox SET checksum=$2 WHERE node_id='hk-01' AND generation=$1", cfg.Generation, strings.Repeat("0", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReportUsage(ctx, "hk-01", developmentUsage); !errors.Is(err, ErrForbidden) {
		t.Fatalf("development usage trusted a corrupted historical snapshot: %v", err)
	}
	if _, err = s.pool.Exec(ctx, "UPDATE config_outbox SET checksum=$2 WHERE node_id='hk-01' AND generation=$1", cfg.Generation, job.Checksum); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReportUsage(ctx, "hk-01", developmentUsage); err != nil {
		t.Fatalf("authorized development WAL could not drain after bypass disabled: %v", err)
	}
	developmentUsage.ConfigGeneration++
	developmentUsage.Epoch = "forged-development-epoch"
	if _, err = s.ReportUsage(ctx, "hk-01", developmentUsage); err == nil {
		t.Fatal("usage claimed an unauthorized development generation")
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
	// Restore this test's isolated schema to v16, including newer additive tables.
	if _, e = s.pool.Exec(ctx, "DELETE FROM schema_migrations WHERE version>=17; DROP TABLE node_bootstrap,node_preflight"); e != nil {
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
