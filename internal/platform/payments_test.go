package platform

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"reladock/internal/payments"
)

type paymentFake struct{}

func (paymentFake) CreateCheckout(_ context.Context, r payments.CheckoutRequest) (payments.Session, error) {
	return payments.Session{ID: "cs_" + r.OrderID, URL: "https://checkout.stripe.com/c/pay/test", Currency: "usd", AmountTotal: r.AmountCents, Metadata: map[string]string{"order_id": r.OrderID, "tenant_id": r.TenantID, "amount_cents": fmt.Sprint(r.AmountCents), "currency": "usd"}}, nil
}

type paymentModeFake struct{ live bool }

func (f paymentModeFake) LiveMode() bool { return f.live }
func (f paymentModeFake) CreateCheckout(_ context.Context, r payments.CheckoutRequest) (payments.Session, error) {
	mode := "test"
	if f.live {
		mode = "live"
	}
	return payments.Session{ID: "cs_" + mode + "_" + r.OrderID, URL: "https://checkout.stripe.com/c/pay/" + mode, Currency: "usd", AmountTotal: r.AmountCents, Metadata: map[string]string{"order_id": r.OrderID, "tenant_id": r.TenantID, "amount_cents": fmt.Sprint(r.AmountCents), "currency": "usd"}}, nil
}
func paymentTestStore(t *testing.T) *Store {
	t.Helper()
	s := billingTestStore(t)
	for _, migration := range []struct{ table, file string }{{"plan_price_versions", "migration7.sql"}, {"payment_orders", "migration9.sql"}} {
		var exists bool
		if e := s.pool.QueryRow(context.Background(), "SELECT to_regclass($1) IS NOT NULL", migration.table).Scan(&exists); e != nil {
			t.Fatal(e)
		}
		if !exists {
			raw, e := os.ReadFile(migration.file)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = s.pool.Exec(context.Background(), string(raw)); e != nil {
				t.Fatal(e)
			}
		}
	}
	if _, e := s.pool.Exec(context.Background(), PaymentFreezeMigrationSQL); e != nil {
		t.Fatal(e)
	}
	return s
}
func paymentEvent(t *testing.T, id, kind string, obj map[string]any) payments.VerifiedEvent {
	t.Helper()
	raw, e := json.Marshal(map[string]any{"id": id, "type": kind, "data": map[string]any{"object": obj}})
	if e != nil {
		t.Fatal(e)
	}
	stamp := time.Now().Unix()
	h := hmac.New(sha256.New, []byte("test-webhook-secret"))
	fmt.Fprintf(h, "%d.", stamp)
	h.Write(raw)
	v, e := payments.VerifyEvent(raw, fmt.Sprintf("t=%d,v1=%x", stamp, h.Sum(nil)), "test-webhook-secret", time.Now())
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func paidObject(order PaymentOrder) map[string]any {
	return map[string]any{"id": order.SessionID, "object": "checkout.session", "mode": "payment", "status": "complete", "payment_status": "paid", "payment_intent": "pi_" + order.ID, "currency": "usd", "amount_total": order.AmountCents, "metadata": map[string]string{"order_id": order.ID, "tenant_id": order.TenantID, "amount_cents": fmt.Sprint(order.AmountCents), "currency": "usd"}}
}
func paymentOrder(t *testing.T, s *Store, who Identity, key string) PaymentOrder {
	t.Helper()
	o, e := s.CheckoutPayment(context.Background(), who, key, 500, paymentFake{})
	if e != nil {
		t.Fatal(e)
	}
	return o
}
func paymentBalance(t *testing.T, s *Store, who Identity, want int64, entries int) {
	t.Helper()
	w, e := s.Wallet(context.Background(), who)
	if e != nil || w.BalanceCents != want {
		t.Fatalf("balance %+v %v want %d", w, e, want)
	}
	ledger, e := s.WalletLedger(context.Background(), who, "", 0)
	if e != nil || len(ledger) != entries {
		t.Fatalf("ledger count=%d %v want %d", len(ledger), e, entries)
	}
}

func TestCheckoutRejectsPersistedSessionFromDifferentStripeMode(t *testing.T) {
	s := paymentTestStore(t)
	who, _ := register(t, s, "payment-mode-switch@example.com")
	order, err := s.CheckoutPayment(context.Background(), who, "payment-mode-switch", 500, paymentModeFake{})
	if err != nil || !strings.HasPrefix(order.SessionID, "cs_test_") {
		t.Fatalf("test checkout order=%+v err=%v", order, err)
	}
	if _, err = s.CheckoutPayment(context.Background(), who, "payment-mode-switch", 500, paymentModeFake{live: true}); !errors.Is(err, ErrConflict) {
		t.Fatalf("persisted test checkout reused by live provider: %v", err)
	}
}

func planPaymentFixture(t *testing.T, priceCents int64) (*Store, Identity, Identity, PlanPriceVersion) {
	t.Helper()
	s := subscriptionTestStore(t)
	admin, _ := adminIdentity(t, s)
	customer, _ := register(t, s, "plan-payment@example.com")
	offering := publishedOffering(t, s, admin, "plan-payment-line", "hk-01")
	input := configuredPlan()
	input.Code = "plan-payment"
	input.OfferingID = offering.ID
	input.PriceCents = priceCents
	plan, err := s.AdminCreatePlan(context.Background(), admin, input)
	if err != nil {
		t.Fatal(err)
	}
	return s, admin, customer, plan
}

func TestPaymentMigration35PreservesTopupsAndConstrainsPlanOrders(t *testing.T) {
	s, _, who, plan := planPaymentFixture(t, 1999)
	ctx := context.Background()
	if SchemaVersion < 35 {
		t.Fatalf("SchemaVersion=%d want at least 35", SchemaVersion)
	}
	topup, err := s.CheckoutPayment(ctx, who, "migration35-topup", 500, paymentFake{})
	if err != nil {
		t.Fatal(err)
	}
	if topup.Purpose != "wallet-topup" || topup.PurchaseState != "none" || topup.PriceVersionID != "" || topup.SubscriptionID != "" {
		t.Fatalf("topup backfill/model = %+v", topup)
	}

	firstID := newID()
	if _, err = s.pool.Exec(ctx, `INSERT INTO payment_orders(id,tenant_id,created_by,idempotency_key,amount_cents,purpose,price_version_id,purchase_state) VALUES($1,$2,$3,'migration35-plan-a',1499,'plan-purchase',$4,'pending')`, firstID, who.TenantID, who.MemberID, plan.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO payment_orders(id,tenant_id,created_by,idempotency_key,amount_cents,purpose,price_version_id,purchase_state) VALUES($1,$2,$3,'migration35-plan-b',1499,'plan-purchase',$4,'pending')`, newID(), who.TenantID, who.MemberID, plan.ID); err == nil {
		t.Fatal("duplicate pending unbound plan selection was accepted")
	}
	for name, statement := range map[string]string{
		"plan missing price": `INSERT INTO payment_orders(id,tenant_id,created_by,idempotency_key,amount_cents,purpose,purchase_state) VALUES('00000000-0000-0000-0000-000000000101','` + who.TenantID + `','` + who.MemberID + `','migration35-invalid-a',500,'plan-purchase','pending')`,
		"topup with price":   `INSERT INTO payment_orders(id,tenant_id,created_by,idempotency_key,amount_cents,purpose,price_version_id,purchase_state) VALUES('00000000-0000-0000-0000-000000000102','` + who.TenantID + `','` + who.MemberID + `','migration35-invalid-b',500,'wallet-topup','` + plan.ID + `','none')`,
	} {
		if _, err = s.pool.Exec(ctx, statement); err == nil {
			t.Fatalf("%s constraint was not enforced", name)
		}
	}
}

func TestPlanPaymentCheckoutDerivesShortfallRecoversSelectionAndScopesReads(t *testing.T) {
	s, _, who, plan := planPaymentFixture(t, 1999)
	ctx := context.Background()
	if _, err := s.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: who.TenantID, Key: "plan-payment-credit", AmountCents: 500, Reason: "test", Operator: "test"}); err != nil {
		t.Fatal(err)
	}
	input := PurchasePlanInput{PriceVersionID: plan.ID}
	first, err := s.CheckoutPlanPayment(ctx, who, "plan-payment-checkout-a", input, paymentFake{})
	if err != nil {
		t.Fatal(err)
	}
	if first.AmountCents != 1499 || first.Purpose != "plan-purchase" || first.PriceVersionID != plan.ID || first.RuleID != "" || first.PurchaseState != "pending" || first.CheckoutURL == "" {
		t.Fatalf("plan checkout = %+v", first)
	}
	replay, err := s.CheckoutPlanPayment(ctx, who, "plan-payment-checkout-a", input, paymentFake{})
	if err != nil || replay.ID != first.ID {
		t.Fatalf("same-key replay = %+v err=%v", replay, err)
	}
	recovered, err := s.CheckoutPlanPayment(ctx, who, "plan-payment-checkout-b", input, paymentFake{})
	if err != nil || recovered.ID != first.ID {
		t.Fatalf("same-selection recovery = %+v err=%v", recovered, err)
	}

	exact, err := s.PaymentOrder(ctx, who, first.ID)
	if err != nil || exact.ID != first.ID {
		t.Fatalf("exact order = %+v err=%v", exact, err)
	}
	recent, err := s.PlanPaymentOrders(ctx, who)
	if err != nil || len(recent) != 1 || recent[0].ID != first.ID {
		t.Fatalf("recent orders = %+v err=%v", recent, err)
	}
	other, _ := register(t, s, "plan-payment-other@example.com")
	if _, err = s.PaymentOrder(ctx, other, first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign exact order err=%v", err)
	}
}

func TestPlanPaymentCheckoutUsesStripeMinimumAndRejectsWalletOnlyOrFrozen(t *testing.T) {
	ctx := context.Background()
	s, _, who, plan := planPaymentFixture(t, 1999)
	if _, err := s.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: who.TenantID, Key: "plan-payment-near-full", AmountCents: 1998, Reason: "test", Operator: "test"}); err != nil {
		t.Fatal(err)
	}
	order, err := s.CheckoutPlanPayment(ctx, who, "plan-payment-minimum", PurchasePlanInput{PriceVersionID: plan.ID}, paymentFake{})
	if err != nil || order.AmountCents != 50 {
		t.Fatalf("minimum checkout = %+v err=%v", order, err)
	}

	sufficientStore, _, sufficient, sufficientPlan := planPaymentFixture(t, 1999)
	if _, err = sufficientStore.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: sufficient.TenantID, Key: "plan-payment-sufficient", AmountCents: 1999, Reason: "test", Operator: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err = sufficientStore.CheckoutPlanPayment(ctx, sufficient, "plan-payment-wallet-only", PurchasePlanInput{PriceVersionID: sufficientPlan.ID}, paymentFake{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("sufficient wallet checkout err=%v", err)
	}
	if _, err = sufficientStore.pool.Exec(ctx, "UPDATE billing_wallets SET spending_frozen=true WHERE tenant_id=$1", sufficient.TenantID); err != nil {
		t.Fatal(err)
	}
	if _, err = sufficientStore.CheckoutPlanPayment(ctx, sufficient, "plan-payment-frozen", PurchasePlanInput{PriceVersionID: sufficientPlan.ID}, paymentFake{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("frozen wallet checkout err=%v", err)
	}
}

func TestPlanPaymentUnboundDedicatedAdmissionIsRecheckedAtQuoteAndCallback(t *testing.T) {
	ctx := context.Background()
	s := subscriptionTestStore(t)
	admin, _ := adminIdentity(t, s)
	owner, _ := register(t, s, "plan-dedicated-owner@example.com")
	other, _ := register(t, s, "plan-dedicated-other@example.com")
	if err := s.AdminAddNode(ctx, admin, NodeInput{ID: "plan-dedicated", Name: "Dedicated", Region: "Test", ListenAddress: "0.0.0.0", CapacityMbps: 1000, ResourceMode: ResourceModeDedicatedTenant, DedicatedTenantID: owner.TenantID}); err != nil {
		t.Fatal(err)
	}
	offering := publishedOffering(t, s, admin, "plan-dedicated", "plan-dedicated")
	input := configuredPlan()
	input.Code = "plan-dedicated"
	input.OfferingID = offering.ID
	input.PriceCents = 1999
	input.ResourceMode = ResourceModeDedicatedTenant
	plan, err := s.AdminCreatePlan(ctx, admin, input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CheckoutPlanPayment(ctx, other, "dedicated-foreign-quote", PurchasePlanInput{PriceVersionID: plan.ID}, paymentFake{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("foreign dedicated quote err=%v", err)
	}
	if _, err = s.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: other.TenantID, Key: "dedicated-foreign-funds", AmountCents: 1999, Reason: "test", Operator: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PurchasePlan(ctx, other, "dedicated-foreign-purchase", PurchasePlanInput{PriceVersionID: plan.ID}); !errors.Is(err, ErrConflict) {
		t.Fatalf("foreign dedicated wallet purchase err=%v", err)
	}

	order, err := s.CheckoutPlanPayment(ctx, owner, "dedicated-owner-quote", PurchasePlanInput{PriceVersionID: plan.ID}, paymentFake{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, "UPDATE nodes SET dedicated_tenant_id=$2 WHERE id=$1", "plan-dedicated", other.TenantID); err != nil {
		t.Fatal(err)
	}
	if err = s.ApplyStripeEvent(ctx, paymentEvent(t, "evt_dedicated_owner_changed", "checkout.session.completed", paidObject(order))); err != nil {
		t.Fatal(err)
	}
	got, err := s.PaymentOrder(ctx, owner, order.ID)
	if err != nil || got.State != "credited" || got.PurchaseState != "failed" || got.PurchaseError != "conflict" || got.SubscriptionID != "" {
		t.Fatalf("owner-changed callback=%+v err=%v", got, err)
	}
	paymentBalance(t, s, owner, 1999, 1)
}

func checkoutPlanPayment(t *testing.T, s *Store, who Identity, plan PlanPriceVersion, key string) PaymentOrder {
	t.Helper()
	order, err := s.CheckoutPlanPayment(context.Background(), who, key, PurchasePlanInput{PriceVersionID: plan.ID}, paymentFake{})
	if err != nil {
		t.Fatal(err)
	}
	return order
}

func TestPlanPaymentPaidCreditsAndPurchasesOnce(t *testing.T) {
	ctx := context.Background()
	s, _, who, plan := planPaymentFixture(t, 1999)
	order := checkoutPlanPayment(t, s, who, plan, "plan-payment-paid")
	event := paymentEvent(t, "evt_plan_payment_paid", "checkout.session.completed", paidObject(order))

	results := make(chan error, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- s.ApplyStripeEvent(ctx, event)
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ApplyStripeEvent(ctx, paymentEvent(t, "evt_plan_payment_paid_again", "checkout.session.async_payment_succeeded", paidObject(order))); err != nil {
		t.Fatal(err)
	}

	got, err := s.PaymentOrder(ctx, who, order.ID)
	if err != nil || got.State != "credited" || got.PurchaseState != "purchased" || got.SubscriptionID == "" || got.PurchaseError != "" {
		t.Fatalf("paid order = %+v err=%v", got, err)
	}
	paymentBalance(t, s, who, 0, 2)
	var subscriptions int
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM rule_subscriptions WHERE tenant_id=$1", who.TenantID).Scan(&subscriptions); err != nil || subscriptions != 1 {
		t.Fatalf("subscriptions=%d err=%v", subscriptions, err)
	}
	rows, err := s.pool.Query(ctx, "SELECT kind,amount_cents,balance_after_cents FROM billing_ledger WHERE tenant_id=$1 ORDER BY id", who.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	want := []struct {
		kind    string
		amount  int64
		balance int64
	}{{"payment-credit", 1999, 1999}, {"subscription-debit", -1999, 0}}
	for i := 0; rows.Next(); i++ {
		if i >= len(want) {
			t.Fatal("unexpected extra ledger row")
		}
		var kind string
		var amount, balance int64
		if err = rows.Scan(&kind, &amount, &balance); err != nil {
			t.Fatal(err)
		}
		if kind != want[i].kind || amount != want[i].amount || balance != want[i].balance {
			t.Fatalf("ledger[%d]=%s/%d/%d want=%+v", i, kind, amount, balance, want[i])
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestPlanPaymentPaidMinimumLeavesExcessInWallet(t *testing.T) {
	ctx := context.Background()
	s, _, who, plan := planPaymentFixture(t, 1999)
	if _, err := s.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: who.TenantID, Key: "plan-minimum-existing", AmountCents: 1998, Reason: "test", Operator: "test"}); err != nil {
		t.Fatal(err)
	}
	order := checkoutPlanPayment(t, s, who, plan, "plan-payment-paid-minimum")
	if order.AmountCents != 50 {
		t.Fatalf("checkout amount=%d", order.AmountCents)
	}
	if err := s.ApplyStripeEvent(ctx, paymentEvent(t, "evt_plan_payment_minimum", "checkout.session.completed", paidObject(order))); err != nil {
		t.Fatal(err)
	}
	got, err := s.PaymentOrder(ctx, who, order.ID)
	if err != nil || got.PurchaseState != "purchased" {
		t.Fatalf("minimum order=%+v err=%v", got, err)
	}
	paymentBalance(t, s, who, 49, 3)
}

func TestPlanPaymentBusinessFailureRetainsVerifiedCredit(t *testing.T) {
	for _, scenario := range []string{"unavailable", "unauthorized", "spent", "frozen"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			s, admin, who, plan := planPaymentFixture(t, 1999)
			if scenario == "spent" {
				if _, err := s.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: who.TenantID, Key: "plan-spend-credit", AmountCents: 500, Reason: "test", Operator: "test"}); err != nil {
					t.Fatal(err)
				}
			}
			order := checkoutPlanPayment(t, s, who, plan, "plan-business-"+scenario)
			switch scenario {
			case "unavailable":
				if _, err := s.AdminSetOfferingState(ctx, admin, plan.OfferingID, OfferingStateInput{ExpectedRevision: 2, State: OfferingStatePaused}); err != nil {
					t.Fatal(err)
				}
			case "unauthorized":
				if _, err := s.pool.Exec(ctx, "UPDATE members SET role='viewer' WHERE id=$1", who.MemberID); err != nil {
					t.Fatal(err)
				}
			case "spent":
				if _, err := s.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: who.TenantID, Key: "plan-spend-before-paid", AmountCents: -500, Reason: "test", Operator: "test"}); err != nil {
					t.Fatal(err)
				}
			case "frozen":
				if _, err := s.pool.Exec(ctx, "INSERT INTO billing_wallets(tenant_id,spending_frozen) VALUES($1,true) ON CONFLICT(tenant_id) DO UPDATE SET spending_frozen=true", who.TenantID); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.ApplyStripeEvent(ctx, paymentEvent(t, "evt_plan_business_"+scenario, "checkout.session.completed", paidObject(order))); err != nil {
				t.Fatal(err)
			}
			if scenario == "unauthorized" {
				if _, err := s.pool.Exec(ctx, "UPDATE members SET role='owner' WHERE id=$1", who.MemberID); err != nil {
					t.Fatal(err)
				}
			}
			got, err := s.PaymentOrder(ctx, who, order.ID)
			wantError := map[string]string{"unavailable": "unavailable", "unauthorized": "unauthorized", "spent": "insufficient-balance", "frozen": "wallet-frozen"}[scenario]
			if err != nil || got.State != "credited" || got.PurchaseState != "failed" || got.PurchaseError != wantError || got.SubscriptionID != "" {
				t.Fatalf("business failure order=%+v err=%v wantError=%s", got, err, wantError)
			}
			var subscriptions int
			if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM rule_subscriptions WHERE tenant_id=$1", who.TenantID).Scan(&subscriptions); err != nil || subscriptions != 0 {
				t.Fatalf("subscriptions=%d err=%v", subscriptions, err)
			}
			wantBalance := order.AmountCents
			if scenario == "spent" {
				wantBalance = 1499
			}
			var balance int64
			if err = s.pool.QueryRow(ctx, "SELECT balance_cents FROM billing_wallets WHERE tenant_id=$1", who.TenantID).Scan(&balance); err != nil || balance != wantBalance {
				t.Fatalf("balance=%d want=%d err=%v", balance, wantBalance, err)
			}
		})
	}
}

func TestPlanPaymentRollbackAndProviderTerminalEvents(t *testing.T) {
	t.Run("audit rollback retries whole paid event", func(t *testing.T) {
		ctx := context.Background()
		s, _, who, plan := planPaymentFixture(t, 1999)
		order := checkoutPlanPayment(t, s, who, plan, "plan-payment-rollback")
		if _, err := s.pool.Exec(ctx, "ALTER TABLE platform_audit_logs ADD CONSTRAINT plan_payment_reject CHECK(action<>'payment.credit') NOT VALID"); err != nil {
			t.Fatal(err)
		}
		event := paymentEvent(t, "evt_plan_payment_rollback", "checkout.session.completed", paidObject(order))
		if err := s.ApplyStripeEvent(ctx, event); err == nil {
			t.Fatal("paid event committed despite forced audit failure")
		}
		paymentBalance(t, s, who, 0, 0)
		var state, purchaseState string
		if err := s.pool.QueryRow(ctx, "SELECT state,purchase_state FROM payment_orders WHERE id=$1", order.ID).Scan(&state, &purchaseState); err != nil || state != "pending" || purchaseState != "pending" {
			t.Fatalf("rolled-back order=%s/%s err=%v", state, purchaseState, err)
		}
		if _, err := s.pool.Exec(ctx, "ALTER TABLE platform_audit_logs DROP CONSTRAINT plan_payment_reject"); err != nil {
			t.Fatal(err)
		}
		if err := s.ApplyStripeEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
		got, err := s.PaymentOrder(ctx, who, order.ID)
		if err != nil || got.PurchaseState != "purchased" {
			t.Fatalf("retried order=%+v err=%v", got, err)
		}
	})

	for _, kind := range []string{"checkout.session.expired", "checkout.session.async_payment_failed"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			s, _, who, plan := planPaymentFixture(t, 1999)
			order := checkoutPlanPayment(t, s, who, plan, "plan-terminal-"+kind)
			obj := paidObject(order)
			obj["status"] = "expired"
			obj["payment_status"] = "unpaid"
			if err := s.ApplyStripeEvent(ctx, paymentEvent(t, "evt_"+kind, kind, obj)); err != nil {
				t.Fatal(err)
			}
			got, err := s.PaymentOrder(ctx, who, order.ID)
			wantError := "payment-failed"
			if kind == "checkout.session.expired" {
				wantError = "payment-expired"
			}
			if err != nil || got.State != map[string]string{"payment-expired": "expired", "payment-failed": "failed"}[wantError] || got.PurchaseState != "failed" || got.PurchaseError != wantError {
				t.Fatalf("terminal order=%+v err=%v", got, err)
			}
			paymentBalance(t, s, who, 0, 0)
			replacement := checkoutPlanPayment(t, s, who, plan, "plan-terminal-retry-"+kind)
			if replacement.ID == order.ID {
				t.Fatal("terminal order kept pending selection guard")
			}
		})
	}
}

func TestPaymentsCheckoutValidationTenantAndIdempotency(t *testing.T) {
	s := paymentTestStore(t)
	ctx := context.Background()
	a, _ := register(t, s, "payment-a@example.com")
	b, _ := register(t, s, "payment-b@example.com")
	for _, amount := range []int64{-1, 0, 49, 1000001} {
		if _, e := s.CheckoutPayment(ctx, a, "payment-key", amount, paymentFake{}); !errors.Is(e, ErrInvalid) {
			t.Fatalf("amount %d: %v", amount, e)
		}
	}
	first := paymentOrder(t, s, a, "payment-key")
	again := paymentOrder(t, s, a, "payment-key")
	if first.ID != again.ID {
		t.Fatal("checkout replay made another order")
	}
	if _, e := s.CheckoutPayment(ctx, a, "payment-key", 501, paymentFake{}); !errors.Is(e, ErrConflict) {
		t.Fatalf("changed amount: %v", e)
	}
	other := paymentOrder(t, s, b, "payment-key")
	if first.ID == other.ID {
		t.Fatal("cross-tenant order")
	}
	forged := a
	forged.MemberID = b.MemberID
	if _, e := s.CheckoutPayment(ctx, forged, "forged-key", 500, paymentFake{}); !errors.Is(e, ErrUnauthorized) {
		t.Fatalf("forged identity %v", e)
	}
	paymentBalance(t, s, a, 0, 0)
}
func TestPaymentsPaidConcurrentReplayAndConflict(t *testing.T) {
	s := paymentTestStore(t)
	ctx := context.Background()
	who, _ := register(t, s, "paid@example.com")
	order := paymentOrder(t, s, who, "paid-key-1")
	event := paymentEvent(t, "evt_paid", "checkout.session.completed", paidObject(order))
	results := make(chan error, 16)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- s.ApplyStripeEvent(ctx, event) }()
	}
	wg.Wait()
	close(results)
	for e := range results {
		if e != nil {
			t.Fatal(e)
		}
	}
	paymentBalance(t, s, who, 500, 1)
	// Stripe may emit distinct event IDs for the same session; still one credit.
	if e := s.ApplyStripeEvent(ctx, paymentEvent(t, "evt_paid_other", "checkout.session.completed", paidObject(order))); e != nil {
		t.Fatal(e)
	}
	changed := paidObject(order)
	changed["amount_total"] = 501
	if e := s.ApplyStripeEvent(ctx, paymentEvent(t, "evt_paid", "checkout.session.completed", changed)); !errors.Is(e, ErrConflict) {
		t.Fatalf("conflicting event identity: %v", e)
	}
	paymentBalance(t, s, who, 500, 1)
	var n int
	if e := s.pool.QueryRow(ctx, "SELECT count(*) FROM platform_audit_logs WHERE action='payment.credit'").Scan(&n); e != nil || n != 1 {
		t.Fatalf("audit count %d %v", n, e)
	}
}
func TestPaymentsRejectMismatchesAndAuditRollback(t *testing.T) {
	s := paymentTestStore(t)
	ctx := context.Background()
	who, _ := register(t, s, "mismatch@example.com")
	for _, field := range []string{"amount_total", "currency", "id", "object", "tenant_id", "order_id", "amount_cents", "metadata_currency", "status", "payment_status", "mode", "payment_intent"} {
		// Every case starts pending: an earlier review state must not mask a
		// missing validation check in a later case.
		order := paymentOrder(t, s, who, "mismatch-"+field)
		obj := paidObject(order)
		switch field {
		case "amount_total":
			obj[field] = 501
		case "currency":
			obj[field] = "eur"
		case "tenant_id", "order_id", "amount_cents":
			obj["metadata"].(map[string]string)[field] = newID()
		case "metadata_currency":
			obj["metadata"].(map[string]string)["currency"] = "eur"
		case "payment_intent":
			obj[field] = ""
		default:
			obj[field] = "wrong"
		}
		if e := s.ApplyStripeEvent(ctx, paymentEvent(t, "evt_bad_"+field, "checkout.session.completed", obj)); e != nil {
			t.Fatal(e)
		}
		paymentBalance(t, s, who, 0, 0)
	}
	paymentBalance(t, s, who, 0, 0)
	// A different order proves audit failure rolls back event, credit, and wallet.
	order := paymentOrder(t, s, who, "rollback-key")
	if _, e := s.pool.Exec(ctx, "ALTER TABLE platform_audit_logs ADD CONSTRAINT payment_reject CHECK(action<>'payment.credit') NOT VALID"); e != nil {
		t.Fatal(e)
	}
	event := paymentEvent(t, "evt_rollback", "checkout.session.completed", paidObject(order))
	if e := s.ApplyStripeEvent(ctx, event); e == nil {
		t.Fatal("accepted failed audit")
	}
	paymentBalance(t, s, who, 0, 0)
	if _, e := s.pool.Exec(ctx, "ALTER TABLE platform_audit_logs DROP CONSTRAINT payment_reject"); e != nil {
		t.Fatal(e)
	}
	if e := s.ApplyStripeEvent(ctx, event); e != nil {
		t.Fatal(e)
	}
	paymentBalance(t, s, who, 500, 1)
}
func refundObject(order PaymentOrder) map[string]any {
	return map[string]any{"id": "ch_" + order.ID, "object": "charge", "payment_intent": "pi_" + order.ID, "amount": 500, "amount_refunded": 500, "currency": "usd", "refunded": true}
}
func TestPaymentsRefundReversalInsufficientPartialAndReordering(t *testing.T) {
	for _, scenario := range []string{"full", "spent", "partial", "before-credit"} {
		t.Run(scenario, func(t *testing.T) {
			s := paymentTestStore(t)
			ctx := context.Background()
			who, _ := register(t, s, "refund@example.com")
			order := paymentOrder(t, s, who, "refund-key")
			paid := paymentEvent(t, "evt_credit", "checkout.session.completed", paidObject(order))
			if scenario != "before-credit" {
				if e := s.ApplyStripeEvent(ctx, paid); e != nil {
					t.Fatal(e)
				}
			}
			if scenario == "spent" {
				if _, e := s.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: who.TenantID, Key: "spend-key-1", AmountCents: -1, Reason: "test correction", Operator: "test"}); e != nil {
					t.Fatal(e)
				}
			}
			refund := refundObject(order)
			if scenario == "partial" {
				refund["amount_refunded"] = 100
				refund["refunded"] = false
			}
			event := paymentEvent(t, "evt_refund", "charge.refunded", refund)
			var wg sync.WaitGroup
			errs := make(chan error, 8)
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() { defer wg.Done(); errs <- s.ApplyStripeEvent(ctx, event) }()
			}
			wg.Wait()
			close(errs)
			for e := range errs {
				if e != nil {
					t.Fatal(e)
				}
			}
			if e := s.ApplyStripeEvent(ctx, paid); e != nil {
				t.Fatal(e)
			}
			if e := s.ApplyStripeEvent(ctx, paymentEvent(t, "evt_refund_new", "charge.refunded", refund)); e != nil {
				t.Fatal(e)
			}
			state := "refunded"
			want := int64(0)
			entries := 2
			switch scenario {
			case "spent":
				state = "needs-review"
				want = 499
			case "partial":
				state = "needs-review"
				want = 500
				entries = 1
			case "before-credit":
				state = "needs-review"
				entries = 0
			}
			paymentBalance(t, s, who, want, entries)
			var got string
			if e := s.pool.QueryRow(ctx, "SELECT state FROM payment_orders WHERE id=$1", order.ID).Scan(&got); e != nil || got != state {
				t.Fatalf("state=%s want=%s %v", got, state, e)
			}
			var incidents int
			if e := s.pool.QueryRow(ctx, "SELECT count(*) FROM payment_incidents").Scan(&incidents); e != nil {
				t.Fatal(e)
			}
			if state == "needs-review" && incidents < 1 {
				t.Fatal("missing durable incident")
			}
			paymentFrozen(t, s, who, state == "needs-review")
		})
	}
}

func TestPaymentsRefundCompetesWithWalletDebit(t *testing.T) {
	s := paymentTestStore(t)
	ctx := context.Background()
	who, _ := register(t, s, "refund-race@example.com")
	order := paymentOrder(t, s, who, "refund-race")
	if e := s.ApplyStripeEvent(ctx, paymentEvent(t, "evt_race_credit", "checkout.session.completed", paidObject(order))); e != nil {
		t.Fatal(e)
	}
	refund := paymentEvent(t, "evt_race_refund", "charge.refunded", refundObject(order))
	start := make(chan struct{})
	debits := make(chan error, 1)
	refunds := make(chan error, 1)
	go func() {
		<-start
		_, e := s.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: who.TenantID, Key: "competing-debit", AmountCents: -500, Reason: "test correction", Operator: "test"})
		debits <- e
	}()
	go func() { <-start; refunds <- s.ApplyStripeEvent(ctx, refund) }()
	close(start)
	debitErr := <-debits
	if e := <-refunds; e != nil {
		t.Fatal(e)
	}
	if debitErr != nil && !errors.Is(debitErr, ErrConflict) {
		t.Fatal(debitErr)
	}
	paymentBalance(t, s, who, 0, 2)
	var state string
	if e := s.pool.QueryRow(ctx, "SELECT state FROM payment_orders WHERE id=$1", order.ID).Scan(&state); e != nil {
		t.Fatal(e)
	}
	if debitErr == nil && state != "needs-review" || debitErr != nil && state != "refunded" {
		t.Fatalf("debit=%v refund state=%s", debitErr, state)
	}
}

type paymentUncertainProvider struct {
	calls   int
	orderID string
}

func (p *paymentUncertainProvider) CreateCheckout(ctx context.Context, r payments.CheckoutRequest) (payments.Session, error) {
	p.calls++
	if p.orderID != "" && p.orderID != r.OrderID {
		return payments.Session{}, errors.New("unstable order")
	}
	p.orderID = r.OrderID
	if p.calls == 1 {
		return payments.Session{}, context.DeadlineExceeded
	}
	return (paymentFake{}).CreateCheckout(ctx, r)
}
func TestPaymentsUncertainCheckoutReusesOrderAndExpiresSafely(t *testing.T) {
	s := paymentTestStore(t)
	ctx := context.Background()
	who, _ := register(t, s, "uncertain@example.com")
	provider := &paymentUncertainProvider{}
	if _, e := s.CheckoutPayment(ctx, who, "uncertain-key", 500, provider); !errors.Is(e, payments.ErrProvider) {
		t.Fatalf("uncertain response %v", e)
	}
	order, e := s.CheckoutPayment(ctx, who, "uncertain-key", 500, provider)
	if e != nil || order.ID != provider.orderID {
		t.Fatalf("retry %+v %v", order, e)
	}
	var n int
	if e = s.pool.QueryRow(ctx, "SELECT count(*) FROM payment_orders").Scan(&n); e != nil || n != 1 {
		t.Fatalf("order count %d %v", n, e)
	}
	old := &paymentUncertainProvider{}
	if _, e = s.CheckoutPayment(ctx, who, "expired-key-1", 500, old); !errors.Is(e, payments.ErrProvider) {
		t.Fatal(e)
	}
	if _, e = s.pool.Exec(ctx, "UPDATE payment_orders SET created_at=clock_timestamp()-interval '25 hours' WHERE id=$1", old.orderID); e != nil {
		t.Fatal(e)
	}
	if _, e = s.CheckoutPayment(ctx, who, "expired-key-1", 500, old); !errors.Is(e, ErrConflict) {
		t.Fatalf("recreated expired provider key %v", e)
	}
	if old.calls != 1 {
		t.Fatal("contacted provider after idempotency retention window")
	}
	paymentBalance(t, s, who, 0, 0)
}

func TestPaymentsUnmatchedRefundEventsAreDurableAndDeduplicated(t *testing.T) {
	s := paymentTestStore(t)
	ctx := context.Background()
	for _, kind := range []string{"refund.created", "refund.updated", "refund.failed", "charge.refund.updated", "charge.refunded"} {
		event := paymentEvent(t, "evt_"+kind, kind, map[string]any{"id": "re_unmatched", "payment_intent": "pi_unmatched", "object": "refund", "amount": 500, "currency": "usd"})
		for i := 0; i < 2; i++ {
			if e := s.ApplyStripeEvent(ctx, event); e != nil {
				t.Fatal(e)
			}
		}
	}
	var incidents int
	if e := s.pool.QueryRow(ctx, "SELECT count(*) FROM payment_incidents WHERE state='needs-review'").Scan(&incidents); e != nil || incidents != 5 {
		t.Fatalf("incidents=%d %v", incidents, e)
	}
}

func TestPaymentsFullRefundAfterPendingNotificationReversesOnce(t *testing.T) {
	s := paymentTestStore(t)
	ctx := context.Background()
	who, _ := register(t, s, "refund-lifecycle@example.com")
	order := paymentOrder(t, s, who, "refund-lifecycle")
	if e := s.ApplyStripeEvent(ctx, paymentEvent(t, "evt_lifecycle_credit", "checkout.session.completed", paidObject(order))); e != nil {
		t.Fatal(e)
	}
	pending := map[string]any{"id": "re_lifecycle", "object": "refund", "payment_intent": "pi_" + order.ID, "amount": 500, "currency": "usd", "status": "pending"}
	if e := s.ApplyStripeEvent(ctx, paymentEvent(t, "evt_pending_refund", "refund.created", pending)); e != nil {
		t.Fatal(e)
	}
	paymentBalance(t, s, who, 500, 1)
	if e := s.ApplyStripeEvent(ctx, paymentEvent(t, "evt_lifecycle_full", "charge.refunded", refundObject(order))); e != nil {
		t.Fatal(e)
	}
	paymentBalance(t, s, who, 0, 2)
	if e := s.ApplyStripeEvent(ctx, paymentEvent(t, "evt_lifecycle_full_again", "charge.refunded", refundObject(order))); e != nil {
		t.Fatal(e)
	}
	paymentBalance(t, s, who, 0, 2)
	paymentFrozen(t, s, who, true)
}

func paymentFrozen(t *testing.T, s *Store, who Identity, want bool) {
	t.Helper()
	var frozen bool
	if e := s.pool.QueryRow(context.Background(), "SELECT spending_frozen FROM billing_wallets WHERE tenant_id=$1", who.TenantID).Scan(&frozen); e != nil || frozen != want {
		t.Fatalf("frozen=%v want=%v error=%v", frozen, want, e)
	}
	tx, e := s.pool.Begin(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(context.Background())
	allowed, e := CheckWalletSpendingAllowed(context.Background(), tx, who.TenantID)
	if e != nil || allowed == want {
		t.Fatalf("spending allowed=%v frozen=%v error=%v", allowed, want, e)
	}
}

func TestPaymentsDisputeLifecycleFreezesWithoutLedgerMutation(t *testing.T) {
	for _, kind := range []string{"charge.dispute.created", "charge.dispute.updated", "charge.dispute.closed", "charge.dispute.funds_withdrawn", "charge.dispute.funds_reinstated", "charge.dispute.future_event"} {
		t.Run(kind, func(t *testing.T) {
			s := paymentTestStore(t)
			ctx := context.Background()
			who, _ := register(t, s, "dispute@example.com")
			order := paymentOrder(t, s, who, "dispute-order")
			if e := s.ApplyStripeEvent(ctx, paymentEvent(t, "evt_dispute_credit", "checkout.session.completed", paidObject(order))); e != nil {
				t.Fatal(e)
			}
			obj := map[string]any{"id": "du_test", "object": "dispute", "payment_intent": "pi_" + order.ID, "amount": 500, "currency": "usd", "status": "won"}
			event := paymentEvent(t, "evt_dispute", kind, obj)
			var wg sync.WaitGroup
			results := make(chan error, 8)
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() { defer wg.Done(); results <- s.ApplyStripeEvent(ctx, event) }()
			}
			wg.Wait()
			close(results)
			for e := range results {
				if e != nil {
					t.Fatal(e)
				}
			}
			paymentBalance(t, s, who, 500, 1)
			paymentFrozen(t, s, who, true)
			var state string
			if e := s.pool.QueryRow(ctx, "SELECT state FROM payment_orders WHERE id=$1", order.ID).Scan(&state); e != nil || state != "needs-review" {
				t.Fatalf("state=%s %v", state, e)
			}
			for _, query := range []string{"SELECT count(*) FROM payment_incidents", "SELECT count(*) FROM platform_audit_logs WHERE action='payment.needs-review'"} {
				var n int
				if e := s.pool.QueryRow(ctx, query).Scan(&n); e != nil || n != 1 {
					t.Fatalf("duplicate review writes=%d %v", n, e)
				}
			}
			changed := map[string]any{"id": "du_test", "object": "dispute", "payment_intent": "pi_" + order.ID, "amount": 501, "currency": "usd"}
			if e := s.ApplyStripeEvent(ctx, paymentEvent(t, "evt_dispute", kind, changed)); !errors.Is(e, ErrConflict) {
				t.Fatalf("conflicting dispute replay %v", e)
			}
			if e := s.ApplyStripeEvent(ctx, paymentEvent(t, "evt_dispute_closed", "charge.dispute.closed", obj)); e != nil {
				t.Fatal(e)
			}
			paymentBalance(t, s, who, 500, 1)
			paymentFrozen(t, s, who, true)
		})
	}
}

func TestPaymentsUnmatchedDisputeAndReviewAfterRefundIncident(t *testing.T) {
	s := paymentTestStore(t)
	ctx := context.Background()
	who, _ := register(t, s, "dispute-review@example.com")
	order := paymentOrder(t, s, who, "dispute-review")
	if e := s.ApplyStripeEvent(ctx, paymentEvent(t, "evt_review_credit", "checkout.session.completed", paidObject(order))); e != nil {
		t.Fatal(e)
	}
	partial := refundObject(order)
	partial["amount_refunded"] = 100
	partial["refunded"] = false
	if e := s.ApplyStripeEvent(ctx, paymentEvent(t, "evt_review_partial", "charge.refunded", partial)); e != nil {
		t.Fatal(e)
	}
	obj := map[string]any{"id": "du_other", "object": "dispute", "payment_intent": "pi_" + order.ID}
	if e := s.ApplyStripeEvent(ctx, paymentEvent(t, "evt_review_dispute", "charge.dispute.created", obj)); e != nil {
		t.Fatal(e)
	}
	obj["payment_intent"] = "pi_unknown"
	for i := 0; i < 2; i++ {
		if e := s.ApplyStripeEvent(ctx, paymentEvent(t, "evt_unmatched_dispute", "charge.dispute.created", obj)); e != nil {
			t.Fatal(e)
		}
	}
	var n int
	if e := s.pool.QueryRow(ctx, "SELECT count(*) FROM payment_incidents").Scan(&n); e != nil || n != 3 {
		t.Fatalf("refund and both disputes need explicit review records: %d %v", n, e)
	}
	paymentBalance(t, s, who, 500, 1)
	paymentFrozen(t, s, who, true)
}

func TestPaymentsFreezeAndDisputeReviewAuditFailureRollsBack(t *testing.T) {
	s := paymentTestStore(t)
	ctx := context.Background()
	who, _ := register(t, s, "freeze-rollback@example.com")
	order := paymentOrder(t, s, who, "freeze-rollback")
	if e := s.ApplyStripeEvent(ctx, paymentEvent(t, "evt_freeze_credit", "checkout.session.completed", paidObject(order))); e != nil {
		t.Fatal(e)
	}
	if _, e := s.pool.Exec(ctx, "ALTER TABLE platform_audit_logs ADD CONSTRAINT payment_hold_audit_reject CHECK(action<>'payment.needs-review') NOT VALID"); e != nil {
		t.Fatal(e)
	}
	event := paymentEvent(t, "evt_freeze_dispute", "charge.dispute.created", map[string]any{"id": "du_freeze", "object": "dispute", "payment_intent": "pi_" + order.ID, "amount": 500, "currency": "usd"})
	if e := s.ApplyStripeEvent(ctx, event); e == nil {
		t.Fatal("accepted failed dispute audit")
	}
	paymentFrozen(t, s, who, false)
	paymentBalance(t, s, who, 500, 1)
	for _, query := range []string{"SELECT count(*) FROM payment_provider_events WHERE event_id='evt_freeze_dispute'", "SELECT count(*) FROM payment_incidents", "SELECT count(*) FROM payment_orders WHERE state='needs-review'"} {
		var n int
		if e := s.pool.QueryRow(ctx, query).Scan(&n); e != nil || n != 0 {
			t.Fatalf("failed audit left review state %d %v", n, e)
		}
	}
	if _, e := s.pool.Exec(ctx, "ALTER TABLE platform_audit_logs DROP CONSTRAINT payment_hold_audit_reject"); e != nil {
		t.Fatal(e)
	}
	if e := s.ApplyStripeEvent(ctx, event); e != nil {
		t.Fatal(e)
	}
	paymentFrozen(t, s, who, true)
	paymentBalance(t, s, who, 500, 1)
	if _, e := s.pool.Exec(ctx, PaymentFreezeMigrationSQL); e != nil {
		t.Fatal(e)
	}
	paymentFrozen(t, s, who, true) // Reapplying the additive migration cannot release a hold.
	// Simulate the false default on review history created before migration 12.
	if _, e := s.pool.Exec(ctx, "UPDATE billing_wallets SET spending_frozen=false WHERE tenant_id=$1", who.TenantID); e != nil {
		t.Fatal(e)
	}
	if _, e := s.pool.Exec(ctx, PaymentFreezeMigrationSQL); e != nil {
		t.Fatal(e)
	}
	paymentFrozen(t, s, who, true)
	paymentBalance(t, s, who, 500, 1)
}

func TestPaymentsAmbiguousDisputeFreezesEveryMatchedTenant(t *testing.T) {
	s := paymentTestStore(t)
	ctx := context.Background()
	a, _ := register(t, s, "hold-a@example.com")
	b, _ := register(t, s, "hold-b@example.com")
	oa := paymentOrder(t, s, a, "hold-order-a")
	ob := paymentOrder(t, s, b, "hold-order-b")
	for i, o := range []PaymentOrder{oa, ob} {
		if e := s.ApplyStripeEvent(ctx, paymentEvent(t, fmt.Sprintf("evt_hold_credit_%d", i), "checkout.session.completed", paidObject(o))); e != nil {
			t.Fatal(e)
		}
	}
	obj := map[string]any{"id": "du_ambiguous", "object": "dispute", "payment_intent": "pi_" + oa.ID, "amount": 100, "currency": "usd", "metadata": map[string]string{"order_id": ob.ID}}
	if e := s.ApplyStripeEvent(ctx, paymentEvent(t, "evt_ambiguous_dispute", "charge.dispute.created", obj)); e != nil {
		t.Fatal(e)
	}
	for _, who := range []Identity{a, b} {
		paymentFrozen(t, s, who, true)
		paymentBalance(t, s, who, 500, 1)
	}
	var n int
	if e := s.pool.QueryRow(ctx, "SELECT count(*) FROM payment_incidents").Scan(&n); e != nil || n != 2 {
		t.Fatalf("all implicated tenants need incidents %d %v", n, e)
	}
}
