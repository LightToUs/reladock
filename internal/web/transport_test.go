package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func TestHTTPSOriginCookieAndUntrustedForwarding(t *testing.T) {
	s := &server{origin: "https://console.example.com"}
	r := httptest.NewRecorder()
	s.sessionCookie(r, "test-token")
	if c := r.Result().Cookies()[0]; !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
		t.Fatal("HTTPS cookie protections missing")
	}
	h := New(nil, fstest.MapFS{}, "https://console.example.com")
	req := httptest.NewRequest("GET", "https://attacker.example/", nil)
	req.Header.Set("X-Forwarded-Host", "console.example.com")
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatal("forwarded host bypassed Host check")
	}
	if rec.Header().Get("Strict-Transport-Security") == "" {
		t.Fatal("missing HSTS")
	}
}
