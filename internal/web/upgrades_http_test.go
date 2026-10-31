package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"reladock/internal/nodeauth"
	"reladock/internal/platform"
	"reladock/internal/runtimeconfig"
)

func upgradeBootstrapConfig() runtimeconfig.BootstrapConfig {
	return runtimeconfig.BootstrapConfig{AgentOrigin: "https://console.example.com", Version: "v0.9.0", AMD64SHA256: strings.Repeat("a", 64), ARM64SHA256: strings.Repeat("b", 64)}
}

func prepareUpgradeNode(t *testing.T, store *platform.Store, fingerprint string) (platform.Identity, int64) {
	t.Helper()
	ctx := context.Background()
	if err := store.EnrollNode(ctx, "hk-01", fingerprint); err != nil {
		t.Fatal(err)
	}
	if err := store.ReportAgentHeartbeat(ctx, "hk-01", fingerprint, platform.AgentHeartbeat{Version: "v0.8.0", Mode: "validation", StartedAt: time.Now().Add(-time.Minute), CertificateNotAfter: time.Now().Add(24 * time.Hour), RotationState: "idle"}); err != nil {
		t.Fatal(err)
	}
	admin, _, err := store.Register(ctx, "Upgrade operator", "upgrade-operator@example.com", "long-test-password-42")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SetPlatformAdmin(ctx, admin.Email, true); err != nil {
		t.Fatal(err)
	}
	admin, _, err = store.Login(ctx, admin.Email, "long-test-password-42")
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := store.AdminNodes(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	return admin, nodes[0].Revision
}

func adminUpgradeHandler(t *testing.T, store *platform.Store, config *runtimeconfig.BootstrapConfig) (http.Handler, *http.Cookie) {
	t.Helper()
	options := Options{}
	if config != nil {
		options.Bootstrap = &BootstrapOptions{Config: *config}
	}
	h := NewWithOptions(store, fs.FS(nil), "http://127.0.0.1:3001", options)
	login := request(h, "POST", "/api/auth/login", `{"email":"upgrade-operator@example.com","password":"long-test-password-42"}`, nil)
	if login.Code != http.StatusOK {
		t.Fatal(login.Body.String())
	}
	return h, login.Result().Cookies()[0]
}

func TestAdminUpgradeAPIUsesOnlyPinnedServerTarget(t *testing.T) {
	store := testWebStore(t)
	_, revision := prepareUpgradeNode(t, store, strings.Repeat("c", 64))
	config := upgradeBootstrapConfig()
	h, cookie := adminUpgradeHandler(t, store, &config)

	if rec := request(h, "POST", "/api/admin/nodes/hk-01/upgrade", `{"revision":2,"targetVersion":"v9.9.9"}`, cookie); rec.Code != http.StatusBadRequest {
		t.Fatalf("client-selected target accepted: %d %s", rec.Code, rec.Body.String())
	}
	body := `{"revision":` + strconv.FormatInt(revision, 10) + `}`
	first := request(h, "POST", "/api/admin/nodes/hk-01/upgrade", body, cookie)
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), `"targetVersion":"v0.9.0"`) || strings.Contains(first.Body.String(), "sha256") {
		t.Fatalf("upgrade response: %d %s", first.Code, first.Body.String())
	}
	var created platform.AgentUpgrade
	if err := json.Unmarshal(first.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	replay := request(h, "POST", "/api/admin/nodes/hk-01/upgrade", body, cookie)
	var repeated platform.AgentUpgrade
	_ = json.Unmarshal(replay.Body.Bytes(), &repeated)
	if replay.Code != http.StatusOK || repeated.ID != created.ID {
		t.Fatalf("idempotent request failed: %d %s", replay.Code, replay.Body.String())
	}
}

func TestAdminUpgradeAPIRequiresPlatformAdminAndReleaseConfiguration(t *testing.T) {
	store := testWebStore(t)
	_, revision := prepareUpgradeNode(t, store, strings.Repeat("d", 64))
	config := upgradeBootstrapConfig()
	h, _ := adminUpgradeHandler(t, store, &config)
	owner, _, err := store.Register(context.Background(), "Ordinary", "ordinary-upgrade@example.com", "long-test-password-42")
	if err != nil {
		t.Fatal(err)
	}
	_ = owner
	login := request(h, "POST", "/api/auth/login", `{"email":"ordinary-upgrade@example.com","password":"long-test-password-42"}`, nil)
	if rec := request(h, "POST", "/api/admin/nodes/hk-01/upgrade", `{"revision":2}`, login.Result().Cookies()[0]); rec.Code != http.StatusForbidden {
		t.Fatalf("ordinary owner upgraded node: %d", rec.Code)
	}
	unconfigured, cookie := adminUpgradeHandler(t, store, nil)
	body := `{"revision":` + strconv.FormatInt(revision, 10) + `}`
	if rec := request(unconfigured, "POST", "/api/admin/nodes/hk-01/upgrade", body, cookie); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured release accepted: %d %s", rec.Code, rec.Body.String())
	}
}

