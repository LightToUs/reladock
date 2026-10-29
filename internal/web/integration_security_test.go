package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestBearerIsTenantScopedAndCannotUsePlatformPrivileges(t *testing.T) {
	s := testWebStore(t)
	ctx := context.Background()
	owner, _, err := s.Register(ctx, "Operator", "bearer-admin@example.com", "long-test-password-42")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetPlatformAdmin(ctx, owner.Email, true); err != nil {
		t.Fatal(err)
	}
	h := New(s, fstest.MapFS{}, "http://127.0.0.1:3001")
	key, raw, err := s.CreateAPIKey(ctx, owner, "automation", "write", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	bearerRequest := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://127.0.0.1:3001"+path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+raw)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Requested-With", "RelaDock")
		r.Header.Set("Idempotency-Key", "bearer-create-rule")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	if r := bearerRequest("GET", "/api/admin/overview", ""); r.Code != 403 {
		t.Fatalf("key inherited platform privileges: %d", r.Code)
	}
	if r := bearerRequest("GET", "/api/auth/me", ""); r.Code != 200 {
		t.Fatal(r.Body.String())
	} else {
		var who map[string]any
		json.Unmarshal(r.Body.Bytes(), &who)
		if who["platformAdmin"] != false {
			t.Fatal("key elevated")
		}
	}
	if r := bearerRequest("POST", "/api/rules", `{"name":"key relay","protocol":"tcp","targetHost":"192.0.2.10","targetPort":443,"bandwidthMbps":10}`); r.Code != 201 {
		t.Fatal(r.Body.String())
	}
	if err = s.RevokeAPIKey(ctx, owner, key.ID); err != nil {
		t.Fatal(err)
	}
	if r := bearerRequest("GET", "/api/rules", ""); r.Code != 401 {
		t.Fatalf("revoked key: %d", r.Code)
	}
	read, readRaw, err := s.CreateAPIKey(ctx, owner, "read only", "read", time.Now().Add(time.Hour))
	_ = read
	if err != nil {
		t.Fatal(err)
	}
	raw = readRaw
	if r := bearerRequest("POST", "/api/rules", `{"name":"denied","protocol":"tcp","targetHost":"192.0.2.10","targetPort":443,"bandwidthMbps":10}`); r.Code != 403 {
		t.Fatalf("read key wrote rule: %d", r.Code)
	}
}

func TestProductionRateLimitIsSharedAcrossHandlers(t *testing.T) {
	s := testWebStore(t)
	a := NewWithOptions(s, fstest.MapFS{}, "http://127.0.0.1:3001", Options{PersistentRateLimit: true})
	b := NewWithOptions(s, fstest.MapFS{}, "http://127.0.0.1:3001", Options{PersistentRateLimit: true})
	for i := 0; i < 10; i++ {
		h := a
		if i%2 == 1 {
			h = b
		}
		rec := request(h, http.MethodPost, "/api/auth/login", `{}`, nil)
		if rec.Code != 401 {
			t.Fatalf("request %d: %d", i, rec.Code)
		}
	}
	if rec := request(b, http.MethodPost, "/api/auth/login", `{}`, nil); rec.Code != 429 {
		t.Fatalf("second process reset rate budget: %d", rec.Code)
	}
}
