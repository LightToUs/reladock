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

func subscriptionHTTPStore(t *testing.T) (*platform.Store, *pgxpool.Pool) {
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
	t.Cleanup(pool.Close)
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
	return store, pool
}

func TestSubscriptionsHTTPConfiguredPurchaseAuthorizationAndStrictInput(t *testing.T) {
	store, pool := subscriptionHTTPStore(t)
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
	offeringBody := `{"code":"hk-http","name":"香港优化线路","nodeId":"hk-01","sourceMarket":"中国大陆","destinationMarket":"香港 / 亚洲","notes":"HTTP 线路备注"}`
	if w := call("POST", "/api/admin/offerings", offeringBody, "", who); w.Code != 403 {
		t.Fatalf("owner offering creation: %d %s", w.Code, w.Body.String())
	}
	createdOffering := call("POST", "/api/admin/offerings", offeringBody, "", admin)
	var offering platform.LineOffering
	if err = json.Unmarshal(createdOffering.Body.Bytes(), &offering); err != nil || createdOffering.Code != 201 || offering.State != platform.OfferingStateDraft || offering.NodeID != "hk-01" || offering.Notes != "HTTP 线路备注" || !offering.Deletable {
		t.Fatalf("create offering: %d %s", createdOffering.Code, createdOffering.Body.String())
	}
	if w := call("GET", "/api/admin/offerings", "", "", admin); w.Code != 200 || !strings.Contains(w.Body.String(), offering.ID) {
		t.Fatalf("admin offering list: %d %s", w.Code, w.Body.String())
	}
	if w := call("GET", "/api/admin/offerings", "", "", who); w.Code != 403 {
		t.Fatalf("owner offering list: %d %s", w.Code, w.Body.String())
	}
	deleteDraftResponse := call("POST", "/api/admin/offerings", `{"code":"delete-http","name":"待删除草稿","nodeId":"hk-01","sourceMarket":"","destinationMarket":"","notes":"配置错误"}`, "", admin)
	var deleteDraft platform.LineOffering
	if err = json.Unmarshal(deleteDraftResponse.Body.Bytes(), &deleteDraft); err != nil || deleteDraftResponse.Code != 201 {
		t.Fatalf("create deletable draft: %d %s", deleteDraftResponse.Code, deleteDraftResponse.Body.String())
	}
	deleteBody := `{"offeringId":"` + deleteDraft.ID + `","expectedRevision":1}`
	if w := call("POST", "/api/admin/offerings/delete", deleteBody, "", who); w.Code != 403 {
		t.Fatalf("owner offering delete: %d %s", w.Code, w.Body.String())
	}
	if w := call("POST", "/api/admin/offerings/delete", deleteBody, "", admin); w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"deleted"`) {
		t.Fatalf("delete draft: %d %s", w.Code, w.Body.String())
	}
	stateBody := `{"offeringId":"` + offering.ID + `","expectedRevision":1,"state":"published"}`
	publishedOffering := call("POST", "/api/admin/offerings/state", stateBody, "", admin)
	if err = json.Unmarshal(publishedOffering.Body.Bytes(), &offering); err != nil || publishedOffering.Code != 200 || offering.State != platform.OfferingStatePublished || offering.Revision != 2 {
		t.Fatalf("publish unprovisioned offering: %d %s", publishedOffering.Code, publishedOffering.Body.String())
	}
	if w := call("POST", "/api/admin/offerings/state", stateBody, "", admin); w.Code != 409 {
		t.Fatalf("stale offering state: %d %s", w.Code, w.Body.String())
	}
	body := `{"offeringId":"` + offering.ID + `","code":"001","name":"Automatic version plan","priceCents":25,"quotaBytes":321,"bandwidthMbps":50,"periodSeconds":86400}`
	if w := call("POST", "/api/admin/plans", body, "", who); w.Code != 403 {
		t.Fatalf("owner plan creation: %d %s", w.Code, w.Body.String())
	}
	for label, invalidBody := range map[string]string{
		"null version":       strings.Replace(body, `"name"`, `"version":null,"name"`, 1),
		"string version":     strings.Replace(body, `"name"`, `"version":"1","name"`, 1),
		"fractional version": strings.Replace(body, `"name"`, `"version":1.5,"name"`, 1),
		"negative version":   strings.Replace(body, `"name"`, `"version":-1,"name"`, 1),
	} {
		if w := call("POST", "/api/admin/plans", invalidBody, "", admin); w.Code != 400 {
			t.Fatalf("%s accepted: %d %s", label, w.Code, w.Body.String())
		}
	}
	zeroVersionBody := strings.Replace(body, `"name"`, `"version":0,"name"`, 1)
	created := call("POST", "/api/admin/plans", zeroVersionBody, "", admin)
	var plan platform.PlanPriceVersion
	if err = json.Unmarshal(created.Body.Bytes(), &plan); err != nil || created.Code != 201 || plan.Code != "001" || plan.Version != 1 || plan.QuotaBytes != 321 || plan.PeriodSeconds != 86400 {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	omittedVersionReplay := call("POST", "/api/admin/plans", body, "", admin)
	var planReplay platform.PlanPriceVersion
	if err = json.Unmarshal(omittedVersionReplay.Body.Bytes(), &planReplay); err != nil || omittedVersionReplay.Code != 201 || planReplay.ID != plan.ID || planReplay.Version != 1 {
		t.Fatalf("omitted version replay: %d %s", omittedVersionReplay.Code, omittedVersionReplay.Body.String())
	}
	if w := call("POST", "/api/admin/offerings/delete", `{"offeringId":"`+offering.ID+`","expectedRevision":2}`, "", admin); w.Code != 409 {
		t.Fatalf("linked non-draft offering deleted: %d %s", w.Code, w.Body.String())
	}
	if w := call("GET", "/api/plans", "", "", who); w.Code != 200 {
		t.Fatalf("catalog without rule: %d %s", w.Code, w.Body.String())
	}
	if err = store.AdminAddNode(ctx, admin, platform.NodeInput{ID: "sg-http", Name: "Singapore", Region: "Singapore", ListenAddress: "0.0.0.0", CapacityMbps: 1000, ResourceMode: platform.ResourceModeShared}); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, "UPDATE nodes SET status='ready' WHERE id='sg-http'"); err != nil {
		t.Fatal(err)
	}
	sgOfferingResponse := call("POST", "/api/admin/offerings", `{"code":"sg-http","name":"新加坡线路","nodeId":"sg-http","sourceMarket":"东南亚","destinationMarket":"国际"}`, "", admin)
	var sgOffering platform.LineOffering
	if err = json.Unmarshal(sgOfferingResponse.Body.Bytes(), &sgOffering); err != nil || sgOfferingResponse.Code != 201 {
		t.Fatalf("create SG offering: %d %s", sgOfferingResponse.Code, sgOfferingResponse.Body.String())
	}
	sgState := `{"offeringId":"` + sgOffering.ID + `","expectedRevision":1,"state":"published"}`
	if w := call("POST", "/api/admin/offerings/state", sgState, "", admin); w.Code != 200 {
		t.Fatalf("publish SG offering: %d %s", w.Code, w.Body.String())
	}
	sgPlanBody := `{"offeringId":"` + sgOffering.ID + `","code":"sg-per-rule","version":1,"name":"SG plan","priceCents":25,"quotaBytes":321,"bandwidthMbps":50,"periodSeconds":86400}`
	sgCreated := call("POST", "/api/admin/plans", sgPlanBody, "", admin)
	var sgPlan platform.PlanPriceVersion
	if err = json.Unmarshal(sgCreated.Body.Bytes(), &sgPlan); err != nil || sgCreated.Code != 201 {
		t.Fatalf("create SG plan: %d %s", sgCreated.Code, sgCreated.Body.String())
	}
	if w := call("GET", "/api/plans?ruleId="+rule.ID, "", "", who); w.Code != 200 || !strings.Contains(w.Body.String(), plan.ID) || strings.Contains(w.Body.String(), sgPlan.ID) || !strings.Contains(w.Body.String(), `"resourceMode":"shared"`) {
		t.Fatalf("rule catalog: %d %s", w.Code, w.Body.String())
	}
	if w := call("GET", "/api/plans?ruleId="+rule.ID, "", "", foreign); w.Code != 404 {
		t.Fatalf("foreign rule catalog: %d %s", w.Code, w.Body.String())
	}
	if w := call("GET", "/api/admin/plans", "", "", who); w.Code != 403 {
		t.Fatalf("owner admin listing: %d", w.Code)
	}
	if w := call("GET", "/api/plans", "", "", platform.Identity{}); w.Code != 401 {
		t.Fatalf("anonymous plans: %d", w.Code)
	}
	purchase := `{"ruleId":"` + rule.ID + `","priceVersionId":"` + plan.ID + `"}`
	if w := call("POST", "/api/subscriptions/purchase", purchase, "purchase-no-funds", who); w.Code != 409 || !strings.Contains(w.Body.String(), "钱包余额不足，请先充值或联系平台管理员") {
		t.Fatalf("insufficient: %d %s", w.Code, w.Body.String())
	}
	if _, err = store.AdjustWalletFromCLI(ctx, platform.WalletAdjustment{TenantID: who.TenantID, Key: "http-subscription-funds", AmountCents: 100, Reason: "local correction", Operator: "test"}); err != nil {
		t.Fatal(err)
	}
	wrongPurchase := `{"ruleId":"` + rule.ID + `","priceVersionId":"` + sgPlan.ID + `"}`
	if w := call("POST", "/api/subscriptions/purchase", wrongPurchase, "wrong-node-http", who); w.Code != 409 {
		t.Fatalf("wrong-node purchase: %d %s", w.Code, w.Body.String())
	}
	wallet, err := store.Wallet(ctx, who)
	if err != nil || wallet.BalanceCents != 100 {
		t.Fatalf("wrong-node purchase changed wallet: %+v %v", wallet, err)
	}
	created = call("POST", "/api/subscriptions/purchase", purchase, "purchase-http-once", who)
	var period platform.RuleSubscription
	if err = json.Unmarshal(created.Body.Bytes(), &period); err != nil || created.Code != 201 || period.RemainingBytes != 321 || period.UsedBytes != 0 {
		t.Fatalf("purchase: %d %s", created.Code, created.Body.String())
	}
	renewBody := `{"subscriptionId":"` + period.ID + `","enabled":true}`
	if w := call("POST", "/api/subscriptions/auto-renew", renewBody, "", who); w.Code != 200 || !strings.Contains(w.Body.String(), `"autoRenew":true`) {
		t.Fatalf("enable: %d %s", w.Code, w.Body.String())
	}
	if w := call("POST", "/api/subscriptions/auto-renew", renewBody, "", foreign); w.Code != 404 {
		t.Fatalf("cross tenant enable: %d", w.Code)
	}
	if w := call("GET", "/api/subscriptions?all=true", "", "", who); w.Code != 200 || !strings.Contains(w.Body.String(), period.ID) {
		t.Fatalf("all: %d %s", w.Code, w.Body.String())
	}
	if w := call("GET", "/api/subscriptions?all=true", "", "", foreign); w.Code != 200 || strings.Contains(w.Body.String(), period.ID) {
		t.Fatalf("foreign all: %d %s", w.Code, w.Body.String())
	}
	for _, body := range []string{`{"subscriptionId":"` + period.ID + `"}`, `{"subscriptionId":"` + period.ID + `","enabled":null}`, `{"subscriptionId":"` + period.ID + `","enabled":"true"}`, `{"subscriptionId":"` + period.ID + `","enabled":true,"tenantId":"` + who.TenantID + `"}`} {
		if w := call("POST", "/api/subscriptions/auto-renew", body, "", who); w.Code != 400 {
			t.Fatalf("invalid auto-renew: %d %s", w.Code, w.Body.String())
		}
	}
	if w := call("GET", "/api/subscriptions/auto-renew", "", "", who); w.Code != 405 {
		t.Fatalf("auto-renew GET: %d", w.Code)
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
		{"POST", "/api/admin/offerings", strings.TrimSuffix(offeringBody, "}") + `,"tenantId":"` + foreign.TenantID + `"}`, ""},
		{"POST", "/api/admin/offerings/state", strings.TrimSuffix(stateBody, "}") + `,"nodeId":"hk-01"}`, ""},
		{"POST", "/api/admin/offerings/delete", strings.TrimSuffix(deleteBody, "}") + `,"state":"draft"}`, ""},
		{"GET", "/api/admin/offerings?ruleId=" + rule.ID, "", ""},
		{"GET", "/api/plans?page=0", "", ""},
		{"GET", "/api/plans?ruleId=" + rule.ID + "&ruleId=" + rule.ID, "", ""},
		{"GET", "/api/admin/plans?ruleId=" + rule.ID, "", ""},
		{"GET", "/api/subscriptions", "", ""},
		{"GET", "/api/subscriptions?ruleId=" + rule.ID + "&before=-1", "", ""},
		{"GET", "/api/subscriptions?ruleId=" + rule.ID + "&tenantId=" + foreign.TenantID, "", ""},
	} {
		actor := who
		if strings.HasPrefix(tc.path, "/api/admin/") {
			actor = admin
		}
		if w := call(tc.method, tc.path, tc.body, tc.key, actor); w.Code != 400 {
			t.Fatalf("invalid accepted: %+v %d %s", tc, w.Code, w.Body.String())
		}
	}
	for _, tc := range []struct{ method, path string }{{"PUT", "/api/admin/offerings"}, {"GET", "/api/admin/offerings/state"}, {"GET", "/api/admin/offerings/delete"}, {"POST", "/api/plans"}} {
		if w := call(tc.method, tc.path, `{}`, "", admin); w.Code != 405 {
			t.Fatalf("invalid method accepted: %+v %d %s", tc, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	if s.serveSubscriptions(w, httptest.NewRequest("GET", "/api/other", nil), who) {
		t.Fatal("claimed other route")
	}
}
