package web

import (
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestBootstrapDisabledDoesNotExposeInstaller(t *testing.T) {
	h := New(nil, fstest.MapFS{}, "https://console.example.com")
	for _, path := range []string{"/install/agent.sh", "/api/node-bootstrap/release", "/api/node-bootstrap/redeem"} {
		r := httptest.NewRequest("GET", "https://console.example.com"+path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 503 || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), "未配置") {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
}
