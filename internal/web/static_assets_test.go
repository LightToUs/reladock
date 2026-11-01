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
	actionDialogJS, err := fs.ReadFile(assets.Files, "node-action-dialog.js")
	if err != nil {
		t.Fatal(err)
	}
	combined := string(adminJS) + string(page) + string(actionDialogJS)
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
	actionDialogJS, err := fs.ReadFile(assets.Files, "node-action-dialog.js")
	if err != nil {
		t.Fatal(err)
	}
	combined := string(adminJS) + string(page) + string(actionDialogJS)
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
	for _, path := range []string{"/admin.js", "/console-navigation.js", "/node-action-dialog.js", "/node-bootstrap.js", "/node-contact-status.js", "/node-content-dialog.js", "/node-editor.js", "/node-withdrawal.js", "/operations-tools.js"} {
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

func TestEnterpriseRuleEditorModuleIsServedOverHTTP(t *testing.T) {
	handler := web.New(nil, assets.Files, "http://127.0.0.1:3001")
	for _, path := range []string{"/rule-editor.js", "/telemetry.js", "/node-monitoring.js"} {
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

func TestEnterpriseWorkspaceExposesOperationalSummaryAndRouteSignal(t *testing.T) {
	page, err := fs.ReadFile(assets.Files, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	app, err := fs.ReadFile(assets.Files, "app.js")
	if err != nil {
		t.Fatal(err)
	}
	combined := string(page) + string(app)
	for _, required := range []string{
		`class="workspace-summary"`, `id="summary-rule-total"`, `id="summary-rule-applied"`,
		`id="summary-rule-pending"`, `id="summary-node-contact"`, `class="relay-signal"`,
		"workspaceSummary", "ruleStatusPresentation",
	} {
		if !strings.Contains(combined, required) {
			t.Errorf("enterprise workspace redesign contract missing %q", required)
		}
	}
}

func TestEnterpriseWorkspaceExposesTrafficAndNodeResourceTelemetry(t *testing.T) {
	page, err := fs.ReadFile(assets.Files, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	app, err := fs.ReadFile(assets.Files, "app.js")
	if err != nil {
		t.Fatal(err)
	}
	telemetry, err := fs.ReadFile(assets.Files, "telemetry.js")
	if err != nil {
		t.Fatal(err)
	}
	combined := string(page) + string(app) + string(telemetry)
	for _, required := range []string{
		`id="telemetry-section"`, `id="telemetry-rule"`, `id="telemetry-window"`,
		`id="traffic-trend"`, `id="node-resource-cards"`, `id="startup-error"`, "telemetryPresentation",
		"/api/billing/telemetry", "/api/operations/nodes?staleAfterSeconds=90",
		"平台接收时间", "空档表示没有已接收报告，不推断为 0",
	} {
		if !strings.Contains(combined, required) {
			t.Errorf("enterprise telemetry presentation missing %q", required)
		}
	}
}

func TestEnterpriseWorkspaceExposesLiveNodeStatus(t *testing.T) {
	page, err := fs.ReadFile(assets.Files, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	app, err := fs.ReadFile(assets.Files, "app.js")
	if err != nil {
		t.Fatal(err)
	}
	combined := string(page) + string(app)
	for _, required := range []string{
		`href="#/node-status"`, `id="node-status-section"`, `id="node-status-rows"`, `id="node-status-message"`,
		"节点状态", "实时速率", "累计流量", "Agent 运行", "连接跟踪", "nodeMonitoringPresentation", "createAdaptivePoller",
		"/api/operations/nodes?staleAfterSeconds=90", "renderEnterpriseNodeMonitoring", "loadAllOperationNodes", "cycle.isCurrent()",
	} {
		if !strings.Contains(combined, required) {
			t.Errorf("enterprise live node status missing %q", required)
		}
	}
}

func TestEnterpriseWorkspaceKeepsRuleTableIDUnique(t *testing.T) {
	page, err := fs.ReadFile(assets.Files, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	source := string(page)
	if count := strings.Count(source, `id="rules"`); count != 1 {
		t.Fatalf("rule table id must be unique, got %d occurrences", count)
	}
	if !strings.Contains(source, `href="#/rules"`) || !strings.Contains(source, `id="rules-section"`) {
		t.Fatal("rule navigation must target the dedicated rules page route")
	}
}

func TestEnterpriseWorkspaceUsesRoutedPagesAndCollapsibleSidebar(t *testing.T) {
	page, err := fs.ReadFile(assets.Files, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	app, err := fs.ReadFile(assets.Files, "app.js")
	if err != nil {
		t.Fatal(err)
	}
	css, err := fs.ReadFile(assets.Files, "app.css")
	if err != nil {
		t.Fatal(err)
	}
	combined := string(page) + string(app) + string(css)
	for _, required := range []string{
		`id="workspace-sidebar"`, `id="workspace-nav-collapse"`, `id="workspace-nav-mobile"`,
		`data-view="overview"`, `data-view="rules"`, `data-view="team"`,
		`data-pane="overview"`, `data-pane="rules"`, `data-pane="team"`,
		`href="#/overview"`, `href="#/rules"`, `href="#/operations"`,
		"bindConsoleNavigation", "sidebar-collapsed", "nav-open",
	} {
		if !strings.Contains(combined, required) {
			t.Errorf("enterprise routed navigation missing %q", required)
		}
	}
}

func TestEnterpriseRuleManagementUsesOneCreateEditDialog(t *testing.T) {
	page, err := fs.ReadFile(assets.Files, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	app, err := fs.ReadFile(assets.Files, "app.js")
	if err != nil {
		t.Fatal(err)
	}
	combined := string(page) + string(app)
	for _, required := range []string{`<dialog id="rule-dialog"`, `id="rule-create"`, "createRuleEditor", "ruleEditor.openEdit(rule)", "method: 'PATCH'"} {
		if !strings.Contains(combined, required) {
			t.Errorf("enterprise rule dialog contract missing %q", required)
		}
	}
	if strings.Contains(combined, `<section><h2>创建中转规则</h2>`) {
		t.Fatal("the enterprise workspace still exposes the old inline create form")
	}
}

func TestNodeRowContentActionsUseDialogs(t *testing.T) {
	page, err := fs.ReadFile(assets.Files, "admin.html")
	if err != nil {
		t.Fatal(err)
	}
	adminJS, err := fs.ReadFile(assets.Files, "admin.js")
	if err != nil {
		t.Fatal(err)
	}
	combined := string(page) + string(adminJS)
	for _, required := range []string{
		`<dialog id="certificate-dialog"`, `<dialog id="node-delivery-dialog"`,
		"bindNodeContentDialog", "bindNodeDeliveryDialog", "deliveryDialog.open(node)",
	} {
		if !strings.Contains(combined, required) {
			t.Errorf("node-row dialog contract missing %q", required)
		}
	}
	if strings.Contains(combined, `id="certificate-panel"`) {
		t.Fatal("certificate editor remains an inline panel")
	}
}

func TestNodeDialogConflictRefreshStaysInsideSessionGuard(t *testing.T) {
	adminJS, err := fs.ReadFile(assets.Files, "admin.js")
	if err != nil {
		t.Fatal(err)
	}
	source := string(adminJS)
	start := strings.Index(source, "async function mutate")
	end := strings.Index(source, "const retirementReasons")
	if start < 0 || end <= start {
		t.Fatal("cannot locate mutate implementation")
	}
	mutateSource := source[start:end]
	for _, forbidden := range []string{"editNode(", "editCertificate("} {
		if strings.Contains(mutateSource, forbidden) {
			t.Fatalf("mutate refreshes dialog outside its submission session via %q", forbidden)
		}
	}
	for _, required := range []string{"runDialogSession(nodeEditor", "runDialogSession(certificateDialog", "if (latest) editNode(latest)", "if (latest) editCertificate(latest)"} {
		if !strings.Contains(source, required) {
			t.Errorf("session-guarded conflict refresh missing %q", required)
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
	navigationJS, err := fs.ReadFile(assets.Files, "console-navigation.js")
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

func TestAdminConsoleUsesRoutedPagesAndCollapsibleSidebar(t *testing.T) {
	page, err := fs.ReadFile(assets.Files, "admin.html")
	if err != nil {
		t.Fatal(err)
	}
	adminJS, err := fs.ReadFile(assets.Files, "admin.js")
	if err != nil {
		t.Fatal(err)
	}
	css, err := fs.ReadFile(assets.Files, "admin.css")
	if err != nil {
		t.Fatal(err)
	}
	combined := string(page) + string(adminJS) + string(css)
	for _, required := range []string{
		`id="admin-sidebar-collapse"`, `id="admin-nav-toggle"`, `id="admin-view-description"`,
		`data-view="overview"`, `href="#/overview"`, `href="#/nodes"`, `href="#/audit"`,
		"bindConsoleNavigation", "sidebar-collapsed", "#admin-shell.nav-open .admin-nav nav", "adminNavigation.select",
	} {
		if !strings.Contains(combined, required) {
			t.Errorf("admin routed navigation missing %q", required)
		}
	}
}

func TestAdminSummaryUsesConciseSuspendedTenantLabel(t *testing.T) {
	adminJS, err := fs.ReadFile(assets.Files, "admin.js")
	if err != nil {
		t.Fatal(err)
	}
	source := string(adminJS)
	if !strings.Contains(source, "`${data.suspendedTenants} 家已停用`") {
		t.Fatal("admin summary does not use the concise suspended-tenant label")
	}
	if strings.Contains(source, "`${data.suspendedTenants} 家已停用登录`") {
		t.Fatal("admin summary still includes the redundant login suffix")
	}
}

func TestAdminNodeListExposesLiveMonitoringTargetsWithoutFullReload(t *testing.T) {
	page, err := fs.ReadFile(assets.Files, "admin.html")
	if err != nil {
		t.Fatal(err)
	}
	app, err := fs.ReadFile(assets.Files, "admin.js")
	if err != nil {
		t.Fatal(err)
	}
	combined := string(page) + string(app)
	for _, required := range []string{
		"实时速率", "累计流量", "Agent 运行", "动态资源", `id="admin-node-monitoring-message"`,
		"data-monitoring", "patchAdminNodeMonitoring", "startAdminNodeMonitoring", "nodeMonitoringPresentation", "createAdaptivePoller",
		"/api/admin/operations/nodes?staleAfterSeconds=90", "loadAllOperationNodes", "cycle.isCurrent()",
	} {
		if !strings.Contains(combined, required) {
			t.Errorf("admin live node monitoring missing %q", required)
		}
	}
	start := strings.Index(string(app), "function startAdminNodeMonitoring")
	if start < 0 {
		t.Fatal("cannot locate admin monitoring poller")
	}
	end := strings.Index(string(app)[start:], "\n}")
	if end < 0 {
		t.Fatal("cannot locate admin monitoring poller end")
	}
	if strings.Contains(string(app)[start:start+end], "reload(") {
		t.Fatal("monitoring poller must patch cells without rebuilding management controls")
	}
}

func TestConsoleThemesUseBluePrimaryPalettes(t *testing.T) {
	enterpriseCSS, err := fs.ReadFile(assets.Files, "app.css")
	if err != nil {
		t.Fatal(err)
	}
	adminCSS, err := fs.ReadFile(assets.Files, "admin.css")
	if err != nil {
		t.Fatal(err)
	}
	enterprisePage, err := fs.ReadFile(assets.Files, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	adminPage, err := fs.ReadFile(assets.Files, "admin.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(enterpriseCSS), "--accent: #3b82f6") || !strings.Contains(string(adminCSS), "--accent: #2563eb") {
		t.Fatal("console primary tokens are not blue")
	}
	if !strings.Contains(string(enterprisePage), `name="theme-color" content="#07111f"`) || !strings.Contains(string(adminPage), `name="theme-color" content="#081a33"`) {
		t.Fatal("console browser theme colors are not blue")
	}
	for _, oldPrimary := range []string{"#c7f75a", "#dbff8d", "#16815f", "#48c89e"} {
		if strings.Contains(string(enterpriseCSS), oldPrimary) || strings.Contains(string(adminCSS), oldPrimary) {
			t.Errorf("old green primary color remains: %s", oldPrimary)
		}
	}
	for _, semantic := range []string{"--warning:", "--danger:"} {
		if !strings.Contains(string(enterpriseCSS), semantic) || !strings.Contains(string(adminCSS), semantic) {
			t.Errorf("alert semantic token missing: %s", semantic)
		}
	}
}
