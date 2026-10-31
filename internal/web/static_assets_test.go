package web_test

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"reladock/internal/web"
	assets "reladock/public"
)

func TestAdminUpgradePresentationContract(t *testing.T) {
	adminJS, err := fs.ReadFile(assets.Files, "admin.js")
	if err != nil {
		t.Fatal(err)
	}
	page, err := fs.ReadFile(assets.Files, "admin.html")
	if err != nil {
		t.Fatal(err)
	}
	combined := string(adminJS) + string(page)
	for _, required := range []string{
		"升级到当前固定版本", "upgrade-cancel", "当前版本", "目标版本", "升级中，操作已锁定",
		"rollback-failed", "回滚失败，需要人工处理", "控制连接与配置轮询恢复", "不代表实际转发健康",
	} {
		if !strings.Contains(combined, required) {
			t.Errorf("admin upgrade presentation missing %q", required)
		}
	}
}

func TestAdminModuleGraphIsServedOverHTTP(t *testing.T) {
	handler := web.New(nil, assets.Files, "http://127.0.0.1:3001")
	for _, path := range []string{"/admin.js", "/node-bootstrap.js", "/node-contact-status.js", "/operations-tools.js"} {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:3001"+path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status=%d body=%q", path, rec.Code, rec.Body.String())
		}
		if contentType := rec.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/javascript") {
			t.Fatalf("GET %s content-type=%q", path, contentType)
		}
		if rec.Body.Len() == 0 {
			t.Fatalf("GET %s returned an empty module", path)
		}
	}
}
