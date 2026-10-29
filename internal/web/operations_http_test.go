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

func operationsHTTPServer(t *testing.T) (*server, platform.Identity) {
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
	defer pool.Close()
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
	return &server{store: store, origin: "http://127.0.0.1:3001", host: "127.0.0.1:3001", limits: map[string]window{}}, owner
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
	s, owner := operationsHTTPServer(t)
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

func TestOperationsHTTPIntegratedAdminReplyAndBearerDenial(t *testing.T) {
	s, owner := operationsHTTPServer(t)
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
