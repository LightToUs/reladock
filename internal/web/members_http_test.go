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

func membersHTTPServer(t *testing.T) (*server, platform.Identity) {
	t.Helper()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	pool, err := pgxpool.New(context.Background(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		pool.Close()
		t.Fatal(err)
	}
	own := "web_members_" + hex.EncodeToString(nonce[:])
	if _, err = pool.Exec(context.Background(), `CREATE SCHEMA `+own); err != nil {
		pool.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Exec(context.Background(), `DROP SCHEMA `+own+` CASCADE`); pool.Close() })
	parsed, err := url.Parse(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", own)
	parsed.RawQuery = query.Encode()
	store, err := platform.Open(context.Background(), parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	conn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err = conn.Exec(context.Background(), `SET search_path TO "`+own+`"`); err != nil {
		t.Fatal(err)
	}
	var exists bool
	if err = conn.QueryRow(context.Background(), `SELECT to_regclass('member_invitations') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		sql, e := os.ReadFile("../platform/migration4.sql")
		if e != nil {
			t.Fatal(e)
		}
		if _, e = conn.Exec(context.Background(), string(sql)); e != nil {
			t.Fatal(e)
		}
	}
	owner, _, err := store.Register(context.Background(), "Members", "members-http@example.com", "long-test-password-42")
	if err != nil {
		t.Fatal(err)
	}
	return &server{store: store, origin: "http://127.0.0.1:3001", host: "127.0.0.1:3001", limits: map[string]window{}}, owner
}

func memberHTTP(s *server, who platform.Identity, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://127.0.0.1:3001"+path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Requested-With", "RelaDock")
	r.Header.Set("Origin", s.origin)
	r.RemoteAddr = "127.0.0.1:23456"
	w := httptest.NewRecorder()
	if !s.serveInvitationAccept(w, r) && !s.serveMembers(w, r, who) {
		http.NotFound(w, r)
	}
	return w
}

func TestMembersHTTPStrictDecodeAndInvitationFlow(t *testing.T) {
	s, owner := membersHTTPServer(t)
	for _, body := range []string{`{"email":"a@example.com","role":"viewer","platformAdmin":true}`, `{} {}`, `null`} {
		w := memberHTTP(s, owner, "POST", "/api/invitations", body)
		if w.Code != 400 {
			t.Fatalf("strict decode: %d %s", w.Code, w.Body.String())
		}
	}
	w := memberHTTP(s, owner, "POST", "/api/invitations", `{"email":"new@example.com","role":"viewer","expiresAt":"`+time.Now().Add(time.Hour).UTC().Format(time.RFC3339)+`"}`)
	if w.Code != 201 {
		t.Fatalf("invite: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.Token == "" {
		t.Fatal("missing one-time secret")
	}
	w = memberHTTP(s, owner, "GET", "/api/invitations", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), created.Token) || strings.Contains(w.Body.String(), "token_hash") {
		t.Fatalf("list secrets: %s", w.Body.String())
	}
	w = memberHTTP(s, platform.Identity{}, "POST", "/api/auth/invitations/accept", `{"token":"`+created.Token+`","email":"new@example.com","password":"long-new-password-42"}`)
	if w.Code != 201 || len(w.Result().Cookies()) != 1 || !w.Result().Cookies()[0].HttpOnly {
		t.Fatalf("accept: %d %s", w.Code, w.Body.String())
	}
	var who platform.Identity
	if err := json.Unmarshal(w.Body.Bytes(), &who); err != nil || who.Role != "viewer" || who.PlatformAdmin {
		t.Fatalf("accepted identity: %+v %v", who, err)
	}
	w = memberHTTP(s, who, "GET", "/api/members", "")
	if w.Code != 403 {
		t.Fatalf("viewer members: %d", w.Code)
	}
	w = memberHTTP(s, who, "POST", "/api/api-keys", `{"name":"viewer","scope":"write","expiresAt":"`+time.Now().Add(time.Hour).UTC().Format(time.RFC3339)+`"}`)
	if w.Code != 403 {
		t.Fatalf("viewer write key: %d", w.Code)
	}
}

func TestMembersHTTPKeySecretsAndPublicAcceptanceInput(t *testing.T) {
	s, owner := membersHTTPServer(t)
	w := memberHTTP(s, owner, "POST", "/api/api-keys", `{"name":"reader","scope":"read","expiresAt":"`+time.Now().Add(time.Hour).UTC().Format(time.RFC3339)+`"}`)
	if w.Code != 201 {
		t.Fatalf("key: %d %s", w.Code, w.Body.String())
	}
	var result struct {
		Token string              `json:"token"`
		Key   platform.APIKeyInfo `json:"key"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.Token == "" {
		t.Fatal("missing key secret")
	}
	w = memberHTTP(s, owner, "GET", "/api/api-keys", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), result.Token) {
		t.Fatal("list leaks secret")
	}
	w = memberHTTP(s, owner, "POST", "/api/api-keys/"+result.Key.ID+"/revoke", `{"unknown":true}`)
	if w.Code != 400 {
		t.Fatalf("revoke strict decode: %d", w.Code)
	}
	w = memberHTTP(s, owner, "POST", "/api/api-keys/"+result.Key.ID+"/revoke", `{}`)
	if w.Code != 200 {
		t.Fatalf("revoke: %d", w.Code)
	}
	if _, err := s.store.APIKey(context.Background(), result.Token); err != platform.ErrUnauthorized {
		t.Fatalf("revoked key: %v", err)
	}
	for _, tc := range []struct {
		body string
		want int
	}{{`{"token":"x","email":"new@example.com","password":"long-new-password-42","role":"owner"}`, 400}, {`{} {}`, 400}, {`{"token":"` + strings.Repeat("x", 9000) + `"}`, 413}} {
		w = memberHTTP(s, platform.Identity{}, "POST", "/api/auth/invitations/accept", tc.body)
		if w.Code != tc.want {
			t.Fatalf("accept strict decode: %d want %d", w.Code, tc.want)
		}
	}
	if w = memberHTTP(s, owner, "GET", "/api/auth/invitations/accept", ""); w.Code != 405 {
		t.Fatalf("accept method: %d", w.Code)
	}
}

func TestMembersIntegratedBearerRulesAndPlatformDenial(t *testing.T) {
	s, owner := membersHTTPServer(t)
	ctx := context.Background()
	if err := s.store.SetPlatformAdmin(ctx, owner.Email, true); err != nil {
		t.Fatal(err)
	}
	key, writer, err := s.store.CreateAPIKey(ctx, owner, "integration writer", "write", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	_, reader, err := s.store.CreateAPIKey(ctx, owner, "integration reader", "read", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	h := New(s.store, nil, s.origin)
	call := func(method, path, token, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, s.origin+path, strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:45678"
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-Requested-With", "RelaDock")
		r.Header.Set("Origin", s.origin)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", "integrated-key-rule")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	body := `{"name":"key rule","protocol":"tcp","targetHost":"192.0.2.10","targetPort":443,"bandwidthMbps":100}`
	if w := call("POST", "/api/rules", reader, body); w.Code != 403 {
		t.Fatalf("read key created rule: %d %s", w.Code, w.Body.String())
	}
	if w := call("POST", "/api/rules", writer, body); w.Code != 201 {
		t.Fatalf("write key failed: %d %s", w.Code, w.Body.String())
	}
	for _, tc := range []struct{ method, path string }{{"GET", "/api/admin/overview"}, {"POST", "/api/auth/logout"}} {
		if w := call(tc.method, tc.path, writer, `{}`); w.Code != 403 {
			t.Fatalf("bearer platform/logout: %s %d", tc.path, w.Code)
		}
	}
	if w := call("GET", "/api/auth/me", writer, ""); w.Code != 200 || strings.Contains(w.Body.String(), `"platformAdmin":true`) {
		t.Fatalf("bearer platform identity: %d %s", w.Code, w.Body.String())
	}
	if err = s.store.RevokeAPIKey(ctx, owner, key.ID); err != nil {
		t.Fatal(err)
	}
	if w := call("GET", "/api/rules", writer, ""); w.Code != 401 {
		t.Fatalf("revoked bearer authenticated: %d", w.Code)
	}
}
