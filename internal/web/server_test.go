package web

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"reladock/internal/platform"
)

func testServer(t *testing.T) http.Handler {
	store := testWebStore(t)
	var assets fs.FS = fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html>RelaDock</html>")}}
	return New(store, assets, "http://127.0.0.1:3001")
}

func testWebStore(t *testing.T) *platform.Store {
	t.Helper()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := "web_" + strings.ReplaceAll(time.Now().Format("150405.000000000"), ".", "")
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(dbURL)
	q := parsed.Query()
	q.Set("search_path", schema)
	parsed.RawQuery = q.Encode()
	store, err := platform.Open(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close(); admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); admin.Close() })
	return store
}

func request(handler http.Handler, method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://127.0.0.1:3001"+path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "RelaDock")
	req.Header.Set("Origin", "http://127.0.0.1:3001")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if path == "/api/rules" {
		req.Header.Set("Idempotency-Key", "web-rule-request")
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestAuthenticatedRuleFlowAndLogout(t *testing.T) {
	h := testServer(t)
	if rec := request(h, "GET", "/api/overview", "", nil); rec.Code != 401 {
		t.Fatalf("anonymous: %d", rec.Code)
	}
	rec := request(h, "POST", "/api/auth/register", `{"company":"Acme","email":"owner@example.com","password":"long-test-password-42"}`, nil)
	if rec.Code != 201 {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal("insecure session cookie")
	}
	cookie := cookies[0]
	rec = request(h, "POST", "/api/rules", `{"name":"Relay","protocol":"both","targetHost":"192.0.2.10","targetPort":443,"bandwidthMbps":1000}`, cookie)
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var rule platform.Rule
	if err := json.Unmarshal(rec.Body.Bytes(), &rule); err != nil {
		t.Fatal(err)
	}
	if rule.Status != "pending-agent" {
		t.Fatal("claimed deployment")
	}
	rec = request(h, "PATCH", "/api/rules/"+rule.ID, `{"name":"Relay updated","targetHost":"198.51.100.20","targetPort":8443,"bandwidthMbps":250,"desiredVersion":1}`, cookie)
	if rec.Code != 200 {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rule); err != nil {
		t.Fatal(err)
	}
	if rule.Name != "Relay updated" || rule.TargetHost != "198.51.100.20" || rule.TargetPort != 8443 || rule.BandwidthMbps != 250 || rule.DesiredVersion != 2 {
		t.Fatalf("unexpected updated rule: %+v", rule)
	}
	rec = request(h, "GET", "/api/overview", "", cookie)
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "balanceCents") || strings.Contains(rec.Body.String(), "usageBytes") {
		t.Fatal("fabricated billing data")
	}
	rec = request(h, "POST", "/api/rules/"+rule.ID+"/pause", "", cookie)
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	rec = request(h, "POST", "/api/rules/"+rule.ID+"/delete", "", cookie)
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	rec = request(h, "POST", "/api/auth/logout", "", cookie)
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	if rec = request(h, "GET", "/api/auth/me", "", cookie); rec.Code != 401 {
		t.Fatal("old session still works")
	}
	rec = request(h, "POST", "/api/auth/login", `{"email":"owner@example.com","password":"long-test-password-42"}`, nil)
	if rec.Code != 200 {
		t.Fatal("cannot log back in")
	}
}

func TestChangePasswordEndpointKeepsCurrentSessionAndRejectsOldPassword(t *testing.T) {
	h := testServer(t)
	registered := request(h, "POST", "/api/auth/register", `{"company":"Password","email":"change-password-http@example.com","password":"long-test-password-42"}`, nil)
	if registered.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", registered.Code, registered.Body.String())
	}
	cookie := registered.Result().Cookies()[0]
	if rec := request(h, "POST", "/api/auth/password", `{"currentPassword":"wrong-current-password","newPassword":"new-long-password-42"}`, cookie); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong current password: %d %s", rec.Code, rec.Body.String())
	}
	if rec := request(h, "POST", "/api/auth/password", `{"currentPassword":"long-test-password-42","newPassword":"new-long-password-42"}`, cookie); rec.Code != http.StatusOK {
		t.Fatalf("change password: %d %s", rec.Code, rec.Body.String())
	}
	if rec := request(h, "GET", "/api/auth/me", "", cookie); rec.Code != http.StatusOK {
		t.Fatalf("current session revoked: %d", rec.Code)
	}
	old := request(h, "POST", "/api/auth/login", `{"email":"change-password-http@example.com","password":"long-test-password-42"}`, nil)
	if old.Code != http.StatusUnauthorized {
		t.Fatalf("old password accepted: %d", old.Code)
	}
	newLogin := request(h, "POST", "/api/auth/login", `{"email":"change-password-http@example.com","password":"new-long-password-42"}`, nil)
	if newLogin.Code != http.StatusOK {
		t.Fatalf("new password rejected: %d %s", newLogin.Code, newLogin.Body.String())
	}
}

