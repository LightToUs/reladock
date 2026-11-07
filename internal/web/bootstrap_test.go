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

func TestLocalBootstrapDoesNotSuggestUsingProductionForLocalCredentials(t *testing.T) {
	h := New(nil, fstest.MapFS{}, "http://127.0.0.1:3012")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1:3012/install/agent.sh", nil))
	if w.Code != 503 || !strings.Contains(w.Body.String(), "本地测试") || !strings.Contains(w.Body.String(), "无需手动填写客户端证书") {
		t.Fatalf("unexpected local enrollment guidance: %d %s", w.Code, w.Body.String())
	}
}
