package platform

import (
	"context"
	_ "embed"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"reladock/internal/payments"
)

// PaymentMigrationSQL must be registered as migration 9 by the parent.
//
//go:embed migration9.sql
var PaymentMigrationSQL string

// PaymentFreezeMigrationSQL must be registered as migration 12 by the parent.
//
//go:embed migration12.sql
var PaymentFreezeMigrationSQL string

type PaymentOrder struct {
	ID          string    `json:"id"`
	TenantID    string    `json:"-"`
	AmountCents int64     `json:"amountCents"`
	Currency    string    `json:"currency"`
	State       string    `json:"state"`
	SessionID   string    `json:"-"`
	CheckoutURL string    `json:"checkoutUrl,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
}

const paymentColumns = "id::text,tenant_id::text,amount_cents,currency,state,COALESCE(session_id,''),COALESCE(checkout_url,''),created_at"

func scanPayment(row pgx.Row) (PaymentOrder, error) {
	var o PaymentOrder
	e := row.Scan(&o.ID, &o.TenantID, &o.AmountCents, &o.Currency, &o.State, &o.SessionID, &o.CheckoutURL, &o.CreatedAt)
	return o, e
}

// CheckoutPayment commits a local order before contacting Stripe. Uncertain
// provider results retain the order and reuse its provider idempotency key.
func (s *Store) CheckoutPayment(ctx context.Context, who Identity, key string, amount int64, provider payments.Provider) (PaymentOrder, error) {
	if provider == nil {
		return PaymentOrder{}, payments.ErrProvider
	}
	if !billingUUID.MatchString(who.TenantID) || !billingUUID.MatchString(who.MemberID) || !idempotencyKey.MatchString(key) || amount < payments.MinTopUpCents || amount > payments.MaxTopUpCents {
		return PaymentOrder{}, ErrInvalid
	}
	o, err := s.createPaymentOrder(ctx, who, key, amount)
	if err != nil {
		return o, err
	}
	if o.State != "pending" {
		o.CheckoutURL = ""
		return o, nil
	}
	if o.SessionID != "" {
		return o, nil
	}
	// Stripe v1 can discard idempotency keys after 24 hours. Never recreate an
	// uncertain checkout with a potentially expired provider key.
	if time.Since(o.CreatedAt) > 23*time.Hour {
		return PaymentOrder{}, ErrConflict
	}
	callCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	session, err := provider.CreateCheckout(callCtx, payments.CheckoutRequest{OrderID: o.ID, TenantID: o.TenantID, AmountCents: o.AmountCents})
	if err != nil {
		return PaymentOrder{}, payments.ErrProvider
	}
	if session.ID == "" || len(session.ID) > 255 || !payments.ValidCheckoutURL(session.URL) || session.Currency != "usd" || session.AmountTotal != amount || !paymentMetadata(session.Metadata, o) {
		return PaymentOrder{}, payments.ErrProvider
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PaymentOrder{}, err
	}
	defer tx.Rollback(ctx)
	if err = s.AuthorizeWrite(ctx, tx, who); err != nil {
		return PaymentOrder{}, err
	}
	current, err := scanPayment(tx.QueryRow(ctx, "SELECT "+paymentColumns+" FROM payment_orders WHERE id=$1 AND tenant_id=$2 FOR UPDATE", o.ID, who.TenantID))
	if err != nil {
		return PaymentOrder{}, err
	}
	if current.SessionID != "" {
		if current.SessionID != session.ID {
			return PaymentOrder{}, ErrConflict
		}
		if current.State != "pending" {
			current.CheckoutURL = ""
		}
		return current, tx.Commit(ctx)
	}
	if current.State != "pending" {
		return PaymentOrder{}, ErrConflict
	}
	if _, err = tx.Exec(ctx, "UPDATE payment_orders SET session_id=$2,checkout_url=$3 WHERE id=$1", o.ID, session.ID, session.URL); err != nil {
		return PaymentOrder{}, memberConflict(err)
	}
	if err = platformLog(ctx, tx, &who, "payment.checkout", o.ID, map[string]any{"tenantId": o.TenantID}); err != nil {
		return PaymentOrder{}, err
	}
	o.SessionID = session.ID
	o.CheckoutURL = session.URL
	return o, tx.Commit(ctx)
}
func (s *Store) createPaymentOrder(ctx context.Context, who Identity, key string, amount int64) (PaymentOrder, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PaymentOrder{}, err
	}
	defer tx.Rollback(ctx)
	if err = s.AuthorizeWrite(ctx, tx, who); err != nil {
		return PaymentOrder{}, err
	}
	result, err := tx.Exec(ctx, "INSERT INTO payment_orders(id,tenant_id,created_by,idempotency_key,amount_cents) VALUES($1,$2,$3,$4,$5) ON CONFLICT(tenant_id,idempotency_key) DO NOTHING", newID(), who.TenantID, who.MemberID, key, amount)
	if err != nil {
		return PaymentOrder{}, err
	}
	o, err := scanPayment(tx.QueryRow(ctx, "SELECT "+paymentColumns+" FROM payment_orders WHERE tenant_id=$1 AND idempotency_key=$2", who.TenantID, key))
	if err != nil {
		return o, err
	}
	if o.AmountCents != amount {
		return PaymentOrder{}, ErrConflict
	}
	if result.RowsAffected() == 1 {
		if err = platformLog(ctx, tx, &who, "payment.order", o.ID, map[string]any{"tenantId": o.TenantID, "amountCents": amount, "currency": "USD"}); err != nil {
			return PaymentOrder{}, err
		}
	}
	return o, tx.Commit(ctx)
}
func paymentMetadata(m map[string]string, o PaymentOrder) bool {
	return m["order_id"] == o.ID && m["tenant_id"] == o.TenantID && m["amount_cents"] == strconv.FormatInt(o.AmountCents, 10) && m["currency"] == "usd"
}

// ApplyStripeEvent accepts only a signature-verified event. All provider event
// identity, incident, order, wallet, ledger and audit writes commit together.
func (s *Store) ApplyStripeEvent(ctx context.Context, verified payments.VerifiedEvent) error {
	e := verified.Event()
	if e.ID == "" || len(verified.Hash()) != 64 {
		return ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Serialize webhook events, including refund-before-credit and different IDs
	// for the same session, before taking order and wallet locks.
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(7263520199)"); err != nil {
		return err
	}
	inserted, err := tx.Exec(ctx, "INSERT INTO payment_provider_events(event_id,payload_hash,event_type) VALUES($1,$2,$3) ON CONFLICT DO NOTHING", e.ID, verified.Hash(), e.Type)
	if err != nil {
		return err
	}
	if inserted.RowsAffected() == 0 {
		var hash string
		if err = tx.QueryRow(ctx, "SELECT payload_hash FROM payment_provider_events WHERE event_id=$1", e.ID).Scan(&hash); err != nil {
			return err
		}
		if hash != verified.Hash() {
			return ErrConflict
		}
		return tx.Commit(ctx)
	}
	obj := e.Data.Object
	isPaid := e.Type == "checkout.session.completed" || e.Type == "checkout.session.async_payment_succeeded"
	isRefund := strings.HasPrefix(e.Type, "refund.") || e.Type == "charge.refunded" || e.Type == "charge.refund.updated"
	isDispute := strings.HasPrefix(e.Type, "charge.dispute.")
	if !isPaid && !isRefund && !isDispute {
		return tx.Commit(ctx)
	}
	if e.Account != "" {
		if err = paymentReview(ctx, tx, nil, e, "connected-account-event"); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	var order PaymentOrder
	if isPaid {
		if !billingUUID.MatchString(obj.Metadata["order_id"]) {
			err = pgx.ErrNoRows
		} else {
			order, err = scanPayment(tx.QueryRow(ctx, "SELECT "+paymentColumns+" FROM payment_orders WHERE id=$1 FOR UPDATE", obj.Metadata["order_id"]))
		}
	} else {
		// Only a previously bound intent or server metadata may locate an order.
		id := obj.Metadata["order_id"]
		if !billingUUID.MatchString(id) {
			id = ""
		}
		rows, queryErr := tx.Query(ctx, "SELECT "+paymentColumns+" FROM payment_orders WHERE (payment_intent=$1 AND $1<>'') OR id=NULLIF($2,'')::uuid ORDER BY id FOR UPDATE", obj.PaymentIntent, id)
		if queryErr != nil {
			return queryErr
		}
		var matches []PaymentOrder
		for rows.Next() {
			matched, scanErr := scanPayment(rows)
			if scanErr != nil {
				rows.Close()
				return scanErr
			}
			matches = append(matches, matched)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(matches) > 1 {
			// Conflicting intent and order metadata must hold every implicated
			// wallet, rather than selecting an arbitrary tenant or making a debit.
			for i := range matches {
				if err = paymentReview(ctx, tx, &matches[i], e, "ambiguous-provider-order"); err != nil {
					return err
				}
			}
			return tx.Commit(ctx)
		}
		if len(matches) == 0 {
			err = pgx.ErrNoRows
		} else {
			order = matches[0]
		}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		if err = paymentReview(ctx, tx, nil, e, "unmatched-provider-event"); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if err != nil {
		return err
	}
	if isPaid {
		if order.SessionID == "" && order.State == "pending" {
			return ErrConflict
		} // Retry after checkout attachment commits.
		err = paymentCredit(ctx, tx, order, e)
	} else if isDispute {
		err = paymentReview(ctx, tx, &order, e, "stripe-dispute:"+e.Type)
	} else {
		err = paymentRefund(ctx, tx, order, e)
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func paymentCredit(ctx context.Context, tx pgx.Tx, o PaymentOrder, e payments.Event) error {
	obj := e.Data.Object
	if obj.Object != "checkout.session" || obj.Mode != "payment" || obj.Status != "complete" || obj.PaymentStatus != "paid" || obj.Currency != "usd" || obj.AmountTotal != o.AmountCents || obj.ID != o.SessionID || obj.PaymentIntent == "" || !paymentMetadata(obj.Metadata, o) {
		return paymentReview(ctx, tx, &o, e, "checkout-mismatch-or-unpaid")
	}
	var intent string
	var credited bool
	if err := tx.QueryRow(ctx, "SELECT COALESCE(payment_intent,''),credit_ledger_id IS NOT NULL FROM payment_orders WHERE id=$1", o.ID).Scan(&intent, &credited); err != nil {
		return err
	}
	if intent != "" && intent != obj.PaymentIntent {
		return paymentReview(ctx, tx, &o, e, "payment-intent-mismatch")
	}
	if credited || o.State != "pending" {
		return nil
	}
	var review bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM payment_incidents WHERE payment_intent=$1)", obj.PaymentIntent).Scan(&review); err != nil {
		return err
	}
	if review {
		return paymentReview(ctx, tx, &o, e, "refund-preceded-credit")
	}
	// Intent uniqueness stops a provider intent from funding two local orders.
	var owner string
	err := tx.QueryRow(ctx, "SELECT id::text FROM payment_orders WHERE payment_intent=$1", obj.PaymentIntent).Scan(&owner)
	if err == nil && owner != o.ID {
		return paymentReview(ctx, tx, &o, e, "intent-already-used")
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	balance, err := paymentWalletLock(ctx, tx, o.TenantID)
	if err != nil {
		return err
	}
	if balance > math.MaxInt64-o.AmountCents {
		return paymentReview(ctx, tx, &o, e, "wallet-overflow")
	}
	ledger, err := paymentLedger(ctx, tx, o, "payment-credit", o.AmountCents, balance+o.AmountCents)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "UPDATE payment_orders SET state='credited',payment_intent=$2,credit_ledger_id=$3 WHERE id=$1", o.ID, obj.PaymentIntent, ledger); err != nil {
		return err
	}
	return paymentAudit(ctx, tx, "payment.credit", o.ID, e.ID)
}
func paymentRefund(ctx context.Context, tx pgx.Tx, o PaymentOrder, e payments.Event) error {
	obj := e.Data.Object
	var intent string
	var credited, refunded bool
	if err := tx.QueryRow(ctx, "SELECT COALESCE(payment_intent,''),credit_ledger_id IS NOT NULL,refund_ledger_id IS NOT NULL FROM payment_orders WHERE id=$1", o.ID).Scan(&intent, &credited, &refunded); err != nil {
		return err
	}
	if refunded {
		return nil
	}
	// Pending refund notifications may have marked an already credited order for
	// review. A later authoritative full refund can still reverse that credit;
	// the original incident remains durable for operational reconciliation.
	if e.Type != "charge.refunded" || obj.Object != "charge" || obj.PaymentIntent == "" || obj.PaymentIntent != intent || obj.Currency != "usd" || obj.Amount != o.AmountCents || obj.AmountRefunded != o.AmountCents || !obj.Refunded || !credited {
		return paymentReview(ctx, tx, &o, e, "refund-requires-review")
	}
	balance, err := paymentWalletLock(ctx, tx, o.TenantID)
	if err != nil {
		return err
	}
	if balance < o.AmountCents {
		return paymentReview(ctx, tx, &o, e, "refund-insufficient-wallet-funds")
	}
	ledger, err := paymentLedger(ctx, tx, o, "payment-refund", -o.AmountCents, balance-o.AmountCents)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "UPDATE payment_orders SET state='refunded',refund_ledger_id=$2 WHERE id=$1", o.ID, ledger); err != nil {
		return err
	}
	return paymentAudit(ctx, tx, "payment.refund", o.ID, e.ID)
}
func paymentWalletLock(ctx context.Context, tx pgx.Tx, tenant string) (int64, error) {
	if _, err := tx.Exec(ctx, "INSERT INTO billing_wallets(tenant_id) VALUES($1) ON CONFLICT DO NOTHING", tenant); err != nil {
		return 0, err
	}
	var balance int64
	err := tx.QueryRow(ctx, "SELECT balance_cents FROM billing_wallets WHERE tenant_id=$1 FOR UPDATE", tenant).Scan(&balance)
	return balance, err
}

// CheckWalletSpendingAllowed locks an existing wallet until the caller commits
// or rolls back tx. Call after tenant authorization and wallet creation, before
// any debit, in the SAME transaction as that debit. False means a persistent
// payment-review hold; errors must also prevent spending. Never clears holds.
func CheckWalletSpendingAllowed(ctx context.Context, tx pgx.Tx, tenantID string) (bool, error) {
	if !billingUUID.MatchString(tenantID) {
		return false, ErrInvalid
	}
	var allowed bool
	err := tx.QueryRow(ctx, "SELECT NOT spending_frozen FROM billing_wallets WHERE tenant_id=$1 FOR UPDATE", tenantID).Scan(&allowed)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	return allowed, err
}
func paymentLedger(ctx context.Context, tx pgx.Tx, o PaymentOrder, kind string, delta, balance int64) (int64, error) {
	if _, err := tx.Exec(ctx, "UPDATE billing_wallets SET balance_cents=$2 WHERE tenant_id=$1", o.TenantID, balance); err != nil {
		return 0, err
	}
	var id int64
	err := tx.QueryRow(ctx, "INSERT INTO billing_ledger(tenant_id,kind,amount_cents,balance_after_cents,idempotency_key,reason,operator_name) VALUES($1,$2,$3,$4,$5,$6,'stripe-webhook') RETURNING id", o.TenantID, kind, delta, balance, kind+":"+o.ID, "Verified Stripe "+kind).Scan(&id)
	return id, err
}
func paymentReview(ctx context.Context, tx pgx.Tx, o *PaymentOrder, e payments.Event, reason string) error {
	var orderID any
	key := "event:" + e.ID
	if o != nil {
		orderID = o.ID
		key = "order:" + o.ID
		// Order -> wallet is the same lock order used by normal payment
		// handling. The hold and review are committed with the provider event.
		if _, err := paymentWalletLock(ctx, tx, o.TenantID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE billing_wallets SET spending_frozen=true WHERE tenant_id=$1 AND NOT spending_frozen", o.TenantID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE payment_orders SET state='needs-review' WHERE id=$1 AND state NOT IN ('refunded','needs-review')", o.ID); err != nil {
			return err
		}
	}
	if strings.HasPrefix(e.Type, "charge.dispute.") {
		// Each dispute notification remains explicit even if this order
		// already has a refund incident; identical event retries stop upstream.
		key += ":dispute:" + e.ID
	}
	result, err := tx.Exec(ctx, "INSERT INTO payment_incidents(id,dedupe_key,order_id,event_id,payment_intent,reason) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(dedupe_key) DO NOTHING", newID(), key, orderID, e.ID, e.Data.Object.PaymentIntent, reason)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return nil
	}
	return paymentAudit(ctx, tx, "payment.needs-review", key, e.ID)
}
func paymentAudit(ctx context.Context, tx pgx.Tx, action, resource, eventID string) error {
	_, err := tx.Exec(ctx, "INSERT INTO platform_audit_logs(actor_email,action,resource_id,details) VALUES('stripe-webhook',$1,$2,jsonb_build_object('provider','stripe','eventId',$3::text))", action, resource, eventID)
	return err
}
