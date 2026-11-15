package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"reladock/internal/payments"
	"reladock/internal/platform"
)

func TestPaymentHTTPDisabledSignaturesAndLimits(t *testing.T) {
	disabled := NewPayments(nil, nil, "")
	w := httptest.NewRecorder()
	disabled.Webhook(w, httptest.NewRequest("POST", "/", nil))
	if w.Code != 503 {
		t.Fatalf("disabled %d", w.Code)
	}
	p := NewPayments(nil, httpPaymentFake{}, "test-secret")
	for _, tc := range []struct {
		method, body, sig string
		want              int
	}{{"GET", "", "", 405}, {"POST", "{}", "bad", 400}, {"POST", strings.Repeat("x", payments.MaxWebhookBytes+1), "bad", 413}} {
		r := httptest.NewRequest(tc.method, "/", strings.NewReader(tc.body))
		r.Header.Set("Stripe-Signature", tc.sig)
		w := httptest.NewRecorder()
		p.Webhook(w, r)
		if w.Code != tc.want {
			t.Fatalf("got %d want %d", w.Code, tc.want)
		}
	}
	for _, tc := range []struct {
		origin, marker, site string
		who                  platform.Identity
		want                 int
	}{
		{"http://127.0.0.1:3001", "RelaDock", "", platform.Identity{}, 401},
		{"https://evil.example", "RelaDock", "", platform.Identity{TenantID: "a", MemberID: "b"}, 403},
		{"http://127.0.0.1:3001", "", "", platform.Identity{TenantID: "a", MemberID: "b"}, 403},
		{"http://127.0.0.1:3001", "RelaDock", "cross-site", platform.Identity{TenantID: "a", MemberID: "b"}, 403},
	} {
		r := httptest.NewRequest("POST", "http://127.0.0.1:3001/api/payments/checkout", strings.NewReader(`{"amountCents":500}`))
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("X-Requested-With", tc.marker)
		r.Header.Set("Sec-Fetch-Site", tc.site)
		w := httptest.NewRecorder()
		p.Checkout(w, r, tc.who)
		if w.Code != tc.want {
			t.Fatalf("checkout got %d want %d", w.Code, tc.want)
		}
	}
}

func TestPaymentWebhookIndependentBudgetModeAndConnect(t *testing.T) {
	p := NewPayments(nil, httpPaymentFake{}, "test-secret")
	for i := 0; i < 121; i++ {
		r := httptest.NewRequest("POST", "/", strings.NewReader("{}"))
		r.RemoteAddr = "192.0.2.1:1234"
		w := httptest.NewRecorder()
		p.Webhook(w, r)
		want := 400
		if i == 120 {
			want = 429
		}
		if w.Code != want {
			t.Fatalf("request %d code=%d want=%d", i, w.Code, want)
		}
	}
	for _, body := range []string{`{"id":"evt_live","type":"checkout.session.completed","livemode":true,"data":{"object":{}}}`, `{"id":"evt_connect","type":"checkout.session.completed","account":"acct_other","data":{"object":{}}}`} {
		stamp := time.Now().Unix()
		h := hmac.New(sha256.New, []byte("test-secret"))
		fmt.Fprintf(h, "%d.%s", stamp, body)
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		r.RemoteAddr = "192.0.2.2:3456"
		r.Header.Set("Stripe-Signature", fmt.Sprintf("t=%d,v1=%x", stamp, h.Sum(nil)))
		w := httptest.NewRecorder()
		p.Webhook(w, r)
		if w.Code != 400 {
			t.Fatalf("unsupported account/mode %d", w.Code)
		}
	}
}

