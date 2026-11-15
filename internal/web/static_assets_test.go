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

func TestAdminNodeVisibilityHasARecoverablePresentation(t *testing.T) {
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
		`id="nodes-show-hidden"`, "显示已隐藏节点", "隐藏节点", "恢复显示",
		"不删除客户规则", "visibilityRevision", "/visibility", "nodesForAdminList",
	} {
		if !strings.Contains(combined, required) {
			t.Errorf("admin node visibility presentation missing %q", required)
		}
	}
}

func TestAdminModuleGraphIsServedOverHTTP(t *testing.T) {
	handler := web.New(nil, assets.Files, "http://127.0.0.1:3001")
	for _, path := range []string{"/admin.js", "/auth-session.js", "/account-menu.js", "/console-navigation.js", "/lightsail-admin.js", "/node-action-dialog.js", "/node-bootstrap.js", "/node-contact-status.js", "/node-content-dialog.js", "/node-editor.js", "/node-visibility.js", "/node-withdrawal.js", "/operations-tools.js", "/resource-mode.js", "/source-flow-logging.js"} {
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

func TestSharedBrandMarkIsServedOverHTTP(t *testing.T) {
	handler := web.New(nil, assets.Files, "http://127.0.0.1:3001")
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		req := httptest.NewRequest(method, "http://127.0.0.1:3001/reladock-mark.svg", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s /reladock-mark.svg status=%d body=%q", method, rec.Code, rec.Body.String())
		}
		if contentType := rec.Header().Get("Content-Type"); contentType != "image/svg+xml" {
			t.Fatalf("%s /reladock-mark.svg content-type=%q", method, contentType)
		}
		if method == http.MethodGet && !strings.Contains(rec.Body.String(), "#2563EB") {
			t.Fatal("GET /reladock-mark.svg did not return the blue brand mark")
		}
		if method == http.MethodHead && rec.Body.Len() != 0 {
			t.Fatal("HEAD /reladock-mark.svg returned a response body")
		}
	}
}

func TestTronAutomaticPaymentPresentationIsServed(t *testing.T) {
	handler := web.New(nil, assets.Files, "http://127.0.0.1:3001")
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:3001/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status=%d", rec.Code)
	}
	page := rec.Body.String()
	for _, required := range []string{"tron-payment-address-label", "tron-payment-address", "tron-payment-qr", "tron-payment-amount", "tron-payment-network", "tron-payment-countdown", "tron-payment-success", "tron-payment-copy-address", "未自动到账？", "自动识别", "网络与代币信息"} {
		if !strings.Contains(page, required) {
			t.Errorf("TRON automatic payment UI missing %q", required)
		}
	}
	if strings.Contains(page, "tron-payment-copy-amount") || strings.Contains(page, "复制金额") {
		t.Fatal("TRON payment UI still offers amount copying")
	}
	if strings.Contains(page, "tron-payment-expiry") || strings.Contains(page, "订单截止时间") {
		t.Fatal("TRON payment UI repeats an absolute deadline below the validity countdown")
	}
	if strings.Count(page, `id="tron-payment-address"`) != 1 {
		t.Fatal("TRON payment UI must render the collection address exactly once")
	}
	ordered := []string{`id="tron-payment-address-label"`, `id="tron-payment-address"`, `id="tron-payment-copy-address"`, `id="tron-payment-qr"`, `id="tron-payment-amount"`, `id="tron-payment-network"`, `id="tron-payment-countdown"`}
	previous := -1
	for _, marker := range ordered {
		current := strings.Index(page, marker)
		if current <= previous {
			t.Fatalf("TRON primary payment hierarchy is out of order at %q", marker)
		}
		previous = current
	}
	contract := strings.Index(page, "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t")
	countdown := strings.Index(page, `id="tron-payment-countdown"`)
	if contract < countdown {
		t.Fatal("official USDT contract appears before the payment validity block")
	}
}

func TestEnterpriseAuthenticationOffersPasswordVisibilityControls(t *testing.T) {
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
		`id="auth-password"`, `id="auth-password-toggle"`,
		`id="invitation-password"`, `id="invitation-password-toggle"`,
		`type="button" aria-controls="auth-password" aria-pressed="false"`,
		`type="button" aria-controls="invitation-password" aria-pressed="false"`,
		"bindPasswordVisibility", "authPasswordVisibility.hide()", "invitationPasswordVisibility.hide()",
	} {
		if !strings.Contains(combined, required) {
			t.Errorf("password visibility control missing %q", required)
		}
	}
}

