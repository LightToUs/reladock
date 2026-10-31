package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"reladock/internal/buildinfo"
	"reladock/internal/platform"
)

func TestReportHeartbeatPostsBoundedJSONWithoutFollowingRedirects(t *testing.T) {
	originalVersion := buildinfo.ReleaseVersion
	buildinfo.ReleaseVersion = "v0.9.0"
	t.Cleanup(func() { buildinfo.ReleaseVersion = originalVersion })
	redirected := false
	var received platform.AgentHeartbeat
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirected" {
			redirected = true
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/agent/heartbeat" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("X-RelaDock-Agent-Version") != "v0.9.0" {
			t.Fatalf("heartbeat request: %s %s headers=%v", r.Method, r.URL.Path, r.Header)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatal(err)
		}
		http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	now := time.Now().UTC().Truncate(time.Second)
	heartbeat := platform.AgentHeartbeat{Version: "v0.9.0", Mode: "validation", StartedAt: now.Add(-time.Minute), CertificateNotAfter: now.Add(24 * time.Hour), RotationState: "idle"}
	c := Client{HTTP: srv.Client(), BaseURL: srv.URL, NodeID: "hk-01"}
	if err := c.ReportHeartbeat(context.Background(), heartbeat); err == nil {
		t.Fatal("redirect response was accepted")
	}
	if redirected {
		t.Fatal("heartbeat followed redirect")
	}
	if received != heartbeat {
		t.Fatalf("heartbeat changed in transit: %+v", received)
	}
}

func TestReportHeartbeatRejectsMissingClient(t *testing.T) {
	c := Client{BaseURL: "https://control.example.com", NodeID: "hk-01"}
	if err := c.ReportHeartbeat(context.Background(), platform.AgentHeartbeat{}); err == nil {
		t.Fatal("heartbeat without mTLS client accepted")
	}
}
