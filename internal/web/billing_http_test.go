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

// The route helper is tested directly until the parent wires server.go. Auth is
// resolved using real sessions here; the production server already owns that step.
func billingHTTPStore(t *testing.T) *platform.Store {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	schema := "billing_web_" + strings.ReplaceAll(time.Now().Format("150405.000000000"), ".", "")
	if _, err = pool.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		pool.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); pool.Close() })
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
	migrationPool, err := pgxpool.New(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	defer migrationPool.Close()
	var exists bool
	if err = migrationPool.QueryRow(ctx, "SELECT to_regclass('billing_wallets') IS NOT NULL").Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		sql, e := os.ReadFile("../platform/migration5.sql")
		if e != nil {
			t.Fatal(e)
		}
		if _, err = migrationPool.Exec(ctx, string(sql)); err != nil {
			t.Fatal(err)
		}
	}
	if err = migrationPool.QueryRow(ctx, "SELECT to_regclass('subscription_usage') IS NOT NULL").Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		sql, e := os.ReadFile("../platform/migration10.sql")
		if e != nil {
			t.Fatal(e)
		}
		if _, err = migrationPool.Exec(ctx, string(sql)); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

func TestBillingHTTPReadOnlyTenantScopeAndUnconnectedPayments(t *testing.T) {
	store := billingHTTPStore(t)
	ctx := context.Background()
	a, token, err := store.Register(ctx, "A", "billing-http-a@example.com", "long-test-password-42")
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := store.Register(ctx, "B", "billing-http-b@example.com", "long-test-password-42")
	if err != nil {
		t.Fatal(err)
	}
	rule, err := store.CreateRule(ctx, a, "billing-http-rule", platform.RuleInput{Name: "relay", Protocol: "tcp", TargetHost: "192.0.2.10", TargetPort: 443, BandwidthMbps: 1000, NodeID: "hk-01"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.AdjustWalletFromCLI(ctx, platform.WalletAdjustment{TenantID: a.TenantID, RuleID: rule.ID, Key: "http-local-credit", AmountCents: 123, Reason: "manual correction", Operator: "operator"}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ReportUsage(ctx, "hk-01", platform.UsageReport{RuleID: rule.ID, Epoch: "http-epoch-1", Sequence: 1, UploadBytes: 7, DownloadBytes: 11}); err != nil {
		t.Fatal(err)
	}
	s := &server{store: store}
	// Keep the callback focused on the extension point, not a duplicate router.
	call := func(method, path string, who platform.Identity) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, nil)
		w := httptest.NewRecorder()
		if !s.serveBilling(w, r, who) {
			t.Fatalf("unhandled billing route: %s", path)
		}
		return w
	}
	for _, path := range []string{"/api/billing", "/api/billing/wallet", "/api/billing/ledger", "/api/billing/usage?ruleId=" + rule.ID} {
		if w := call("GET", path, a); w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	w := call("GET", "/api/billing", a)
	var status struct {
		Payments string `json:"payments"`
		Pricing  string `json:"pricing"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &status); err != nil || status.Payments != "not-connected" || status.Pricing != "plans-supported" {
		t.Fatalf("invented readiness: %s", w.Body.String())
	}
	w = call("GET", "/api/billing/usage?ruleId="+rule.ID, a)
	var usage platform.RuleUsage
	if err = json.Unmarshal(w.Body.Bytes(), &usage); err != nil || usage.TotalBytes != 18 || !usage.HasReports {
		t.Fatalf("usage: %s", w.Body.String())
	}
	if w = call("GET", "/api/billing/usage?ruleId="+rule.ID, b); w.Code != 404 {
		t.Fatalf("foreign usage: %d", w.Code)
	}
	if w = call("GET", "/api/billing/ledger?ruleId="+rule.ID, b); w.Code != 404 {
		t.Fatalf("foreign ledger: %d", w.Code)
	}
	w = call("GET", "/api/billing/wallet", b)
	if strings.Contains(w.Body.String(), "123") || !strings.Contains(w.Body.String(), `"initialized":false`) {
		t.Fatalf("foreign wallet: %s", w.Body.String())
	}
	for _, path := range []string{"/api/billing", "/api/billing/wallet", "/api/billing/ledger", "/api/billing/usage?ruleId=" + rule.ID} {
		if w = call("GET", path, platform.Identity{}); w.Code != 401 {
			t.Fatalf("anonymous %s: %d", path, w.Code)
		}
	}
	for _, path := range []string{"/api/billing/wallet", "/api/billing/ledger", "/api/billing/topup", "/api/billing/usage", "/api/billing/payments"} {
		w = call("POST", path, a)
		if w.Code != 405 && w.Code != 404 {
			t.Fatalf("write exposed: %s %d", path, w.Code)
		}
	}
	for _, path := range []string{"/api/billing/usage", "/api/billing/usage?ruleId=bad", "/api/billing/ledger?before=-1", "/api/billing/ledger?before=9223372036854775808", "/api/billing/wallet?tenantId=" + b.TenantID, "/api/billing/usage?ruleId=" + rule.ID + "&ruleId=" + rule.ID} {
		if w = call("GET", path, a); w.Code != 400 {
			t.Fatalf("bad query accepted: %s %d", path, w.Code)
		}
	}
	w = httptest.NewRecorder()
	if s.serveBilling(w, httptest.NewRequest("GET", "/api/elsewhere", nil), a) {
		t.Fatal("claimed another handler's route")
	}
	// Verify revoked sessions cannot be passed by an eventual router.
	if err = store.Logout(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Session(ctx, token); err != platform.ErrUnauthorized {
		t.Fatalf("session: %v", err)
	}
}
