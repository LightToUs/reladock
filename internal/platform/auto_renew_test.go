package platform

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestAutoRenewSharedQuotaAtomicAndReplay(t *testing.T) {
	s, _, who, a, plan := subscriptionFixture(t)
	ctx := context.Background()
	_, err := s.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: who.TenantID, Key: "auto-renew-funds", AmountCents: 500, Reason: "test", Operator: "test"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.PurchasePlan(ctx, who, "auto-renew-purchase", PurchasePlanInput{RuleID: a.ID, PriceVersionID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	input := ruleInput("udp")
	input.SubscriptionID = p.ID
	input.BandwidthMbps = plan.BandwidthMbps
	b, err := s.CreateRule(ctx, who, "auto-renew-rule-b", input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetSubscriptionAutoRenew(ctx, who, p.ID, true); err != nil {
		t.Fatal(err)
	}
	reports := []UsageReport{{RuleID: a.ID, SubscriptionID: p.ID, Epoch: "auto-renew-a", Sequence: 1, UploadBytes: 60}, {RuleID: b.ID, SubscriptionID: p.ID, Epoch: "auto-renew-b", Sequence: 1, DownloadBytes: 50}}
	var wg sync.WaitGroup
	for _, r := range reports {
		wg.Add(1)
		go func(r UsageReport) {
			defer wg.Done()
			if _, e := s.ReportUsage(ctx, a.NodeID, r); e != nil {
				t.Error(e)
			}
		}(r)
	}
	wg.Wait()
	periods, err := s.RuleSubscriptions(ctx, who, a.ID, 0)
	if err != nil || len(periods) != 2 {
		t.Fatalf("periods=%+v err=%v", periods, err)
	}
	next := periods[0]
	if !next.AutoRenew || next.UsedBytes != 0 || next.Status != "active" || !periods[1].Closed || periods[1].UsedBytes != 110 {
		t.Fatalf("periods=%+v", periods)
	}
	other, err := s.RuleSubscriptions(ctx, who, b.ID, 0)
	if err != nil || len(other) != 2 || other[0].ID != next.ID {
		t.Fatalf("other=%+v err=%v", other, err)
	}
	for _, r := range reports {
		if ack, e := s.ReportUsage(ctx, a.NodeID, r); e != nil || !ack.Duplicate {
			t.Fatalf("replay=%+v err=%v", ack, e)
		}
	}
	late := reports[0]
	late.Sequence = 2
	late.UploadBytes = 80
	if _, err = s.ReportUsage(ctx, a.NodeID, late); err != nil {
		t.Fatal(err)
	}
	wallet, err := s.Wallet(ctx, who)
	if err != nil || wallet.BalanceCents != 500-2*plan.PriceCents {
		t.Fatalf("wallet=%+v err=%v", wallet, err)
	}
	periods, err = s.RuleSubscriptions(ctx, who, a.ID, 0)
	if err != nil || periods[0].UsedBytes != 0 || periods[1].UsedBytes != 130 {
		t.Fatalf("late=%+v err=%v", periods, err)
	}
	if _, err = s.SetSubscriptionAutoRenew(ctx, who, next.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReportUsage(ctx, a.NodeID, UsageReport{RuleID: a.ID, SubscriptionID: next.ID, Epoch: "auto-renew-next", Sequence: 1, UploadBytes: 101}); err != nil {
		t.Fatal(err)
	}
	wallet, _ = s.Wallet(ctx, who)
	if wallet.BalanceCents != 500-2*plan.PriceCents {
		t.Fatal("disabled renewal charged")
	}
}

func TestAutoRenewRefusalsPreserveUsageAndWallet(t *testing.T) {
	for _, reason := range []string{"insufficient-balance", "wallet-frozen", "unavailable", "disabled"} {
		t.Run(reason, func(t *testing.T) {
			s, _, who, r, plan := subscriptionFixture(t)
			ctx := context.Background()
			funds := int64(500)
			if reason == "insufficient-balance" {
				funds = plan.PriceCents
			}
			if _, err := s.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: who.TenantID, Key: "refusal-funds", AmountCents: funds, Reason: "test", Operator: "test"}); err != nil {
				t.Fatal(err)
			}
			p, err := s.PurchasePlan(ctx, who, "refusal-purchase", PurchasePlanInput{RuleID: r.ID, PriceVersionID: plan.ID})
			if err != nil {
				t.Fatal(err)
			}
			if reason != "disabled" {
				if _, err = s.SetSubscriptionAutoRenew(ctx, who, p.ID, true); err != nil {
					t.Fatal(err)
				}
			}
			if reason == "wallet-frozen" {
				_, err = s.pool.Exec(ctx, "UPDATE billing_wallets SET spending_frozen=true WHERE tenant_id=$1", who.TenantID)
			}
			if reason == "unavailable" {
				_, err = s.pool.Exec(ctx, "UPDATE line_offerings SET state='paused' WHERE id=$1", plan.OfferingID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.ReportUsage(ctx, r.NodeID, UsageReport{RuleID: r.ID, SubscriptionID: p.ID, Epoch: "refusal-epoch", Sequence: 1, UploadBytes: 110}); err != nil {
				t.Fatal(err)
			}
			periods, err := s.RuleSubscriptions(ctx, who, r.ID, 0)
			if err != nil || len(periods) != 1 || periods[0].UsedBytes != 110 {
				t.Fatalf("periods=%+v err=%v", periods, err)
			}
			if reason != "disabled" && periods[0].AutoRenewError != reason {
				t.Fatalf("reason=%s", periods[0].AutoRenewError)
			}
			wallet, _ := s.Wallet(ctx, who)
			if wallet.BalanceCents != funds-plan.PriceCents {
				t.Fatal("refusal charged")
			}
			var status string
			if err = s.pool.QueryRow(ctx, "SELECT status FROM relay_rules WHERE id=$1", r.ID).Scan(&status); err != nil || status != "paused" {
				t.Fatalf("status=%s err=%v", status, err)
			}
		})
	}
}

func TestAutoRenewSettingTenantIsolation(t *testing.T) {
	s, _, who, r, plan := subscriptionFixture(t)
	ctx := context.Background()
	if _, err := s.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: who.TenantID, Key: "setting-funds", AmountCents: 500, Reason: "test", Operator: "test"}); err != nil {
		t.Fatal(err)
	}
	p, err := s.PurchasePlan(ctx, who, "setting-purchase", PurchasePlanInput{RuleID: r.ID, PriceVersionID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	other, _ := register(t, s, "auto-other@example.com")
	if _, err = s.SetSubscriptionAutoRenew(ctx, other, p.ID, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross tenant: %v", err)
	}
	if _, err = s.pool.Exec(ctx, "UPDATE members SET role='viewer' WHERE id=$1", who.MemberID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetSubscriptionAutoRenew(ctx, who, p.ID, true); !errors.Is(err, ErrForbidden) {
		t.Fatalf("viewer: %v", err)
	}
}

func TestAutoRenewAuditFailureRollsBackDebitAndUsage(t *testing.T) {
	s, _, who, r, plan := subscriptionFixture(t)
	ctx := context.Background()
	if _, err := s.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: who.TenantID, Key: "rollback-auto-funds", AmountCents: 500, Reason: "test", Operator: "test"}); err != nil {
		t.Fatal(err)
	}
	p, err := s.PurchasePlan(ctx, who, "rollback-auto-buy", PurchasePlanInput{RuleID: r.ID, PriceVersionID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetSubscriptionAutoRenew(ctx, who, p.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `CREATE FUNCTION reject_auto_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='subscription.auto-renew.purchase' THEN RAISE EXCEPTION 'test audit failure'; END IF; RETURN NEW; END; $$; CREATE TRIGGER reject_auto_audit BEFORE INSERT ON platform_audit_logs FOR EACH ROW EXECUTE FUNCTION reject_auto_audit()`); err != nil {
		t.Fatal(err)
	}
	report := UsageReport{RuleID: r.ID, SubscriptionID: p.ID, Epoch: "rollback-auto-epoch", Sequence: 1, UploadBytes: 110}
	if _, err = s.ReportUsage(ctx, r.NodeID, report); err == nil {
		t.Fatal("expected rollback")
	}
	wallet, _ := s.Wallet(ctx, who)
	periods, e := s.RuleSubscriptions(ctx, who, r.ID, 0)
	if e != nil || wallet.BalanceCents != 500-plan.PriceCents || len(periods) != 1 || periods[0].UsedBytes != 0 {
		t.Fatalf("partial commit: %+v %+v %v", wallet, periods, e)
	}
	if _, err = s.pool.Exec(ctx, "DROP TRIGGER reject_auto_audit ON platform_audit_logs"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReportUsage(ctx, r.NodeID, report); err != nil {
		t.Fatal(err)
	}
	wallet, _ = s.Wallet(ctx, who)
	if wallet.BalanceCents != 500-2*plan.PriceCents {
		t.Fatal("retry debit mismatch")
	}
}

func TestAutoRenewPreservesManualPauseAndDoesNotRenewTimeExpiry(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "manual-pause", true: "time-expiry"}[expired], func(t *testing.T) {
			s, _, who, r, plan := subscriptionFixture(t)
			ctx := context.Background()
			if _, err := s.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: who.TenantID, Key: "pause-auto-funds", AmountCents: 500, Reason: "test", Operator: "test"}); err != nil {
				t.Fatal(err)
			}
			p, err := s.PurchasePlan(ctx, who, "pause-auto-buy", PurchasePlanInput{RuleID: r.ID, PriceVersionID: plan.ID})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.SetSubscriptionAutoRenew(ctx, who, p.ID, true); err != nil {
				t.Fatal(err)
			}
			if _, err = s.pool.Exec(ctx, "UPDATE relay_rules SET status='paused' WHERE id=$1", r.ID); err != nil {
				t.Fatal(err)
			}
			if expired {
				if _, err = s.pool.Exec(ctx, "UPDATE rule_subscriptions SET starts_at=starts_at-interval '2 hours',ends_at=ends_at-interval '2 hours' WHERE id=$1", p.ID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = s.ReportUsage(ctx, r.NodeID, UsageReport{RuleID: r.ID, SubscriptionID: p.ID, Epoch: "pause-auto-epoch", Sequence: 1, UploadBytes: 110}); err != nil {
				t.Fatal(err)
			}
			var status string
			if err = s.pool.QueryRow(ctx, "SELECT status FROM relay_rules WHERE id=$1", r.ID).Scan(&status); err != nil || status != "paused" {
				t.Fatalf("manual pause changed: %s %v", status, err)
			}
			wallet, _ := s.Wallet(ctx, who)
			want := int64(500) - 2*plan.PriceCents
			if expired {
				want = 500 - plan.PriceCents
			}
			if wallet.BalanceCents != want {
				t.Fatalf("balance=%d want=%d", wallet.BalanceCents, want)
			}
		})
	}
}
