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

func sourceLoggingHTTPServer(t *testing.T) (*platform.Store, http.Handler) {
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
	schema := "source_logging_http_" + strings.ReplaceAll(time.Now().Format("150405.000000000"), ".", "")
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(dbURL)
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	store, err := platform.OpenWithOptions(ctx, parsed.String(), platform.StoreOptions{AllowUnentitledRules: true, SourceFlowRetentionDays: 180})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.Close()
		admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})
	var assets fs.FS = fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("source logging")}}
	return store, New(store, assets, "http://127.0.0.1:3001")
}

func TestSourceLoggingHTTPIsAdminOnlyStrictAndVersioned(t *testing.T) {
	store, handler := sourceLoggingHTTPServer(t)
	registered := request(handler, http.MethodPost, "/api/auth/register", `{"company":"Source Admin","email":"source-http-admin@example.com","password":"long-test-password-42"}`, nil)
	if registered.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", registered.Code, registered.Body.String())
	}
	tenantCookie := registered.Result().Cookies()[0]
	who, err := store.Session(context.Background(), tenantCookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	created := request(handler, http.MethodPost, "/api/rules", `{"name":"Relay","protocol":"tcp","targetHost":"198.51.100.20","targetPort":443,"bandwidthMbps":100,"nodeId":"hk-01"}`, tenantCookie)
	if created.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	var rule platform.Rule
	if err := json.Unmarshal(created.Body.Bytes(), &rule); err != nil {
		t.Fatal(err)
	}
	nonAdmin := request(handler, http.MethodPost, "/api/auth/register", `{"company":"Other Tenant","email":"source-http-other@example.com","password":"long-test-password-42"}`, nil)
	if nonAdmin.Code != http.StatusCreated {
		t.Fatalf("non-admin register: %d %s", nonAdmin.Code, nonAdmin.Body.String())
	}
	nonAdminCookie := nonAdmin.Result().Cookies()[0]

	if err := store.SetPlatformAdmin(context.Background(), who.Email, true); err != nil {
		t.Fatal(err)
	}
	loggedIn := request(handler, http.MethodPost, "/api/auth/login", `{"email":"source-http-admin@example.com","password":"long-test-password-42"}`, nil)
	if loggedIn.Code != http.StatusOK {
		t.Fatalf("admin login: %d %s", loggedIn.Code, loggedIn.Body.String())
	}
	adminCookie := loggedIn.Result().Cookies()[0]
	fingerprint := strings.Repeat("a", 64)
	if err := store.EnrollNode(context.Background(), "hk-01", fingerprint); err != nil {
		t.Fatal(err)
	}
	heartbeat := platform.AgentHeartbeat{
		SourceFlowSupported: true,
		Version:             "source-flow-test",
		Mode:                "apply",
		StartedAt:           time.Now().Add(-time.Minute),
		CertificateNotAfter: time.Now().Add(24 * time.Hour),
		RotationState:       "idle",
	}
	if err := store.ReportAgentHeartbeat(context.Background(), "hk-01", fingerprint, heartbeat); err != nil {
		t.Fatal(err)
	}

	path := "/api/admin/rules/" + rule.ID + "/source-logging"
	body := `{"tenantId":"` + who.TenantID + `","desiredVersion":1,"enabled":true}`
	if rec := request(handler, http.MethodPatch, path, body, nonAdminCookie); rec.Code != http.StatusForbidden {
		t.Fatalf("tenant access: %d %s", rec.Code, rec.Body.String())
	}
	if rec := request(handler, http.MethodPatch, path, strings.TrimSuffix(body, "}")+`,"extra":true}`, adminCookie); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field: %d %s", rec.Code, rec.Body.String())
	}
	rec := request(handler, http.MethodPatch, path, body, adminCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("toggle: %d %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rule); err != nil {
		t.Fatal(err)
	}
	if !rule.SourceLoggingEnabled || rule.DesiredVersion != 2 {
		t.Fatalf("response: %+v", rule)
	}
	if rec = request(handler, http.MethodPatch, path, `{"tenantId":"`+who.TenantID+`","desiredVersion":1,"enabled":false}`, adminCookie); rec.Code != http.StatusConflict {
		t.Fatalf("stale version: %d %s", rec.Code, rec.Body.String())
	}
}

func TestSourceLoggingConfigReportUsesBoundedExpandedJSONLimit(t *testing.T) {
	body := `{"payload":"` + strings.Repeat("x", 16*1024) + `"}`
	req := httptest.NewRequest(http.MethodPost, "https://control.test/agent/config/job/report", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	var value struct {
		Payload string `json:"payload"`
	}
	expanded := httptest.NewRecorder()
	if !decodeLimit(expanded, req, &value, 2*1024*1024) || len(value.Payload) != 16*1024 {
		t.Fatalf("expanded bounded decoder rejected manifest: status=%d body=%s", expanded.Code, expanded.Body.String())
	}
	req = httptest.NewRequest(http.MethodPost, "https://control.test/agent/config/job/report", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	regular := httptest.NewRecorder()
	if decode(regular, req, &value) || regular.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("regular API decoder accepted oversized body: status=%d", regular.Code)
	}
}
