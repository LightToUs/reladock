package platform

import (
	"context"
	"errors"
	"testing"
)

func TestFrozenWalletCannotPurchaseRulePlan(t *testing.T) {
	s, _, who, rule, plan := subscriptionFixture(t)
	ctx := context.Background()
	if _, err := s.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: who.TenantID, Key: "hold-test-credit", AmountCents: 500, Reason: "test adjustment", Operator: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, "UPDATE billing_wallets SET spending_frozen=true WHERE tenant_id=$1", who.TenantID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PurchasePlan(ctx, who, "hold-purchase-key", PurchasePlanInput{RuleID: rule.ID, PriceVersionID: plan.ID}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("frozen wallet purchase: %v", err)
	}
	wallet, err := s.Wallet(ctx, who)
	if err != nil || wallet.BalanceCents != 500 || !wallet.SpendingFrozen {
		t.Fatalf("frozen balance changed: %+v %v", wallet, err)
	}
}
