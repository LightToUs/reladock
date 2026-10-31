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

func TestAdminRetirementPresentationContract(t *testing.T) {
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
		"排空中", "排空完成 · 可最终退役", "进入安全排空", "最终退役节点", "不可恢复",
		"撤销 Agent 身份", "仍有未删除规则", "等待最新 Agent 排空证明", "不提供强制释放",
	} {
		if !strings.Contains(combined, required) {
			t.Errorf("admin retirement presentation missing %q", required)
		}
	}
	if strings.Contains(strings.ToLower(combined), "force release") || strings.Contains(combined, "强制释放端口") {
		t.Fatal("admin exposes a force-release path")
	}
}

func TestAdminModuleGraphIsServedOverHTTP(t *testing.T) {
	handler := web.New(nil, assets.Files, "http://127.0.0.1:3001")
	for _, path := range []string{"/admin.js", "/admin-navigation.js", "/node-bootstrap.js", "/node-contact-status.js", "/operations-tools.js"} {
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

func TestAdminMobileNavigationHasExplicitToggle(t *testing.T) {
	page, err := fs.ReadFile(assets.Files, "admin.html")
	if err != nil {
		t.Fatal(err)
	}
	adminJS, err := fs.ReadFile(assets.Files, "admin.js")
	if err != nil {
		t.Fatal(err)
	}
	navigationJS, err := fs.ReadFile(assets.Files, "admin-navigation.js")
	if err != nil {
		t.Fatal(err)
	}
	combined := string(page) + string(adminJS) + string(navigationJS)
	for _, required := range []string{
		`id="admin-nav-toggle"`, `aria-controls="admin-navigation"`, `aria-expanded="false"`,
		"nav-open", "aria-expanded", "admin-navigation",
	} {
		if !strings.Contains(combined, required) {
			t.Errorf("admin mobile navigation missing %q", required)
		}
	}
}