func TestMTLSUpgradeClaimAndReportAreStrictAndNodeBound(t *testing.T) {
	store := testWebStore(t)
	dir := filepath.Join(t.TempDir(), "pki")
	if err := nodeauth.CreateLocalBundle(dir, "hk-01"); err != nil {
		t.Fatal(err)
	}
	serverTLS, err := nodeauth.ServerTLS(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key"))
	if err != nil {
		t.Fatal(err)
	}
	cert, err := nodeauth.ReadCertificate(filepath.Join(dir, "agent.crt"))
	if err != nil {
		t.Fatal(err)
	}
	admin, revision := prepareUpgradeNode(t, store, nodeauth.Fingerprint(cert))
	task, err := store.RequestAgentUpgrade(context.Background(), admin, "hk-01", revision, platform.ReleaseTarget{Version: "v0.9.0", AMD64SHA256: strings.Repeat("a", 64), ARM64SHA256: strings.Repeat("b", 64)})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(NewAgent(store, nil))
	srv.TLS = serverTLS
	srv.StartTLS()
	defer srv.Close()
	clientTLS, err := nodeauth.ClientTLS(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "agent.crt"), filepath.Join(dir, "agent.key"))
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{TLSClientConfig: clientTLS.Clone()}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	post := func(path, body, origin string) (int, []byte) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		response, requestErr := client.Do(req)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		defer response.Body.Close()
		raw, _ := io.ReadAll(response.Body)
		return response.StatusCode, raw
	}

	if status, _ := post("/agent/upgrade/claim", `{"arch":"amd64","currentVersion":"v0.8.0","extra":true}`, ""); status != http.StatusBadRequest {
		t.Fatalf("unknown claim field accepted: %d", status)
	}
	if status, _ := post("/agent/upgrade/claim", `{"arch":"amd64","currentVersion":"v0.8.0"}`, "https://browser.example"); status != http.StatusForbidden {
		t.Fatalf("browser origin accepted: %d", status)
	}
	status, raw := post("/agent/upgrade/claim", `{"arch":"amd64","currentVersion":"v0.8.0"}`, "")
	var claim platform.UpgradeClaim
	if err = json.Unmarshal(raw, &claim); err != nil || status != http.StatusOK || claim.ID != task.ID || claim.LeaseToken == "" || strings.Contains(string(raw), "github") || strings.Contains(string(raw), "url") {
		t.Fatalf("claim response: %d %s %v", status, raw, err)
	}
	report, _ := json.Marshal(platform.UpgradeReport{ID: task.ID, LeaseToken: claim.LeaseToken, State: "installing"})
	if status, raw = post("/agent/upgrade/report", string(report), ""); status != http.StatusOK {
		t.Fatalf("report rejected: %d %s", status, raw)
	}
	if status, _ = post("/agent/upgrade/report", string(report), ""); status != http.StatusOK {
		t.Fatalf("exact replay rejected: %d", status)
	}
	conflict, _ := json.Marshal(platform.UpgradeReport{ID: task.ID, LeaseToken: claim.LeaseToken, State: "installing", ErrorCode: "state-invalid"})
	if status, _ = post("/agent/upgrade/report", string(conflict), ""); status != http.StatusConflict {
		t.Fatalf("conflicting replay accepted: %d", status)
	}

	noCert := clientTLS.Clone()
	noCert.Certificates = nil
	withoutIdentity := &http.Client{Transport: &http.Transport{TLSClientConfig: noCert}, Timeout: 2 * time.Second}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/agent/upgrade/claim", bytes.NewBufferString(`{"arch":"amd64","currentVersion":"v0.8.0"}`))
	req.Header.Set("Content-Type", "application/json")
	if response, requestErr := withoutIdentity.Do(req); requestErr == nil {
		response.Body.Close()
		t.Fatal("claim without node certificate connected")
	} else if !strings.Contains(requestErr.Error(), "certificate") && !strings.Contains(requestErr.Error(), "handshake") {
		t.Fatalf("unexpected missing identity error: %v", requestErr)
	}
}
