package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"reladock/internal/agent"
	"reladock/internal/nodeauth"
	"reladock/internal/platform"
)

func TestAdminForcedCertificateRotationThroughRealMTLSHeartbeat(t *testing.T) {
	store := testWebStore(t)
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "pki")
	if err := nodeauth.CreateLocalBundle(dir, "hk-01"); err != nil {
		t.Fatal(err)
	}
	manager, err := agent.OpenCredentialManager(dir, "hk-01")
	if err != nil {
		t.Fatal(err)
	}
	old := manager.Current()
	admin, revision := prepareUpgradeNode(t, store, old.Fingerprint)
	signer, err := nodeauth.LoadBootstrapSigner(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key"), filepath.Join(dir, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	h := NewWithOptions(store, nil, "http://127.0.0.1:3001", Options{Bootstrap: &BootstrapOptions{Signer: signer}})
	login := request(h, "POST", "/api/auth/login", `{"email":"upgrade-operator@example.com","password":"long-test-password-42"}`, nil)
	if login.Code != 200 {
		t.Fatal(login.Body.String())
	}
	cookie := login.Result().Cookies()[0]
	path := "/api/admin/nodes/hk-01/rotate-certificate"
	body := fmt.Sprintf(`{"revision":%d}`, revision)
	if w := request(h, "POST", path, body, nil); w.Code != 401 {
		t.Fatalf("anonymous: %d %s", w.Code, w.Body.String())
	}
	if w := request(h, "POST", path, body, cookie); w.Code != 409 {
		t.Fatalf("old Agent accepted: %d %s", w.Code, w.Body.String())
	}
	for _, raw := range []string{`{}`, `{"revision":0}`, fmt.Sprintf(`{"revision":%d,"certificate":"forged"}`, revision)} {
		if w := request(h, "POST", path, raw, cookie); w.Code != 400 {
			t.Fatalf("strict body: %d %s", w.Code, w.Body.String())
		}
	}
	if w := request(h, "GET", path, "", cookie); w.Code != 405 {
		t.Fatalf("GET: %d", w.Code)
	}
	req := httptest.NewRequest("POST", "http://127.0.0.1:3001"+path, strings.NewReader(body))
	req.AddCookie(cookie)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("missing CSRF headers: %d", rec.Code)
	}
	noSigner := NewWithOptions(store, nil, "http://127.0.0.1:3001", Options{})
	if w := request(noSigner, "POST", path, body, cookie); w.Code != 503 || !strings.Contains(w.Body.String(), "节点证书自动续签未配置：需要签发 CA") {
		t.Fatalf("no signer: %d", w.Code)
	}
	serverTLS, err := nodeauth.ServerTLS(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(NewAgent(store, signer.Sign))
	srv.TLS = serverTLS
	srv.StartTLS()
	defer srv.Close()
	clientTLS, err := manager.TLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{TLSClientConfig: clientTLS}
	manager.RegisterTransport(transport)
	defer transport.CloseIdleConnections()
	runner := agent.Client{HTTP: &http.Client{Transport: transport}, BaseURL: srv.URL, NodeID: "hk-01", Credentials: manager}
	heartbeat := platform.AgentHeartbeat{Version: "dev", Mode: "validation", StartedAt: time.Now().Add(-time.Minute), CertificateNotAfter: old.NotAfter, RotationState: "idle", CertificateRotationSupported: true}
	if err = runner.ReportHeartbeat(ctx, heartbeat); err != nil {
		t.Fatal(err)
	}
	if w := request(h, "POST", path, body, cookie); w.Code != 202 || !strings.Contains(w.Body.String(), `"certificateRotationRequested":true`) {
		t.Fatalf("request: %d %s", w.Code, w.Body.String())
	}
	var listed []platform.ManagedNode
	w := request(h, "GET", "/api/admin/nodes", "", cookie)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &listed) != nil {
		t.Fatal("admin list failed")
	}
	var found bool
	for _, n := range listed {
		if n.ID == "hk-01" {
			found = true
			if !n.CertificateRotationSupported || !n.CertificateRotationRequested || n.CertificateFingerprint != old.Fingerprint {
				t.Fatalf("false completion: %+v", n)
			}
		}
	}
	if !found {
		t.Fatal("missing node")
	}
	// The old certificate still works until possession of the new key is proved.
	if err = store.AuthenticateNode(ctx, "hk-01", old.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if err = runner.ReportHeartbeat(ctx, heartbeat); err != nil {
		t.Fatal(err)
	}
	select {
	case <-manager.RotationRequests():
	default:
		t.Fatal("heartbeat did not deliver request")
	}
	if err = manager.ReconcileRotation(ctx, &runner, time.Now()); err != nil {
		t.Fatal(err)
	}
	next := manager.Current()
	if next.Fingerprint == old.Fingerprint {
		t.Fatal("identity did not rotate")
	}
	if err = store.AuthenticateNode(ctx, "hk-01", old.Fingerprint); err == nil {
		t.Fatal("old identity retained authorization")
	}
	heartbeat.CertificateNotAfter = next.NotAfter
	if err = runner.ReportHeartbeat(ctx, heartbeat); err != nil {
		t.Fatalf("new identity heartbeat: %v", err)
	}
	nodes, err := store.AdminNodes(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.ID == "hk-01" && (n.CertificateRotationRequested || n.CertificateFingerprint != next.Fingerprint) {
			t.Fatalf("activation not visible: %+v", n)
		}
	}
	// The trigger is gone, so a normal reconcile must not create another key.
	if err = manager.ReconcileRotation(ctx, &runner, time.Now()); err != nil {
		t.Fatal(err)
	}
	if manager.Current().Fingerprint != next.Fingerprint {
		t.Fatal("command replay rotated twice")
	}
}
