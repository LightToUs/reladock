package web

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"reladock/internal/platform"
)

func subscriptionHTTPStore(t *testing.T) *platform.Store {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	schema := "subs_web_" + strings.ReplaceAll(time.Now().Format("150405.000000000"), ".", "")
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); admin.Close() })
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	q.Set("search_path", schema)
	parsed.RawQuery = q.Encode()
	store, err := platform.Open(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	pool, err := pgxpool.New(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, migration := range []struct{ table, file string }{{"billing_wallets", "migration5.sql"}, {"plan_price_versions", "migration7.sql"}, {"subscription_usage", "migration10.sql"}} {
		var exists bool
		if err = pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", migration.table).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			sql, e := os.ReadFile("../platform/" + migration.file)
			if e != nil {
				t.Fatal(e)
			}
			if _, err = pool.Exec(ctx, string(sql)); err != nil {
				t.Fatal(err)
			}
		}
	}
	return store
}

func TestSubscriptionsHTTPConfiguredPurchaseAuthorizationAndStrictInput(t *testing.T) {
	store := subscriptionHTTPStore(t)
	ctx := context.Background()
	admin, _, err := store.Register(ctx, "Operator", "subscription-http-admin@example.com", "long-test-password-42")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SetPlatformAdmin(ctx, admin.Email, true); err != nil {
		t.Fatal(err)
	}
	admin, _, err = store.Login(ctx, admin.Email, "long-test-password-42")
	if err != nil {
		t.Fatal(err)
	}
	who, _, err := store.Register(ctx, "Subscriber", "subscription-http@example.com", "long-test-password-42")
	if err != nil {
		t.Fatal(err)
	}
	foreign, _, err := store.Register(ctx, "Other", "subscription-http-other@example.com", "long-test-password-42")
	if err != nil {
		t.Fatal(err)
	}
	rule, err := store.CreateRule(ctx, who, "subscription-http-rule", platform.RuleInput{Name: "relay", NodeID: "hk-01", Protocol: "tcp", TargetHost: "192.0.2.10", TargetPort: 443, BandwidthMbps: 1000})
	if err != nil {
		t.Fatal(err)
	}
	s := &server{store: store}
	call := func(method, path, body, key string, actor platform.Identity) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		if !s.serveSubscriptions(w, r, actor) {
			t.Fatalf("unhandled: %s", path)
		}
		return w
	}
	body := `{"code":"per-rule","version":1,"name":"Explicit plan","priceCents":25,"quotaBytes":321,"bandwidthMbps":50,"periodSeconds":86400}`
	if w := call("POST", "/api/admin/plans", body, "", who); w.Code != 403 {
		t.Fatalf("owner plan creation: %d %s", w.Code, w.Body.String())
	}
	created := call("POST", "/api/admin/plans", body, "", admin)
	var plan platform.PlanPriceVersion
	if err = json.Unmarshal(created.Body.Bytes(), &plan); err != nil || created.Code != 201 || plan.QuotaBytes != 321 || plan.PeriodSeconds != 86400 {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	for _, actor := range []platform.Identity{who, admin} {
		if w := call("GET", "/api/plans", "", "", actor); w.Code != 200 || !strings.Contains(w.Body.String(), plan.ID) {
			t.Fatalf("catalog: %d %s", w.Code, w.Body.String())
		}
	}
	if w := call("GET", "/api/admin/plans", "", "", who); w.Code != 403 {
		t.Fatalf("owner admin listing: %d", w.Code)
	}
	if w := call("GET", "/api/plans", "", "", platform.Identity{}); w.Code != 401 {
		t.Fatalf("anonymous plans: %d", w.Code)
	}
	purchase := `{"ruleId":"` + rule.ID + `","priceVersionId":"` + plan.ID + `"}`
	if w := call("POST", "/api/subscriptions/purchase", purchase, "purchase-no-funds", who); w.Code != 409 {
		t.Fatalf("insufficient: %d %s", w.Code, w.Body.String())
	}
	if _, err = store.AdjustWalletFromCLI(ctx, platform.WalletAdjustment{TenantID: who.TenantID, Key: "http-subscription-funds", AmountCents: 100, Reason: "local correction", Operator: "test"}); err != nil {
		t.Fatal(err)
	}
	created = call("POST", "/api/subscriptions/purchase", purchase, "purchase-http-once", who)
	var period platform.RuleSubscription
	if err = json.Unmarshal(created.Body.Bytes(), &period); err != nil || created.Code != 201 || period.RemainingBytes != 321 || period.UsedBytes != 0 {
		t.Fatalf("purchase: %d %s", created.Code, created.Body.String())
	}
	replay := call("POST", "/api/subscriptions/purchase", purchase, "purchase-http-once", who)
	var duplicate platform.RuleSubscription
	if err = json.Unmarshal(replay.Body.Bytes(), &duplicate); err != nil || duplicate.ID != period.ID || replay.Code != 201 {
		t.Fatalf("replay: %d %s", replay.Code, replay.Body.String())
	}
	if w := call("GET", "/api/subscriptions?ruleId="+rule.ID, "", "", who); w.Code != 200 || !strings.Contains(w.Body.String(), period.ID) {
		t.Fatalf("history: %d %s", w.Code, w.Body.String())
	}
	if w := call("GET", "/api/subscriptions?ruleId="+rule.ID, "", "", foreign); w.Code != 404 {
		t.Fatalf("foreign history: %d", w.Code)
	}
	if w := call("POST", "/api/subscriptions/purchase", purchase, "foreign-purchase", foreign); w.Code != 404 {
		t.Fatalf("foreign purchase: %d", w.Code)
	}
	for _, tc := range []struct{ method, path, body, key string }{
		{"POST", "/api/subscriptions/purchase", purchase, ""},
		{"POST", "/api/subscriptions/purchase", strings.TrimSuffix(purchase, "}") + `,"tenantId":"` + foreign.TenantID + `"}`, "forged-tenant"},
		{"POST", "/api/admin/plans", strings.TrimSuffix(body, "}") + `,"currency":"EUR"}`, ""},
		{"GET", "/api/plans?page=0", "", ""},
		{"GET", "/api/subscriptions", "", ""},
		{"GET", "/api/subscriptions?ruleId=" + rule.ID + "&before=-1", "", ""},
		{"GET", "/api/subscriptions?ruleId=" + rule.ID + "&tenantId=" + foreign.TenantID, "", ""},
	} {
		actor := who
		if tc.path == "/api/admin/plans" {
			actor = admin
		}
		if w := call(tc.method, tc.path, tc.body, tc.key, actor); w.Code != 400 {
			t.Fatalf("invalid accepted: %+v %d %s", tc, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	if s.serveSubscriptions(w, httptest.NewRequest("GET", "/api/other", nil), who) {
		t.Fatal("claimed other route")
	}
}
