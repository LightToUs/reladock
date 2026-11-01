package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"reladock/internal/platform"
)

func operationsHTTPServer(t *testing.T) (*server, platform.Identity, *pgxpool.Pool) {
	t.Helper()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	admin, err := pgxpool.New(context.Background(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	schema := "web_operations_" + hex.EncodeToString(nonce[:])
	if _, err = admin.Exec(context.Background(), `CREATE SCHEMA `+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`); admin.Close() })
	parsed, err := url.Parse(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	q.Set("search_path", schema)
	parsed.RawQuery = q.Encode()
	store, err := platform.Open(context.Background(), parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	pool, err := pgxpool.New(context.Background(), parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var exists bool
	if err = pool.QueryRow(context.Background(), `SELECT to_regclass('operations_incidents') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		sql, e := os.ReadFile("../platform/migration8.sql")
		if e != nil {
			t.Fatal(e)
		}
		if _, e = pool.Exec(context.Background(), string(sql)); e != nil {
			t.Fatal(e)
		}
	}
	owner, _, err := store.Register(context.Background(), "Operations HTTP", "operations-http@example.com", "long-test-password-42")
	if err != nil {
		t.Fatal(err)
	}
	return &server{store: store, origin: "http://127.0.0.1:3001", host: "127.0.0.1:3001", limits: map[string]window{}}, owner, pool
}
func operationsHTTP(s *server, who platform.Identity, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://127.0.0.1:3001"+path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	if !s.serveOperations(w, r, who) {
		http.NotFound(w, r)
	}
	return w
}

func TestOperationsHTTPTicketLifecycleAndStrictBoundaries(t *testing.T) {
	s, owner, pool := operationsHTTPServer(t)
	if _, err := s.store.CreateRule(context.Background(), owner, "operations-resource-rule", platform.RuleInput{Name: "resource relay", Protocol: "tcp", TargetHost: "192.0.2.10", TargetPort: 443, BandwidthMbps: 100, NodeID: "hk-01"}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO node_resource_latest(node_id,sampled_at,cpu_percent,memory_percent,disk_percent,conntrack_percent,clock_skew_seconds,dataplane_state) VALUES('hk-01',clock_timestamp(),11,22,33,4,5,'in-sync')`); err != nil {
		t.Fatal(err)
	}
	nodes := operationsHTTP(s, owner, "GET", "/api/operations/nodes", "")
	if nodes.Code != 200 || !strings.Contains(nodes.Body.String(), `"resourceStatus":"normal"`) || !strings.Contains(nodes.Body.String(), `"cpuPercent":11`) || !strings.Contains(nodes.Body.String(), `"conntrackPercent":4`) || !strings.Contains(nodes.Body.String(), `"dataPlaneState":"in-sync"`) {
		t.Fatalf("resource HTTP facts: %d %s", nodes.Code, nodes.Body.String())
	}
	for _, body := range []string{`{"subject":"Question","body":"Details","tenantId":"other"}`, `{} {}`, `null`} {
		w := operationsHTTP(s, owner, "POST", "/api/operations/tickets", body)
		if w.Code != 400 {
			t.Fatalf("strict input: %d %s", w.Code, w.Body.String())
		}
	}
	w := operationsHTTP(s, owner, "POST", "/api/operations/tickets", `{"subject":"Control question","body":"Can you check the validation failure?"}`)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var ticket platform.SupportTicket
	if err := json.Unmarshal(w.Body.Bytes(), &ticket); err != nil {
		t.Fatal(err)
	}
	w = operationsHTTP(s, owner, "GET", "/api/operations/tickets/"+ticket.ID, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "validation failure") {
		t.Fatalf("thread: %d %s", w.Code, w.Body.String())
	}
	other, _, err := s.store.Register(context.Background(), "Other", "ops-http-other@example.com", "long-test-password-42")
	if err != nil {
		t.Fatal(err)
	}
	w = operationsHTTP(s, other, "GET", "/api/operations/tickets/"+ticket.ID, "")
	if w.Code != 404 {
		t.Fatalf("other tenant read: %d", w.Code)
	}
	w = operationsHTTP(s, other, "POST", "/api/operations/tickets/"+ticket.ID+"/replies", `{"body":"intrusion","revision":1}`)
	if w.Code != 404 {
		t.Fatalf("other tenant reply: %d", w.Code)
	}
	w = operationsHTTP(s, owner, "GET", "/api/admin/operations/tickets", "")
	if w.Code != 403 {
		t.Fatalf("owner platform: %d", w.Code)
	}
	for _, path := range []string{"/api/operations/tickets?page=0", "/api/operations/nodes?staleAfterSeconds=0", "/api/operations/incidents?page=abc"} {
		w = operationsHTTP(s, owner, "GET", path, "")
		if w.Code != 400 {
			t.Fatalf("query validation: %s %d", path, w.Code)
		}
	}
	if w = operationsHTTP(s, owner, "POST", "/api/operations/tickets/bad/replies", `{}`); w.Code != 404 {
		t.Fatalf("UUID validation: %d", w.Code)
	}
}

func TestOperationsHTTPNodeTrafficRespectsTenantAndAdminScope(t *testing.T) {
	s, owner, pool := operationsHTTPServer(t)
	ctx := context.Background()
	other, _, err := s.store.Register(ctx, "Other traffic", "operations-traffic-other@example.com", "long-test-password-42")
	if err != nil {
		t.Fatal(err)
	}
	create := func(who platform.Identity, key string) platform.Rule {
		rule, createErr := s.store.CreateRule(ctx, who, key, platform.RuleInput{Name: key, Protocol: "tcp", TargetHost: "192.0.2.10", TargetPort: 443, BandwidthMbps: 100, NodeID: "hk-01"})
		if createErr != nil {
			t.Fatal(createErr)
		}
		return rule
	}
	ownerRule := create(owner, "owner-traffic-rule")
	otherRule := create(other, "other-traffic-rule")
	insert := func(rule platform.Rule, tenantID, epoch string, upload, download int64) {
		if _, insertErr := pool.Exec(ctx, `INSERT INTO billing_usage_epochs(rule_id,tenant_id,node_id,epoch,last_sequence,upload_bytes,download_bytes) VALUES($1,$2,$3,$4,2,$5,$6)`, rule.ID, tenantID, rule.NodeID, epoch, upload, download); insertErr != nil {
			t.Fatal(insertErr)
		}
		if _, insertErr := pool.Exec(ctx, `INSERT INTO billing_usage_reports(rule_id,tenant_id,node_id,epoch,sequence,upload_bytes,download_bytes,upload_delta_bytes,download_delta_bytes,received_at) VALUES
			($1,$2,$3,$4,1,100,100,100,100,clock_timestamp()-interval '10 seconds'),
			($1,$2,$3,$4,2,$5::bigint,$6::bigint,$5::bigint-100,$6::bigint-100,clock_timestamp()-interval '5 seconds')`, rule.ID, tenantID, rule.NodeID, epoch, upload, download); insertErr != nil {
			t.Fatal(insertErr)
		}
		if _, insertErr := pool.Exec(ctx, `INSERT INTO billing_usage_totals(rule_id,tenant_id,node_id,active_epoch,upload_bytes,download_bytes) VALUES($1,$2,$3,$4,$5,$6)`, rule.ID, tenantID, rule.NodeID, epoch, upload, download); insertErr != nil {
			t.Fatal(insertErr)
		}
	}
	insert(ownerRule, owner.TenantID, "owner-http-epoch", 600, 300)
	insert(otherRule, other.TenantID, "other-http-epoch", 1100, 600)
	decode := func(w *httptest.ResponseRecorder) platform.OperationNode {
		if w.Code != http.StatusOK {
			t.Fatalf("nodes response: %d %s", w.Code, w.Body.String())
		}
		var page platform.Page[platform.OperationNode]
		if err = json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		for _, node := range page.Items {
			if node.ID == "hk-01" {
				return node
			}
		}
		t.Fatalf("hk-01 missing: %s", w.Body.String())
		return platform.OperationNode{}
	}
	tenantNode := decode(operationsHTTP(s, owner, "GET", "/api/operations/nodes", ""))
	if tenantNode.UploadBytes != 600 || tenantNode.DownloadBytes != 300 || tenantNode.RateState != "fresh" {
		t.Fatalf("tenant traffic leaked or missing: %+v", tenantNode)
	}
	if err = s.store.SetPlatformAdmin(ctx, owner.Email, true); err != nil {
		t.Fatal(err)
	}
	owner.PlatformAdmin = true
	adminNode := decode(operationsHTTP(s, owner, "GET", "/api/admin/operations/nodes", ""))
	if adminNode.UploadBytes != 1700 || adminNode.DownloadBytes != 900 || adminNode.UploadBytesPerSecond == nil || *adminNode.UploadBytesPerSecond != 300 {
		t.Fatalf("admin aggregate missing: %+v", adminNode)
	}
}

func TestOperationsHTTPIntegratedAdminReplyAndBearerDenial(t *testing.T) {
	s, owner, _ := operationsHTTPServer(t)
	ctx := context.Background()
	ticket, err := s.store.CreateSupportTicket(ctx, owner, "Admin response", "Please verify the control contact.")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.store.SetPlatformAdmin(ctx, owner.Email, true); err != nil {
		t.Fatal(err)
	}
	_, session, err := s.store.Login(ctx, owner.Email, "long-test-password-42")
	if err != nil {
		t.Fatal(err)
	}
	h := New(s.store, nil, s.origin)
	w := request(h, "POST", "/api/admin/operations/tickets/"+ticket.ID+"/replies", `{"body":"Control contact investigated.","revision":1,"status":"resolved"}`, &http.Cookie{Name: cookieName, Value: session})
	if w.Code != 200 {
		t.Fatalf("integrated admin reply: %d %s", w.Code, w.Body.String())
	}
	thread, err := s.store.SupportThread(ctx, owner, ticket.ID, false)
	if err != nil || thread.Ticket.Status != "resolved" || len(thread.Messages) != 2 {
		t.Fatalf("persisted admin response: %+v %v", thread, err)
	}
	_, raw, err := s.store.CreateAPIKey(ctx, owner, "read support", "read", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path string
		want int
	}{{"/api/operations/tickets", 200}, {"/api/admin/operations/tickets", 403}} {
		r := httptest.NewRequest("GET", s.origin+tc.path, nil)
		r.Header.Set("Authorization", "Bearer "+raw)
		r.RemoteAddr = "127.0.0.1:55555"
		w = httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("bearer %s: %d %s", tc.path, w.Code, w.Body.String())
		}
	}
}
