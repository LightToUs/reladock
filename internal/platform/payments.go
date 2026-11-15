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
	ID             string    `json:"id"`
	TenantID       string    `json:"-"`
	CreatedBy      string    `json:"-"`
	AmountCents    int64     `json:"amountCents"`
	Currency       string    `json:"currency"`
	State          string    `json:"state"`
	SessionID      string    `json:"-"`
	CheckoutURL    string    `json:"checkoutUrl,omitempty"`
	Purpose        string    `json:"purpose"`
	RuleID         string    `json:"ruleId,omitempty"`
	PriceVersionID string    `json:"priceVersionId,omitempty"`
	PurchaseState  string    `json:"purchaseState"`
	PurchaseError  string    `json:"purchaseError,omitempty"`
	SubscriptionID string    `json:"subscriptionId,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
}

const paymentColumns = "id::text,tenant_id::text,created_by::text,amount_cents,currency,state,COALESCE(session_id,''),COALESCE(checkout_url,''),purpose,COALESCE(rule_id::text,''),COALESCE(price_version_id::text,''),purchase_state,purchase_error,COALESCE(subscription_id::text,''),created_at"

func scanPayment(row pgx.Row) (PaymentOrder, error) {
	var o PaymentOrder
	e := row.Scan(&o.ID, &o.TenantID, &o.CreatedBy, &o.AmountCents, &o.Currency, &o.State, &o.SessionID, &o.CheckoutURL, &o.Purpose, &o.RuleID, &o.PriceVersionID, &o.PurchaseState, &o.PurchaseError, &o.SubscriptionID, &o.CreatedAt)
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
	return s.attachPaymentCheckout(ctx, who, o, provider)
}

// CheckoutPlanPayment creates or recovers one pending Stripe checkout for the
// selected immutable plan. Wallet funds are not reserved; the callback repeats
// all purchase checks before spending the credited balance.
func (s *Store) CheckoutPlanPayment(ctx context.Context, who Identity, key string, input PurchasePlanInput, provider payments.Provider) (PaymentOrder, error) {
	if provider == nil {
		return PaymentOrder{}, payments.ErrProvider
	}
	if !billingUUID.MatchString(who.TenantID) || !billingUUID.MatchString(who.MemberID) || !idempotencyKey.MatchString(key) || input.RuleID != "" && !billingUUID.MatchString(input.RuleID) || !billingUUID.MatchString(input.PriceVersionID) {
		return PaymentOrder{}, ErrInvalid
	}
	o, err := s.createPlanPaymentOrder(ctx, who, key, input)
	if err != nil {
		return o, err
	}
	return s.attachPaymentCheckout(ctx, who, o, provider)
}

func (s *Store) attachPaymentCheckout(ctx context.Context, who Identity, o PaymentOrder, provider payments.Provider) (PaymentOrder, error) {
	if o.State != "pending" {
		o.CheckoutURL = ""
		return o, nil
	}
	if o.SessionID != "" {
		if mode, ok := provider.(interface{ LiveMode() bool }); ok {
			sessionLive, known := stripeSessionLiveMode(o.SessionID)
			if known && sessionLive != mode.LiveMode() {
				return PaymentOrder{}, ErrConflict
			}
		}
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
	if session.ID == "" || len(session.ID) > 255 || !payments.ValidCheckoutURL(session.URL) || session.Currency != "usd" || session.AmountTotal != o.AmountCents || !paymentMetadata(session.Metadata, o) {
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

func stripeSessionLiveMode(id string) (bool, bool) {
	switch {
	case strings.HasPrefix(id, "cs_live_"):
		return true, true
	case strings.HasPrefix(id, "cs_test_"):
		return false, true
	default:
		return false, false
	}
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

func (s *Store) createPlanPaymentOrder(ctx context.Context, who Identity, key string, input PurchasePlanInput) (PaymentOrder, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PaymentOrder{}, err
	}
	defer tx.Rollback(ctx)
	if err = s.AuthorizeWrite(ctx, tx, who); err != nil {
		return PaymentOrder{}, err
	}
	selection := "payment-plan-selection:" + who.TenantID + ":" + input.RuleID + ":" + input.PriceVersionID
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", selection); err != nil {
		return PaymentOrder{}, err
	}

	previous, previousErr := scanPayment(tx.QueryRow(ctx, "SELECT "+paymentColumns+" FROM payment_orders WHERE tenant_id=$1 AND idempotency_key=$2", who.TenantID, key))
	if previousErr == nil {
		if previous.Purpose != "plan-purchase" || previous.RuleID != input.RuleID || previous.PriceVersionID != input.PriceVersionID {
			return PaymentOrder{}, ErrConflict
		}
		return previous, tx.Commit(ctx)
	}
	if !errors.Is(previousErr, pgx.ErrNoRows) {
		return PaymentOrder{}, previousErr
	}

	pending, pendingErr := scanPayment(tx.QueryRow(ctx, "SELECT "+paymentColumns+" FROM payment_orders WHERE tenant_id=$1 AND purpose='plan-purchase' AND COALESCE(rule_id::text,'')=$2 AND price_version_id=$3 AND state='pending'", who.TenantID, input.RuleID, input.PriceVersionID))
	if pendingErr == nil {
		return pending, tx.Commit(ctx)
	}
	if !errors.Is(pendingErr, pgx.ErrNoRows) {
		return PaymentOrder{}, pendingErr
	}

	var nodeID string
	if input.RuleID != "" {
		err = tx.QueryRow(ctx, "SELECT node_id FROM relay_rules WHERE id=$1 AND tenant_id=$2 AND status<>'deleted'", input.RuleID, who.TenantID).Scan(&nodeID)
		if errors.Is(err, pgx.ErrNoRows) {
			return PaymentOrder{}, ErrNotFound
		}
		if err != nil {
			return PaymentOrder{}, err
		}
		if err = lockNode(ctx, tx, nodeID); err != nil {
			return PaymentOrder{}, err
		}
		var lockedNode, status string
		err = tx.QueryRow(ctx, "SELECT node_id,status FROM relay_rules WHERE id=$1 AND tenant_id=$2 FOR NO KEY UPDATE", input.RuleID, who.TenantID).Scan(&lockedNode, &status)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && status == "deleted" {
			return PaymentOrder{}, ErrNotFound
		}
		if err != nil {
			return PaymentOrder{}, err
		}
		if lockedNode != nodeID {
			return PaymentOrder{}, ErrConflict
		}
	}

	plan, err := scanPlan(tx.QueryRow(ctx, "SELECT "+planSelectColumns+planSelectFrom+" WHERE v.id=$1", input.PriceVersionID))
	if err != nil {
		return PaymentOrder{}, err
	}
	if plan.Offering == nil {
		return PaymentOrder{}, ErrConflict
	}
	var offeringNode string
	var offeringState OfferingState
	err = tx.QueryRow(ctx, "SELECT node_id,state FROM line_offerings WHERE id=$1 FOR SHARE", plan.OfferingID).Scan(&offeringNode, &offeringState)
	if errors.Is(err, pgx.ErrNoRows) {
		return PaymentOrder{}, ErrConflict
	}
	if err != nil {
		return PaymentOrder{}, err
	}
	if offeringState != OfferingStatePublished || input.RuleID != "" && offeringNode != nodeID {
		return PaymentOrder{}, ErrConflict
	}
	if input.RuleID == "" {
		nodeID = offeringNode
		if err = lockNode(ctx, tx, nodeID); err != nil {
			return PaymentOrder{}, err
		}
		if err = checkNodeAdmission(ctx, tx, nodeID, who.TenantID); err != nil {
			return PaymentOrder{}, err
		}
	}
	var capacity int
	var resourceMode ResourceMode
	var nodeStatus string
	if err = tx.QueryRow(ctx, "SELECT capacity_mbps,resource_mode,status FROM nodes WHERE id=$1", nodeID).Scan(&capacity, &resourceMode, &nodeStatus); err != nil {
		return PaymentOrder{}, err
	}
	if plan.ResourceMode != resourceMode || plan.BandwidthMbps > capacity {
		return PaymentOrder{}, ErrConflict
	}
	if !nodeAllowsSales(nodeStatus) {
		return PaymentOrder{}, ErrNodeNotAllocatable
	}
	if input.RuleID != "" {
		var oldID string
		err = tx.QueryRow(ctx, "SELECT id FROM rule_subscriptions WHERE rule_id=$1 ORDER BY ordinal DESC LIMIT 1 FOR UPDATE", input.RuleID).Scan(&oldID)
		if err == nil {
			old, readErr := readSubscription(ctx, tx, oldID)
			if readErr != nil {
				return PaymentOrder{}, readErr
			}
			if old.Status == "active" {
				return PaymentOrder{}, ErrConflict
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return PaymentOrder{}, err
		}
	}

	var balance int64
	var frozen bool
	err = tx.QueryRow(ctx, "SELECT balance_cents,spending_frozen FROM billing_wallets WHERE tenant_id=$1 FOR UPDATE", who.TenantID).Scan(&balance, &frozen)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return PaymentOrder{}, err
	}
	if frozen {
		return PaymentOrder{}, ErrForbidden
	}
	if balance >= plan.PriceCents {
		return PaymentOrder{}, ErrConflict
	}
	amount := plan.PriceCents - balance
	if amount < payments.MinTopUpCents {
		amount = payments.MinTopUpCents
	}
	if amount > payments.MaxTopUpCents {
		return PaymentOrder{}, ErrInvalid
	}

	id := newID()
	_, err = tx.Exec(ctx, `INSERT INTO payment_orders(id,tenant_id,created_by,idempotency_key,amount_cents,purpose,rule_id,price_version_id,purchase_state) VALUES($1,$2,$3,$4,$5,'plan-purchase',NULLIF($6,'')::uuid,$7,'pending')`, id, who.TenantID, who.MemberID, key, amount, input.RuleID, input.PriceVersionID)
	if err != nil {
		return PaymentOrder{}, memberConflict(err)
	}
	o, err := scanPayment(tx.QueryRow(ctx, "SELECT "+paymentColumns+" FROM payment_orders WHERE id=$1", id))
	if err != nil {
		return PaymentOrder{}, err
	}
	if err = platformLog(ctx, tx, &who, "payment.plan-order", o.ID, map[string]any{"tenantId": o.TenantID, "amountCents": amount, "priceVersionId": o.PriceVersionID, "ruleId": o.RuleID}); err != nil {
		return PaymentOrder{}, err
	}
	return o, tx.Commit(ctx)
}

// PaymentOrder returns one tenant-scoped order for checkout recovery.
func (s *Store) PaymentOrder(ctx context.Context, who Identity, id string) (PaymentOrder, error) {
	if !billingUUID.MatchString(id) {
		return PaymentOrder{}, ErrInvalid
	}
	tx, err := s.billingReadTx(ctx, who)
	if err != nil {
		return PaymentOrder{}, err
	}
	defer tx.Rollback(ctx)
	o, err := scanPayment(tx.QueryRow(ctx, "SELECT "+paymentColumns+" FROM payment_orders WHERE id=$1 AND tenant_id=$2", id, who.TenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return PaymentOrder{}, ErrNotFound
	}
	return o, err
}

// PlanPaymentOrders returns recent direct-package orders for recovery when the
// browser no longer has its sessionStorage entry.
func (s *Store) PlanPaymentOrders(ctx context.Context, who Identity) ([]PaymentOrder, error) {
	tx, err := s.billingReadTx(ctx, who)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, "SELECT "+paymentColumns+" FROM payment_orders WHERE tenant_id=$1 AND purpose='plan-purchase' ORDER BY created_at DESC,id DESC LIMIT 20", who.TenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	orders := []PaymentOrder{}
	for rows.Next() {
		o, scanErr := scanPayment(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		orders = append(orders, o)
	}
	return orders, rows.Err()
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
	isTerminal := e.Type == "checkout.session.expired" || e.Type == "checkout.session.async_payment_failed"
	isRefund := strings.HasPrefix(e.Type, "refund.") || e.Type == "charge.refunded" || e.Type == "charge.refund.updated"
	isDispute := strings.HasPrefix(e.Type, "charge.dispute.")
	if !isPaid && !isTerminal && !isRefund && !isDispute {
		return tx.Commit(ctx)
	}
	if e.Account != "" {
		if err = paymentReview(ctx, tx, nil, e, "connected-account-event"); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	var order PaymentOrder
	if isPaid || isTerminal {
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
		err = s.paymentCredit(ctx, tx, order, e)
	} else if isTerminal {
		err = paymentTerminal(ctx, tx, order, e)
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

func (s *Store) paymentCredit(ctx context.Context, tx pgx.Tx, o PaymentOrder, e payments.Event) error {
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
	var prepared *preparedPlanPurchase
	failure := ""
	if o.Purpose == "plan-purchase" {
		buyer, buyerErr := paymentPlanBuyer(ctx, tx, o)
		if buyerErr != nil {
			if errors.Is(buyerErr, ErrUnauthorized) || errors.Is(buyerErr, ErrForbidden) {
				failure = "unauthorized"
			} else {
				return buyerErr
			}
		} else {
			candidate, prepareErr := s.preparePlanPurchase(ctx, tx, buyer, "plan-payment:"+o.ID, PurchasePlanInput{RuleID: o.RuleID, PriceVersionID: o.PriceVersionID})
			if prepareErr != nil {
				var known bool
				failure, known = planPaymentBusinessError(prepareErr)
				if !known {
					return prepareErr
				}
			} else {
				prepared = &candidate
			}
		}
	}
	balance, frozen, err := paymentWalletLockState(ctx, tx, o.TenantID)
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
	if o.Purpose == "plan-purchase" {
		if failure == "" && frozen {
			failure = "wallet-frozen"
		}
		if failure == "" && balance+o.AmountCents < prepared.plan.PriceCents {
			failure = "insufficient-balance"
		}
		if failure != "" {
			if _, err = tx.Exec(ctx, "UPDATE payment_orders SET state='credited',payment_intent=$2,credit_ledger_id=$3,purchase_state='failed',purchase_error=$4 WHERE id=$1", o.ID, obj.PaymentIntent, ledger, failure); err != nil {
				return err
			}
			if err = paymentAudit(ctx, tx, "payment.plan-failed", o.ID, e.ID); err != nil {
				return err
			}
			return paymentAudit(ctx, tx, "payment.credit", o.ID, e.ID)
		}
		var subscription RuleSubscription
		if prepared.existing != nil {
			subscription = *prepared.existing
		} else {
			subscription, err = s.finishPlanPurchase(ctx, tx, *prepared, balance+o.AmountCents)
			if err != nil {
				return err
			}
		}
		if _, err = tx.Exec(ctx, "UPDATE payment_orders SET state='credited',payment_intent=$2,credit_ledger_id=$3,purchase_state='purchased',purchase_error='',subscription_id=$4 WHERE id=$1", o.ID, obj.PaymentIntent, ledger, subscription.ID); err != nil {
			return err
		}
		if err = paymentAudit(ctx, tx, "payment.plan-purchased", o.ID, e.ID); err != nil {
			return err
		}
		return paymentAudit(ctx, tx, "payment.credit", o.ID, e.ID)
	}
	if _, err = tx.Exec(ctx, "UPDATE payment_orders SET state='credited',payment_intent=$2,credit_ledger_id=$3 WHERE id=$1", o.ID, obj.PaymentIntent, ledger); err != nil {
		return err
	}
	return paymentAudit(ctx, tx, "payment.credit", o.ID, e.ID)
}

func paymentPlanBuyer(ctx context.Context, tx pgx.Tx, o PaymentOrder) (Identity, error) {
	var who Identity
	err := tx.QueryRow(ctx, `SELECT m.id::text,m.tenant_id::text,m.email,m.role,t.name FROM members m JOIN tenants t ON t.id=m.tenant_id WHERE m.id=$1 AND m.tenant_id=$2 AND t.status='active' FOR SHARE OF m,t`, o.CreatedBy, o.TenantID).Scan(&who.MemberID, &who.TenantID, &who.Email, &who.Role, &who.Company)
	if errors.Is(err, pgx.ErrNoRows) {
		return Identity{}, ErrUnauthorized
	}
	if err != nil {
		return Identity{}, err
	}
	if !canWrite(who) {
		return Identity{}, ErrForbidden
	}
	return who, nil
}

func planPaymentBusinessError(err error) (string, bool) {
	switch {
	case errors.Is(err, errPlanSaleUnavailable), errors.Is(err, ErrNodeNotAllocatable), errors.Is(err, ErrNotFound), errors.Is(err, ErrInvalid):
		return "unavailable", true
	case errors.Is(err, ErrConflict):
		return "conflict", true
	case errors.Is(err, ErrInsufficientBalance):
		return "insufficient-balance", true
	case errors.Is(err, ErrForbidden):
		return "wallet-frozen", true
	default:
		return "", false
	}
}

func paymentTerminal(ctx context.Context, tx pgx.Tx, o PaymentOrder, e payments.Event) error {
	obj := e.Data.Object
	if obj.Object != "checkout.session" || obj.ID != o.SessionID || !paymentMetadata(obj.Metadata, o) {
		return paymentReview(ctx, tx, &o, e, "checkout-terminal-mismatch")
	}
	if o.State != "pending" {
		return nil
	}
	state := "failed"
	purchaseError := "payment-failed"
	if e.Type == "checkout.session.expired" {
		state = "expired"
		purchaseError = "payment-expired"
	}
	if o.Purpose == "plan-purchase" {
		if _, err := tx.Exec(ctx, "UPDATE payment_orders SET state=$2,purchase_state='failed',purchase_error=$3 WHERE id=$1", o.ID, state, purchaseError); err != nil {
			return err
		}
	} else if _, err := tx.Exec(ctx, "UPDATE payment_orders SET state=$2 WHERE id=$1", o.ID, state); err != nil {
		return err
	}
	return paymentAudit(ctx, tx, "payment."+state, o.ID, e.ID)
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
	balance, _, err := paymentWalletLockState(ctx, tx, tenant)
	return balance, err
}

func paymentWalletLockState(ctx context.Context, tx pgx.Tx, tenant string) (int64, bool, error) {
	if _, err := tx.Exec(ctx, "INSERT INTO billing_wallets(tenant_id) VALUES($1) ON CONFLICT DO NOTHING", tenant); err != nil {
		return 0, false, err
	}
	var balance int64
	var frozen bool
	err := tx.QueryRow(ctx, "SELECT balance_cents,spending_frozen FROM billing_wallets WHERE tenant_id=$1 FOR UPDATE", tenant).Scan(&balance, &frozen)
	return balance, frozen, err
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