func TestPaymentWebhookRejectsTestEventForLiveProvider(t *testing.T) {
	provider, e := payments.NewStripe(payments.Config{SecretKey: "sk_live_dummy", WebhookSecret: "whsec_dummy", SuccessURL: "https://example.com/return", CancelURL: "https://example.com/cancel"})
	if e != nil {
		t.Fatal(e)
	}
	p := NewPayments(nil, provider, "whsec_dummy", "https://example.com")
	body := `{"id":"evt_test_mode","type":"checkout.session.completed","livemode":false,"data":{"object":{}}}`
	stamp := time.Now().Unix()
	h := hmac.New(sha256.New, []byte("whsec_dummy"))
	fmt.Fprintf(h, "%d.%s", stamp, body)
	r := httptest.NewRequest("POST", "https://example.com/api/payments/stripe/webhook", strings.NewReader(body))
	r.Header.Set("Stripe-Signature", fmt.Sprintf("t=%d,v1=%x", stamp, h.Sum(nil)))
	w := httptest.NewRecorder()
	p.Webhook(w, r)
	if w.Code != 400 {
		t.Fatalf("test event accepted by live provider: %d", w.Code)
	}
}

type httpPaymentFake struct{}

func (httpPaymentFake) CreateCheckout(_ context.Context, in payments.CheckoutRequest) (payments.Session, error) {
	return payments.Session{ID: "cs_" + in.OrderID, URL: "https://checkout.stripe.com/c/pay/test", AmountTotal: in.AmountCents, Currency: "usd", Metadata: map[string]string{"order_id": in.OrderID, "tenant_id": in.TenantID, "amount_cents": fmt.Sprint(in.AmountCents), "currency": "usd"}}, nil
}
func paymentHTTPStore(t *testing.T) *platform.Store {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("TEST_DATABASE_URL required")
	}
	ctx := context.Background()
	pool, e := pgxpool.New(ctx, raw)
	if e != nil {
		t.Fatal(e)
	}
	schema := "payments_web_" + strings.ReplaceAll(time.Now().Format("150405.000000000"), ".", "")
	if _, e = pool.Exec(ctx, "CREATE SCHEMA "+schema); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { pool.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); pool.Close() })
	u, e := url.Parse(raw)
	if e != nil {
		t.Fatal(e)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	s, e := platform.Open(ctx, u.String())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(s.Close)
	migration, e := pgxpool.New(ctx, u.String())
	if e != nil {
		t.Fatal(e)
	}
	defer migration.Close()
	for _, m := range []struct{ table, file string }{{"plan_price_versions", "../platform/migration7.sql"}, {"payment_orders", "../platform/migration9.sql"}} {
		var exists bool
		if e = migration.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", m.table).Scan(&exists); e != nil {
			t.Fatal(e)
		}
		if !exists {
			data, e := os.ReadFile(m.file)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = migration.Exec(ctx, string(data)); e != nil {
				t.Fatal(e)
			}
		}
	}
	if _, e = migration.Exec(ctx, platform.PaymentFreezeMigrationSQL); e != nil {
		t.Fatal(e)
	}
	return s
}
func TestPaymentHTTPCheckoutAndSignedWebhookEndToEnd(t *testing.T) {
	s := paymentHTTPStore(t)
	ctx := context.Background()
	who, token, e := s.Register(ctx, "Payments", "payments-http@example.com", "long-test-password-42")
	if e != nil {
		t.Fatal(e)
	}
	p := NewPayments(s, httpPaymentFake{}, "test-secret")
	handler := NewWithOptions(s, fstest.MapFS{}, "http://127.0.0.1:3001", Options{Payments: p})
	status := request(handler, "GET", "/api/payments/status", "", &http.Cookie{Name: cookieName, Value: token})
	if status.Code != 200 || !strings.Contains(status.Body.String(), `"enabled":true`) {
		t.Fatalf("payment status %d %s", status.Code, status.Body.String())
	}
	checkout := func(body, key string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "http://127.0.0.1:3001/api/payments/checkout", strings.NewReader(body))
		r.AddCookie(&http.Cookie{Name: cookieName, Value: token})
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "http://127.0.0.1:3001")
		r.Header.Set("X-Requested-With", "RelaDock")
		r.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	for _, tc := range []struct {
		body, key string
		want      int
	}{{`{"amountCents":500}`, "", 400}, {`{"amountCents":5.5}`, "payment-test", 400}, {`{"amountCents":500,"tenantId":"foreign"}`, "payment-test", 400}} {
		if w := checkout(tc.body, tc.key); w.Code != tc.want {
			t.Fatalf("bad input %d %s", w.Code, w.Body.String())
		}
	}
	w := checkout(`{"amountCents":500}`, "payment-http")
	if w.Code != 201 {
		t.Fatalf("checkout %d %s", w.Code, w.Body.String())
	}
	var order platform.PaymentOrder
	if e = json.Unmarshal(w.Body.Bytes(), &order); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(w.Body.String(), "test-secret") || strings.Contains(w.Body.String(), "tenantId") || order.State != "pending" {
		t.Fatalf("checkout response %s", w.Body.String())
	}
	if replay := checkout(`{"amountCents":500}`, "payment-http"); !strings.Contains(replay.Body.String(), order.ID) {
		t.Fatal("checkout replay")
	}
	wallet, e := s.Wallet(ctx, who)
	if e != nil || wallet.BalanceCents != 0 {
		t.Fatal("checkout credited wallet")
	}
	body := fmt.Sprintf(`{"id":"evt_http_paid","type":"checkout.session.completed","data":{"object":{"object":"checkout.session","id":"cs_%s","mode":"payment","status":"complete","payment_status":"paid","payment_intent":"pi_http","currency":"usd","amount_total":500,"metadata":{"order_id":"%s","tenant_id":"%s","amount_cents":"500","currency":"usd"}}}}`, order.ID, order.ID, who.TenantID)
	deliver := func(body string) *httptest.ResponseRecorder {
		stamp := time.Now().Unix()
		mac := hmac.New(sha256.New, []byte("test-secret"))
		fmt.Fprintf(mac, "%d.%s", stamp, body)
		r := httptest.NewRequest("POST", "http://127.0.0.1:3001/api/payments/stripe/webhook", strings.NewReader(body))
		r.Header.Set("Stripe-Signature", fmt.Sprintf("t=%d,v1=%x", stamp, mac.Sum(nil)))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		return rec
	}
	for i := 0; i < 2; i++ {
		if rec := deliver(body); rec.Code != 200 {
			t.Fatalf("webhook %d %s", rec.Code, rec.Body.String())
		}
	}
	wallet, e = s.Wallet(ctx, who)
	if e != nil || wallet.BalanceCents != 500 {
		t.Fatalf("paid wallet %+v %v", wallet, e)
	}
	if rec := deliver(strings.Replace(body, `"amount_total":500`, `"amount_total":501`, 1)); rec.Code != 409 {
		t.Fatalf("conflicting replay %d", rec.Code)
	}
	dispute := `{"id":"evt_http_dispute","type":"charge.dispute.created","data":{"object":{"id":"du_http","object":"dispute","payment_intent":"pi_http","currency":"usd","amount":100}}}`
	for _, invalid := range []string{strings.Replace(dispute, `"type":`, `"account":"acct_other","type":`, 1), strings.Replace(dispute, `"type":`, `"livemode":true,"type":`, 1)} {
		if rec := deliver(invalid); rec.Code != 400 {
			t.Fatalf("unsupported dispute account/mode %d %s", rec.Code, rec.Body.String())
		}
		wallet, e = s.Wallet(ctx, who)
		if e != nil || wallet.SpendingFrozen {
			t.Fatalf("rejected dispute changed wallet %+v %v", wallet, e)
		}
	}
	for i := 0; i < 2; i++ {
		if rec := deliver(dispute); rec.Code != 200 {
			t.Fatalf("dispute webhook %d %s", rec.Code, rec.Body.String())
		}
		wallet, e = s.Wallet(ctx, who)
		if e != nil || !wallet.SpendingFrozen || wallet.BalanceCents != 500 {
			t.Fatalf("dispute success before freeze commit %+v %v", wallet, e)
		}
	}
	ledger, e := s.WalletLedger(ctx, who, "", 0)
	if e != nil || len(ledger) != 1 {
		t.Fatalf("dispute mutated ledger: %d %v", len(ledger), e)
	}
	if e = s.Logout(ctx, token); e != nil {
		t.Fatal(e)
	}
	if rec := checkout(`{"amountCents":500}`, "new-payment"); rec.Code != 401 {
		t.Fatalf("revoked checkout %d", rec.Code)
	}
}