func TestEnterpriseOverviewOwnsFinanceAndAccountOffersPasswordChange(t *testing.T) {
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
		`data-view="billing"`, `data-pane="overview"`, `id="wallet-status"`,
		`data-account-view="password"`, `id="password-dialog"`, `id="password-form"`,
		`/api/auth/password`, "修改密码",
	} {
		if !strings.Contains(combined, required) {
			t.Errorf("overview/account password presentation missing %q", required)
		}
	}
	if strings.Contains(string(page), `data-view="finance"`) || strings.Contains(string(page), `data-pane="finance"`) {
		t.Fatal("finance remains a primary navigation pane")
	}
}

func TestResourceModeControlsAndIndependentPackageCopyAreStatic(t *testing.T) {
	adminPage, err := fs.ReadFile(assets.Files, "admin.html")
	if err != nil {
		t.Fatal(err)
	}
	adminJS, err := fs.ReadFile(assets.Files, "admin.js")
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := fs.ReadFile(assets.Files, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	business, err := fs.ReadFile(assets.Files, "business-tools.js")
	if err != nil {
		t.Fatal(err)
	}
	planCatalog, err := fs.ReadFile(assets.Files, "plan-catalog.js")
	if err != nil {
		t.Fatal(err)
	}
	combinedAdmin := string(adminPage) + string(adminJS) + string(planCatalog)
	for _, required := range []string{`name="resourceMode"`, `name="dedicatedTenantId"`, "共享资源", "企业专属资源", "规则专属资源", "dedicatedTenantId", "resourceModeLabel", "tenant.id"} {
		if !strings.Contains(combinedAdmin, required) {
			t.Errorf("admin resource mode presentation missing %q", required)
		}
	}
	for _, required := range []string{`id="offering-form"`, `name="offeringId"`, `name="sourceMarket"`, `name="destinationMarket"`, "/api/admin/offerings", "adminPlanOfferingLabel", "历史版本 · 未绑定线路 · 不可新购"} {
		if !strings.Contains(combinedAdmin, required) {
			t.Errorf("admin line offering presentation missing %q", required)
		}
	}
	for _, required := range []string{`id="offering-new"`, `id="offering-dialog"`, `name="notes"`, `id="offering-form-message"`, `id="offering-action-dialog"`, `id="offering-action-status"`, "/api/admin/offerings/delete", "bindNodeActionDialog"} {
		if !strings.Contains(combinedAdmin, required) {
			t.Errorf("admin offering dialog presentation missing %q", required)
		}
	}
	for _, required := range []string{`id="plan-new"`, `id="plan-dialog"`, `id="plan-form-message"`, `id="plan-dialog-cancel"`, `id="plan-save"`, "planCreateRunner"} {
		if !strings.Contains(combinedAdmin, required) {
			t.Errorf("admin plan dialog presentation missing %q", required)
		}
	}
	planDialogStart := strings.Index(string(adminPage), `<dialog id="plan-dialog"`)
	planFormStart := strings.Index(string(adminPage), `<form id="plan-form">`)
	if planDialogStart < 0 || planFormStart < planDialogStart {
		t.Error("plan creation form is not inside its dialog")
	}
	if strings.Contains(string(adminPage), `name="sourceMarket" maxlength="80" required`) || strings.Contains(string(adminPage), `name="destinationMarket" maxlength="80" required`) {
		t.Error("optional offering market fields are still required")
	}
	planFormEnd := strings.Index(string(adminPage)[planFormStart:], `</form>`)
	if planFormStart < 0 || planFormEnd < 0 || strings.Contains(string(adminPage)[planFormStart:planFormStart+planFormEnd], `name="resourceMode"`) {
		t.Error("plan form still accepts an independent resource mode")
	}
	planForm := string(adminPage)[planFormStart : planFormStart+planFormEnd]
	if strings.Contains(planForm, `name="version"`) || !strings.Contains(planForm, `name="priceUSD"`) || !strings.Contains(planForm, `价格 / 美元`) {
		t.Error("plan form still asks for a version or does not accept a USD price")
	}
	combinedWorkspace := string(workspace) + string(business)
	for _, required := range []string{"/api/plans", "planPurchaseCard", "入口节点"} {
		if !strings.Contains(combinedWorkspace, required) {
			t.Errorf("workspace package mode presentation missing %q", required)
		}
	}
}

func TestEnterpriseBillingUsesPurchaseCardsAndConfirmationDialog(t *testing.T) {
	page, err := fs.ReadFile(assets.Files, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	business, err := fs.ReadFile(assets.Files, "business-tools.js")
	if err != nil {
		t.Fatal(err)
	}
	app, err := fs.ReadFile(assets.Files, "app.js")
	if err != nil {
		t.Fatal(err)
	}
	combined := string(page) + string(business) + string(app)
	for _, required := range []string{
		`id="plan-line-filters"`, `id="plan-card-grid"`,
		`id="purchase-dialog"`, `id="purchase-form"`, `id="purchase-dialog-status"`,
		`data-pane="checkout"`, `id="checkout-balance-pay"`, `id="checkout-external-pay"`,
		"购买套餐", "planPurchaseCard", "purchaseDialogSelection", "先购买套餐",
		"余额支付", "差额支付", "paymentCheckoutPresentation", "planPaymentOrderPresentation", "await preparePaymentCheckout(refresh", "支付状态刷新失败", "workspaceNavigation.select", "resetBusinessTools", "workspaceRefreshGate", "authSessionGate", "logoutGuard",
		"/api/payments/plan-checkout", "/api/payments/orders?purpose=plan-purchase", "验签并成功购买后才开始计时", "节点未部署时等待交付", "外部页面返回不代表成功", "资金会保留在企业钱包",
	} {
		if !strings.Contains(combined, required) {
			t.Errorf("card purchase flow missing %q", required)
		}
	}
	dialogStart := strings.Index(string(page), `<dialog id="purchase-dialog"`)
	formStart := strings.Index(string(page), `<form id="purchase-form"`)
	if dialogStart < 0 || formStart < dialogStart {
		t.Error("purchase confirmation form is not inside its dialog")
	}
	if strings.Contains(string(page), `name="priceVersionId" required></select>`) {
		t.Error("billing page still exposes the old package select")
	}
}

func TestEnterpriseRuleEditorModuleIsServedOverHTTP(t *testing.T) {
	handler := web.New(nil, assets.Files, "http://127.0.0.1:3001")
	for _, path := range []string{"/rule-editor.js", "/telemetry.js", "/node-monitoring.js", "/plan-catalog.js"} {
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

func TestEnterpriseWorkspaceExposesOperationalSummary(t *testing.T) {
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
		`id="summary-rule-pending"`, `id="summary-node-contact"`,
		"workspaceSummary", "ruleStatusPresentation",
	} {
		if !strings.Contains(combined, required) {
			t.Errorf("enterprise workspace redesign contract missing %q", required)
		}
	}
}

func TestEnterpriseWorkspaceRemovesDecorativeCopyAndKeepsOperationalBoundaries(t *testing.T) {
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
	pageSource := string(page)
	for _, decorative := range []string{
		`class="command-deck"`, `class="section-index"`, `class="sidebar-note"`,
		`id="workspace-view-description"`, "Relay operations", "Enterprise relay control",
		"SECURE CHECKOUT", "成员、邀请与 API Key", "企业组网预留：",
		"当前企业配置", "节点数 / 节点总数",
	} {
		if strings.Contains(pageSource, decorative) {
			t.Errorf("enterprise workspace still displays decorative or repeated copy %q", decorative)
		}
	}
	for _, boundary := range []string{
		"配置已应用、节点在线和目标可达均不代表端到端健康。",
		"暂停保留端口",
		"邀请令牌只显示一次", "节点未部署时等待交付",
		"最终状态只以服务端验签、钱包账本和套餐订单为准",
	} {
		if !strings.Contains(pageSource, boundary) {
			t.Errorf("enterprise workspace removed required operational boundary %q", boundary)
		}
	}
	apiKeyStart := strings.Index(pageSource, `<h2>API Key</h2>`)
	if apiKeyStart < 0 {
		t.Fatal("API Key section is missing")
	}
	apiKeyEnd := strings.Index(pageSource[apiKeyStart:], `</section>`)
	if apiKeyEnd < 0 {
		t.Fatal("API Key section is not closed")
	}
	apiKeySection := pageSource[apiKeyStart : apiKeyStart+apiKeyEnd]
	for _, boundary := range []string{"Key 仅限当前企业，不能操作平台管理", "令牌只显示一次"} {
		if !strings.Contains(apiKeySection, boundary) {
			t.Errorf("API Key section removed required boundary %q", boundary)
		}
	}
	if strings.Contains(string(app), "descriptions: {") {
		t.Fatal("enterprise router still maintains hidden page-description copy")
	}
	for _, deadSelector := range []string{".command-deck", ".relay-signal", ".route-point", ".route-track", ".workspace-description", ".workspace-label", ".section-index", ".sidebar-note", ".signal-dot", ".account-menu-item span", "\nfooter {"} {
		if strings.Contains(string(css), deadSelector) {
			t.Errorf("removed copy still leaves dead presentation selector %q", deadSelector)
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
	monitoring, err := fs.ReadFile(assets.Files, "node-monitoring.js")
	if err != nil {
		t.Fatal(err)
	}
	combined := string(page) + string(app) + string(monitoring)
	for _, required := range []string{
		`href="#/node-status"`, `id="node-status-section"`, `id="node-status-rows"`, `id="node-status-message"`,
		"节点状态", "实时速率", "累计流量", "Agent 运行", "连接跟踪", "nodeMonitoringPresentation", "createAdaptivePoller",
		"/api/operations/nodes?staleAfterSeconds=90", "renderEnterpriseNodeMonitoring", "loadAllOperationNodes", "formatEntryAddress", "node.publicAddress", "入口地址待交付", "cycle.isCurrent()",
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
		`data-view="overview"`, `data-view="rules"`, `data-view="node-status"`, `data-view="billing"`,
		`data-pane="overview"`, `data-pane="rules"`, `data-pane="team"`, `data-pane="audit"`, `data-pane="operations"`,
		`href="#/overview"`, `href="#/rules"`, `id="wallet-status"`,
		`id="account-menu-trigger"`, `id="account-menu"`, `id="account-role"`,
		`data-account-view="team"`, `data-account-view="audit"`, `data-account-view="operations"`,
		"bindConsoleNavigation", "bindAccountMenu", "sidebar-collapsed", "nav-open",
	} {
		if !strings.Contains(combined, required) {
			t.Errorf("enterprise routed navigation missing %q", required)
		}
	}
	navStart := strings.Index(string(page), `<nav id="workspace-navigation"`)
	navEnd := strings.Index(string(page)[navStart:], `</nav>`)
	if navStart < 0 || navEnd < 0 {
		t.Fatal("enterprise primary navigation is missing")
	}
	nav := string(page)[navStart : navStart+navEnd]
	if count := strings.Count(nav, `data-view=`); count != 4 {
		t.Fatalf("enterprise primary navigation must contain exactly four flat routes, got %d", count)
	}
	for _, removed := range []string{`data-view="traffic"`, `data-view="delivery"`, `data-view="team"`, `data-view="operations"`} {
		if strings.Contains(nav, removed) {
			t.Errorf("low-frequency or merged route remains in the primary navigation: %s", removed)
		}
	}
	for _, merged := range []string{`id="telemetry-section" class="telemetry-section" data-pane="rules"`, `id="nodes-section" data-pane="rules"`} {
		if !strings.Contains(string(page), merged) {
			t.Errorf("rule workspace merge missing %q", merged)
		}
	}
}

func TestEnterpriseAccountPopoverUsesDisclosureSemantics(t *testing.T) {
	page, err := fs.ReadFile(assets.Files, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	source := string(page)
	for _, required := range []string{`id="account-menu-trigger"`, `aria-controls="account-menu"`, `aria-expanded="false"`} {
		if !strings.Contains(source, required) {
			t.Errorf("account disclosure missing %q", required)
		}
	}
	for _, invalid := range []string{`aria-haspopup="menu"`, `role="menu"`, `role="menuitem"`} {
		if strings.Contains(source, invalid) {
			t.Errorf("account disclosure must not claim an unimplemented ARIA menu model: %s", invalid)
		}
	}
}

func TestEnterpriseLogoutClosesAccountDisclosureBeforeRequest(t *testing.T) {
	app, err := fs.ReadFile(assets.Files, "app.js")
	if err != nil {
		t.Fatal(err)
	}
	source := string(app)
	start := strings.Index(source, `$('#logout').addEventListener('click'`)
	if start < 0 {
		t.Fatal("logout handler is missing")
	}
	end := strings.Index(source[start:], `function cell(`)
	if end < 0 {
		t.Fatal("logout handler boundary is missing")
	}
	handler := source[start : start+end]
	closeIndex := strings.Index(handler, "accountMenu.close()")
	requestIndex := strings.Index(handler, "await api('/api/auth/logout'")
	if closeIndex < 0 || requestIndex < 0 || closeIndex > requestIndex {
		t.Fatal("logout must close the account disclosure before starting the request")
	}
}

func TestEnterpriseOverviewOwnsFinanceStatusMessages(t *testing.T) {
	page, err := fs.ReadFile(assets.Files, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	business, err := fs.ReadFile(assets.Files, "business-tools.js")
	if err != nil {
		t.Fatal(err)
	}
	financeStart := strings.Index(string(page), `<section class="finance-center" data-pane="overview"`)
	if financeStart < 0 {
		t.Fatal("overview finance section is missing")
	}
	financeEnd := strings.Index(string(page)[financeStart:], `</section>`)
	if financeEnd < 0 {
		t.Fatal("finance pane is not closed")
	}
	finance := string(page)[financeStart : financeStart+financeEnd]
	if !strings.Contains(finance, `id="finance-message"`) {
		t.Fatal("finance actions need a visible status region inside the finance pane")
	}
	if count := strings.Count(string(business), `setBusinessStatus('finance'`); count < 3 {
		t.Fatalf("top-up and ledger outcomes must target finance status, got %d calls", count)
	}
}

func TestEnterpriseTopUpUsesUSDInput(t *testing.T) {
	page, err := fs.ReadFile(assets.Files, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	business, err := fs.ReadFile(assets.Files, "business-tools.js")
	if err != nil {
		t.Fatal(err)
	}
	financeStart := strings.Index(string(page), `<section class="finance-center" data-pane="overview"`)
	if financeStart < 0 {
		t.Fatal("overview finance section is missing")
	}
	financeEnd := strings.Index(string(page)[financeStart:], `</section>`)
	if financeEnd < 0 {
		t.Fatal("finance pane is not closed")
	}
	finance := string(page)[financeStart : financeStart+financeEnd]
	for _, required := range []string{`充值金额 / USD`, `name="amountUSD"`, `min="0.50"`, `max="10000.00"`, `step="0.01"`} {
		if !strings.Contains(finance, required) {
			t.Errorf("USD top-up contract missing %q", required)
		}
	}
	for _, forbidden := range []string{`充值金额 / 美分`, `name="amountCents"`} {
		if strings.Contains(finance, forbidden) {
			t.Errorf("customer-facing top-up still exposes cents: %q", forbidden)
		}
	}
	if !strings.Contains(string(business), `usdInputToCents(new FormData(event.target).get('amountUSD'))`) {
		t.Fatal("top-up handler must convert the USD field to integer cents before the API request")
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

func TestLightsailAdminPresentationIsSeparateAndExplicitlyAtMostOnce(t *testing.T) {
	page, err := fs.ReadFile(assets.Files, "admin.html")
	if err != nil {
		t.Fatal(err)
	}
	adminJS, err := fs.ReadFile(assets.Files, "admin.js")
	if err != nil {
		t.Fatal(err)
	}
	lightsailJS, err := fs.ReadFile(assets.Files, "lightsail-admin.js")
	if err != nil {
		t.Fatal(err)
	}
	combined := string(page) + string(adminJS) + string(lightsailJS)
	for _, required := range []string{
		`data-view="lightsail"`, `data-pane="lightsail"`, `id="lightsail-create-dialog"`,
		`id="lightsail-stop-dialog"`, `id="lightsail-destroy-dialog"`, `name="script"`,
		`maxlength="65536"`, "只执行一次", "未知", "不代表 RelaDock 转发或端到端健康",
		"./lightsail-admin.js", "bindLightsailAdmin", "createAdaptivePoller",
	} {
		if !strings.Contains(combined, required) {
			t.Errorf("Lightsail admin presentation missing %q", required)
		}
	}
	for _, forbidden := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", `name="privateKey"`, `name="awsAccessKey"`, `name="awsSecretKey"`} {
		if strings.Contains(combined, forbidden) {
			t.Errorf("Lightsail admin presentation exposes forbidden credential field %q", forbidden)
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
	if strings.Contains(string(page), "<th>节点资源</th>") {
		t.Fatal("admin node list still exposes the redundant node-resource column")
	}
	if strings.Contains(string(app), "cell(row, resourceStatus.label") {
		t.Fatal("admin node renderer still adds the redundant node-resource cell")
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

func TestEnterpriseConsoleKeepsDarkSidebarAndUsesLightWorkspace(t *testing.T) {
	css, err := fs.ReadFile(assets.Files, "app.css")
	if err != nil {
		t.Fatal(err)
	}
	source := string(css)
	for _, required := range []string{
		"--sidebar: #0a1628",
		"aside { position: fixed;",
		"background: var(--sidebar)",
		"#console main {",
		"color-scheme: light",
		"--canvas: #f4f7fb",
		"--surface: #ffffff",
		"--text: #16243a",
		"background: var(--canvas)",
		"#console main .workspace-summary article,",
		"#console main td,",
		"#console main .plan-purchase-card,",
		"#console main .checkout-order-summary > div",
		"#console main .editor-dialog",
		"#console main { --workspace-gutter: 20px; width: 100%; margin: 0; padding: 0 var(--workspace-gutter) 52px; }",
	} {
		if !strings.Contains(source, required) {
			t.Errorf("enterprise light workspace contract missing %q", required)
		}
	}
	if strings.Contains(source, "#console aside {\n  color-scheme: light") {
		t.Fatal("enterprise light theme must not recolor the existing dark sidebar")
	}
	if strings.Contains(source, "#console main .auth") {
		t.Fatal("enterprise light theme must not recolor the standalone login page")
	}
}

func TestEnterpriseConsoleUsesHarmonizedWorkspaceComponents(t *testing.T) {
	css, err := fs.ReadFile(assets.Files, "app.css")
	if err != nil {
		t.Fatal(err)
	}
	source := string(css)
	for _, required := range []string{
		"--radius-panel: 12px",
		"--radius-control: 8px",
		"#console main h1,",
		"#console main section,",
		"#console main .account-menu-trigger",
		"#console main .account-menu {",
		"#console main .account-menu-item:focus-visible",
		"#console main .account-avatar",
		"outline: 2px solid var(--accent)",
		"#console main .account-menu { right: 0; left: auto; }",
	} {
		if !strings.Contains(source, required) {
			t.Errorf("harmonized enterprise workspace style missing %q", required)
		}
	}
	if strings.Contains(source, "#console main .account-menu {\n  background: linear-gradient") {
		t.Fatal("enterprise account menu must use a flat surface, not a gradient")
	}
}

func TestEnterpriseWorkspaceUsesCompactStickyTopBar(t *testing.T) {
	page, err := fs.ReadFile(assets.Files, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	css, err := fs.ReadFile(assets.Files, "app.css")
	if err != nil {
		t.Fatal(err)
	}
	pageSource := string(page)
	cssSource := string(css)
	for _, required := range []string{
		`class="workspace-header workspace-topbar"`,
		`id="workspace-view-title"`,
		"#console main .workspace-topbar {",
		"position: sticky",
		"top: 0",
		"min-height: 72px",
		"background: var(--surface)",
		"#console main .workspace-topbar h1 {",
		"font-size: 22px",
	} {
		if !strings.Contains(pageSource+cssSource, required) {
			t.Errorf("compact enterprise top bar contract missing %q", required)
		}
	}
	if strings.Contains(cssSource, "font-size: clamp(30px, 3vw, 44px)") {
		t.Fatal("enterprise workspace title still uses the oversized hero treatment")
	}
	firstMedia := strings.Index(cssSource, "@media")
	for _, globalConstraint := range []string{
		"#console main .account { width: min(100%, 360px); min-width: 0;",
		"#console main .account-menu-shell { min-width: 0;",
		"#console main .account-menu-trigger { display: flex; width: 100%; min-width: 0;",
	} {
		constraintIndex := strings.Index(cssSource, globalConstraint)
		if constraintIndex < 0 || constraintIndex > firstMedia {
			t.Errorf("top bar account sizing must be global, missing %q before responsive media rules", globalConstraint)
		}
	}
}