func TestTenantNodeInventoryScopesDedicatedNodes(t *testing.T) {
	store := testWebStore(t)
	assets := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("tenant")}}
	h := New(store, assets, "http://127.0.0.1:3001")
	first := request(h, "POST", "/api/auth/register", `{"company":"First","email":"inventory-first@example.com","password":"long-test-password-42"}`, nil)
	second := request(h, "POST", "/api/auth/register", `{"company":"Second","email":"inventory-second@example.com","password":"long-test-password-42"}`, nil)
	if first.Code != http.StatusCreated || second.Code != http.StatusCreated {
		t.Fatalf("register: %d %d", first.Code, second.Code)
	}
	firstCookie := first.Result().Cookies()[0]
	secondCookie := second.Result().Cookies()[0]
	firstWho, err := store.Session(context.Background(), firstCookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	secondWho, err := store.Session(context.Background(), secondCookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	admin, _, err := store.Register(context.Background(), "Operator", "inventory-admin@example.com", "long-test-password-42")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SetPlatformAdmin(context.Background(), admin.Email, true); err != nil {
		t.Fatal(err)
	}
	admin, _, err = store.Login(context.Background(), admin.Email, "long-test-password-42")
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []platform.NodeInput{
		{ID: "inventory-shared", Name: "Shared", Region: "Test", ListenAddress: "0.0.0.0", CapacityMbps: 1000, ResourceMode: platform.ResourceModeShared},
		{ID: "inventory-first", Name: "First only", Region: "Test", ListenAddress: "0.0.0.0", CapacityMbps: 1000, ResourceMode: platform.ResourceModeDedicatedTenant, DedicatedTenantID: firstWho.TenantID},
		{ID: "inventory-second", Name: "Second only", Region: "Test", ListenAddress: "0.0.0.0", CapacityMbps: 1000, ResourceMode: platform.ResourceModeDedicatedRule, DedicatedTenantID: secondWho.TenantID},
	} {
		if err = store.AdminAddNode(context.Background(), admin, input); err != nil {
			t.Fatal(err)
		}
	}
	assertInventory := func(cookie *http.Cookie, present, absent string) {
		t.Helper()
		for _, path := range []string{"/api/nodes", "/api/overview"} {
			rec := request(h, "GET", path, "", cookie)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			if !strings.Contains(body, `"id":"inventory-shared"`) || !strings.Contains(body, `"id":"`+present+`"`) || strings.Contains(body, `"id":"`+absent+`"`) || strings.Contains(body, "dedicatedTenantId") {
				t.Fatalf("scoped inventory %s: %s", path, body)
			}
		}
	}
	assertInventory(firstCookie, "inventory-first", "inventory-second")
	assertInventory(secondCookie, "inventory-second", "inventory-first")
}

func TestHTTPRejectsCrossSiteMalformedAndOversizedRequests(t *testing.T) {
	h := testServer(t)
	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"company":"Acme","email":"owner@example.com","password":"long-test-password-42","role":"admin"}`, 400},
		{`{} {}`, 400}, {`{"company":"` + strings.Repeat("x", 9000) + `"}`, 413},
	} {
		rec := request(h, "POST", "/api/auth/register", tc.body, nil)
		if rec.Code != tc.want {
			t.Fatalf("malformed: %d want %d", rec.Code, tc.want)
		}
	}
	for _, origin := range []string{"https://evil.example", "http://localhost:3001"} {
		req := httptest.NewRequest("POST", "http://127.0.0.1:3001/api/auth/register", strings.NewReader(`{}`))
		req.Header.Set("Origin", origin)
		req.Header.Set("X-Requested-With", "RelaDock")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 403 {
			t.Fatalf("cross-site: %d", rec.Code)
		}
	}
	req := httptest.NewRequest("POST", "http://127.0.0.1:3001/api/auth/register", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("missing CSRF header: %d", rec.Code)
	}
	req = httptest.NewRequest("GET", "http://evil.example/api/auth/me", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("foreign Host: %d", rec.Code)
	}
}

func TestAuthenticationRateLimit(t *testing.T) {
	h := testServer(t)
	for i := 0; i < 10; i++ {
		rec := request(h, "POST", "/api/auth/login", `{}`, nil)
		if rec.Code != 401 {
			t.Fatalf("attempt %d: %d", i, rec.Code)
		}
	}
	rec := request(h, "POST", "/api/auth/login", `{}`, nil)
	if rec.Code != 429 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("rate limit: %d", rec.Code)
	}
}

func TestRejectedCrossSiteRequestsDoNotConsumeLoginBudget(t *testing.T) {
	h := testServer(t)
	for i := 0; i < 12; i++ {
		req := httptest.NewRequest("POST", "http://127.0.0.1:3001/api/auth/login", strings.NewReader(`{}`))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Origin", "https://evil.example")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 403 {
			t.Fatalf("cross-site attempt %d: %d", i, rec.Code)
		}
	}
	if rec := request(h, "POST", "/api/auth/login", `{}`, nil); rec.Code != 401 {
		t.Fatalf("legitimate login budget exhausted: %d", rec.Code)
	}
}

func TestNodeInventoryIsReadOnlyAndResumeIsTenantScoped(t *testing.T) {
	h := testServer(t)
	if rec := request(h, "GET", "/api/nodes", "", nil); rec.Code != 401 {
		t.Fatal("anonymous node inventory")
	}
	first := request(h, "POST", "/api/auth/register", `{"company":"A","email":"a@example.com","password":"long-test-password-42"}`, nil)
	if first.Code != 201 {
		t.Fatal(first.Body.String())
	}
	a := first.Result().Cookies()[0]
	second := request(h, "POST", "/api/auth/register", `{"company":"B","email":"b@example.com","password":"long-test-password-42"}`, nil)
	if second.Code != 201 {
		t.Fatal(second.Body.String())
	}
	b := second.Result().Cookies()[0]
	created := request(h, "POST", "/api/rules", `{"name":"A secret target","protocol":"udp","targetHost":"192.0.2.10","targetPort":443,"bandwidthMbps":1000}`, a)
	var rule platform.Rule
	if err := json.Unmarshal(created.Body.Bytes(), &rule); created.Code != 201 || err != nil {
		t.Fatal(created.Body.String())
	}
	inventory := request(h, "GET", "/api/nodes", "", b)
	if inventory.Code != 200 || strings.Contains(inventory.Body.String(), "192.0.2.10") || strings.Contains(inventory.Body.String(), "certificate") {
		t.Fatal("inventory leaked node secrets or tenant targets")
	}
	if rec := request(h, "POST", "/api/nodes", `{"id":"attacker"}`, b); rec.Code != 404 {
		t.Fatal("tenant can manage global nodes")
	}
	if rec := request(h, "POST", "/agent/config/claim", "", a); rec.Code != 404 {
		t.Fatal("tenant cookie can access agent channel")
	}
	if rec := request(h, "POST", "/api/rules/"+rule.ID+"/resume", "", b); rec.Code != 404 {
		t.Fatal("cross-tenant resume")
	}
	if rec := request(h, "POST", "/api/rules/"+rule.ID+"/pause", "", a); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	resumed := request(h, "POST", "/api/rules/"+rule.ID+"/resume", "", a)
	json.Unmarshal(resumed.Body.Bytes(), &rule)
	if resumed.Code != 200 || rule.Status != "pending-agent" || rule.DesiredVersion != 3 {
		t.Fatalf("resume: %d %s", resumed.Code, resumed.Body.String())
	}
}

func TestPlatformAdminAPIAuthorizationNodeWritesAndConflict(t *testing.T) {
	store := testWebStore(t)
	assets := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("tenant")}, "admin.html": &fstest.MapFile{Data: []byte("platform admin")}}
	h := New(store, assets, "http://127.0.0.1:3001")
	reg := request(h, "POST", "/api/auth/register", `{"company":"Operator","email":"operator@example.com","password":"long-test-password-42"}`, nil)
	cookie := reg.Result().Cookies()[0]
	if rec := request(h, "GET", "/api/admin/overview", "", cookie); rec.Code != 403 {
		t.Fatalf("owner has platform access: %d", rec.Code)
	}
	if rec := request(h, "POST", "/api/admin/nodes", `{"id":"hk-test","name":"test","region":"Hong Kong","listenAddress":"0.0.0.0","capacityMbps":1000}`, cookie); rec.Code != 403 {
		t.Fatal("owner can create platform node")
	}
	if err := store.SetPlatformAdmin(context.Background(), "operator@example.com", true); err != nil {
		t.Fatal(err)
	}
	if rec := request(h, "GET", "/api/admin/overview", "", cookie); rec.Code != 401 {
		t.Fatal("grant preserved old session")
	}
	login := request(h, "POST", "/api/auth/login", `{"email":"operator@example.com","password":"long-test-password-42"}`, nil)
	cookie = login.Result().Cookies()[0]
	for _, path := range []string{"/api/admin/overview", "/api/admin/nodes", "/api/admin/tenants", "/api/admin/rules", "/api/admin/audit"} {
		if rec := request(h, "GET", path, "", cookie); rec.Code != 200 {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	created := request(h, "POST", "/api/admin/nodes", `{"id":"hk-test","name":"香港测试","region":"Hong Kong","listenAddress":"0.0.0.0","publicAddress":"192.0.2.60","capacityMbps":1000,"resourceMode":"shared","dedicatedTenantId":""}`, cookie)
	if created.Code != 201 {
		t.Fatal(created.Body.String())
	}
	rec := request(h, "POST", "/api/admin/nodes/hk-test/maintenance", `{"revision":1}`, cookie)
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	rec = request(h, "POST", "/api/admin/nodes/hk-test/enable", `{"revision":1}`, cookie)
	if rec.Code != 409 {
		t.Fatalf("stale write: %d", rec.Code)
	}
	rec = request(h, "POST", "/api/admin/nodes/hk-test/enroll", `{"revision":2,"certificate":"-----BEGIN PRIVATE KEY-----"}`, cookie)
	if rec.Code != 400 {
		t.Fatal("accepted private key")
	}
	rec = request(h, "GET", "/api/admin/nodes/hk-test/deliveries", "", cookie)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "leaseToken") || strings.Contains(rec.Body.String(), "payload") {
		t.Fatal("invalid delivery metadata")
	}
	rec = request(h, "GET", "/api/admin/rules?tenantId=invalid", "", cookie)
	if rec.Code != 400 {
		t.Fatalf("invalid filter: %d", rec.Code)
	}
	if err := store.SetPlatformAdmin(context.Background(), "operator@example.com", false); err != nil {
		t.Fatal(err)
	}
	if rec = request(h, "GET", "/api/admin/nodes", "", cookie); rec.Code != 401 {
		t.Fatal("revoked session remains authorized")
	}
}

func TestAdminWithdrawNodeRegistrationHTTP(t *testing.T) {
	store := testWebStore(t)
	assets := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("tenant")}, "admin.html": &fstest.MapFile{Data: []byte("platform admin")}}
	h := New(store, assets, "http://127.0.0.1:3001")
	reg := request(h, "POST", "/api/auth/register", `{"company":"Operator","email":"withdraw-operator@example.com","password":"long-test-password-42"}`, nil)
	if reg.Code != http.StatusCreated {
		t.Fatal(reg.Body.String())
	}
	if err := store.SetPlatformAdmin(context.Background(), "withdraw-operator@example.com", true); err != nil {
		t.Fatal(err)
	}
	login := request(h, "POST", "/api/auth/login", `{"email":"withdraw-operator@example.com","password":"long-test-password-42"}`, nil)
	adminCookie := login.Result().Cookies()[0]
	created := request(h, "POST", "/api/admin/nodes", `{"id":"unused-http","name":"误登记节点","region":"Hong Kong","listenAddress":"0.0.0.0","capacityMbps":1000,"resourceMode":"shared","dedicatedTenantId":""}`, adminCookie)
	if created.Code != http.StatusCreated {
		t.Fatal(created.Body.String())
	}
	listed := request(h, "GET", "/api/admin/nodes", "", adminCookie)
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), `"id":"unused-http"`) || !strings.Contains(listed.Body.String(), `"withdrawable":true`) {
		t.Fatalf("withdrawal fact missing: %d %s", listed.Code, listed.Body.String())
	}
	if rec := request(h, "POST", "/api/admin/nodes/unused-http/withdraw-registration", `{"revision":1,"force":true}`, adminCookie); rec.Code != http.StatusBadRequest {
		t.Fatalf("withdrawal accepted unknown field: %d %s", rec.Code, rec.Body.String())
	}
	if rec := request(h, "POST", "/api/admin/nodes/unused-http/withdraw-registration", `{"revision":2}`, adminCookie); rec.Code != http.StatusConflict {
		t.Fatalf("withdrawal accepted stale revision: %d %s", rec.Code, rec.Body.String())
	}
	other := request(h, "POST", "/api/auth/register", `{"company":"Customer","email":"withdraw-customer@example.com","password":"long-test-password-42"}`, nil)
	if other.Code != http.StatusCreated {
		t.Fatal(other.Body.String())
	}
	if rec := request(h, "POST", "/api/admin/nodes/unused-http/withdraw-registration", `{"revision":1}`, other.Result().Cookies()[0]); rec.Code != http.StatusForbidden {
		t.Fatalf("tenant withdrew platform node: %d %s", rec.Code, rec.Body.String())
	}
	if rec := request(h, "POST", "/api/admin/nodes/unused-http/withdraw-registration", `{"revision":1}`, adminCookie); rec.Code != http.StatusOK {
		t.Fatalf("withdrawal failed: %d %s", rec.Code, rec.Body.String())
	}
	listed = request(h, "GET", "/api/admin/nodes", "", adminCookie)
	if listed.Code != http.StatusOK || strings.Contains(listed.Body.String(), `"id":"unused-http"`) {
		t.Fatalf("withdrawn node remained listed: %d %s", listed.Code, listed.Body.String())
	}
}
