package platform

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"reladock/internal/payments"
)

type paymentFake struct{}

func (paymentFake) CreateCheckout(_ context.Context, r payments.CheckoutRequest) (payments.Session, error) {
	return payments.Session{ID: "cs_" + r.OrderID, URL: "https://checkout.stripe.com/c/pay/test", Currency: "usd", AmountTotal: r.AmountCents, Metadata: map[string]string{"order_id": r.OrderID, "tenant_id": r.TenantID, "amount_cents": fmt.Sprint(r.AmountCents), "currency": "usd"}}, nil
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