func TestPlanPaymentHTTPCheckoutRecoveryAndTenantReads(t *testing.T) {
	s, pool := subscriptionHTTPStore(t)
	ctx := context.Background()
	admin, _, err := s.Register(ctx, "Plan Operator", "plan-http-admin@example.com", "long-test-password-42")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetPlatformAdmin(ctx, admin.Email, true); err != nil {
		t.Fatal(err)
	}
	admin, _, err = s.Login(ctx, admin.Email, "long-test-password-42")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, "UPDATE nodes SET status='ready' WHERE id='hk-01'"); err != nil {
		t.Fatal(err)
	}
	offering, err := s.AdminCreateOffering(ctx, admin, platform.LineOfferingInput{Code: "plan-pay-http", Name: "Plan payment", NodeID: "hk-01"})
	if err != nil {
		t.Fatal(err)
	}
	offering, err = s.AdminSetOfferingState(ctx, admin, offering.ID, platform.OfferingStateInput{ExpectedRevision: offering.Revision, State: platform.OfferingStatePublished})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := s.AdminCreatePlan(ctx, admin, platform.PlanInput{OfferingID: offering.ID, Code: "plan-pay-http", Name: "Direct package", PriceCents: 1999, QuotaBytes: 5_000_000, BandwidthMbps: 100, PeriodSeconds: 86400, ResourceMode: platform.ResourceModeShared})
	if err != nil {
		t.Fatal(err)
	}
	who, token, err := s.Register(ctx, "Plan Buyer", "plan-http@example.com", "long-test-password-42")
	if err != nil {
		t.Fatal(err)
	}
	_, foreignToken, err := s.Register(ctx, "Other Buyer", "plan-http-other@example.com", "long-test-password-42")
	if err != nil {
		t.Fatal(err)
	}
	p := NewPayments(s, httpPaymentFake{}, "test-secret", "http://127.0.0.1:3001")
	handler := NewWithOptions(s, fstest.MapFS{}, "http://127.0.0.1:3001", Options{Payments: p})
	cookie := &http.Cookie{Name: cookieName, Value: token}
	foreignCookie := &http.Cookie{Name: cookieName, Value: foreignToken}
	call := func(method, path, body, key string, actor *http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "http://127.0.0.1:3001"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "http://127.0.0.1:3001")
		r.Header.Set("X-Requested-With", "RelaDock")
		r.Header.Set("Idempotency-Key", key)
		if actor != nil {
			r.AddCookie(actor)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	body := `{"priceVersionId":"` + plan.ID + `"}`
	created := call(http.MethodPost, "/api/payments/plan-checkout", body, "plan-http-checkout-a", cookie)
	if created.Code != http.StatusCreated {
		t.Fatalf("plan checkout: %d %s", created.Code, created.Body.String())
	}
	var order platform.PaymentOrder
	if err = json.Unmarshal(created.Body.Bytes(), &order); err != nil {
		t.Fatal(err)
	}
	if order.AmountCents != 1999 || order.Purpose != "plan-purchase" || order.PriceVersionID != plan.ID || order.CheckoutURL == "" || strings.Contains(created.Body.String(), "tenantId") || strings.Contains(created.Body.String(), "sessionId") || strings.Contains(created.Body.String(), "createdBy") {
		t.Fatalf("plan checkout response: %s", created.Body.String())
	}
	recovered := call(http.MethodPost, "/api/payments/plan-checkout", body, "plan-http-checkout-b", cookie)
	if recovered.Code != http.StatusCreated || !strings.Contains(recovered.Body.String(), order.ID) {
		t.Fatalf("selection recovery: %d %s", recovered.Code, recovered.Body.String())
	}
	if exact := call(http.MethodGet, "/api/payments/orders/"+order.ID, "", "", cookie); exact.Code != http.StatusOK || !strings.Contains(exact.Body.String(), order.ID) {
		t.Fatalf("exact order: %d %s", exact.Code, exact.Body.String())
	}
	if exact := call(http.MethodGet, "/api/payments/orders/"+order.ID, "", "", foreignCookie); exact.Code != http.StatusNotFound {
		t.Fatalf("foreign exact: %d %s", exact.Code, exact.Body.String())
	}
	if recent := call(http.MethodGet, "/api/payments/orders?purpose=plan-purchase", "", "", cookie); recent.Code != http.StatusOK || !strings.Contains(recent.Body.String(), order.ID) {
		t.Fatalf("recent orders: %d %s", recent.Code, recent.Body.String())
	}
	if recent := call(http.MethodGet, "/api/payments/orders?purpose=plan-purchase", "", "", foreignCookie); recent.Code != http.StatusOK || strings.Contains(recent.Body.String(), order.ID) {
		t.Fatalf("foreign recent: %d %s", recent.Code, recent.Body.String())
	}

	for name, response := range map[string]*httptest.ResponseRecorder{
		"unknown body":    call(http.MethodPost, "/api/payments/plan-checkout", strings.TrimSuffix(body, "}")+`,"amountCents":1}`, "plan-http-invalid-a", cookie),
		"checkout query":  call(http.MethodPost, "/api/payments/plan-checkout?x=1", body, "plan-http-invalid-b", cookie),
		"checkout method": call(http.MethodGet, "/api/payments/plan-checkout", "", "", cookie),
		"missing purpose": call(http.MethodGet, "/api/payments/orders", "", "", cookie),
		"extra purpose":   call(http.MethodGet, "/api/payments/orders?purpose=plan-purchase&x=1", "", "", cookie),
	} {
		if response.Code != http.StatusBadRequest && response.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s accepted: %d %s", name, response.Code, response.Body.String())
		}
	}

	sufficient, sufficientToken, err := s.Register(ctx, "Funded Buyer", "plan-http-funded@example.com", "long-test-password-42")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdjustWalletFromCLI(ctx, platform.WalletAdjustment{TenantID: sufficient.TenantID, Key: "plan-http-sufficient", AmountCents: 1999, Reason: "test", Operator: "test"}); err != nil {
		t.Fatal(err)
	}
	if response := call(http.MethodPost, "/api/payments/plan-checkout", body, "plan-http-funded", &http.Cookie{Name: cookieName, Value: sufficientToken}); response.Code != http.StatusConflict {
		t.Fatalf("sufficient wallet: %d %s", response.Code, response.Body.String())
	}
	disabled := NewWithOptions(s, fstest.MapFS{}, "http://127.0.0.1:3001", Options{})
	if response := request(disabled, http.MethodPost, "/api/payments/plan-checkout", body, cookie); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled connector: %d %s", response.Code, response.Body.String())
	}

	// The ordinary arbitrary wallet top-up endpoint remains compatible.
	topup := call(http.MethodPost, "/api/payments/checkout", `{"amountCents":500}`, "plan-http-topup", cookie)
	if topup.Code != http.StatusCreated || !strings.Contains(topup.Body.String(), `"purpose":"wallet-topup"`) {
		t.Fatalf("top-up compatibility: %d %s", topup.Code, topup.Body.String())
	}
	_ = who
}
