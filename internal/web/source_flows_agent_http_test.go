package web

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"reladock/internal/nodeauth"
	"reladock/internal/platform"
	"reladock/internal/sourceflow"
)

func TestSourceFlowAgentIngestRequiresMTLSAndReturnsExactReceipt(t *testing.T) {
	store, _ := sourceLoggingHTTPServer(t)
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "pki")
	if err := nodeauth.CreateLocalBundle(dir, "hk-01"); err != nil {
		t.Fatal(err)
	}
	cert, err := nodeauth.ReadCertificate(filepath.Join(dir, "agent.crt"))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.EnrollNode(ctx, "hk-01", nodeauth.Fingerprint(cert)); err != nil {
		t.Fatal(err)
	}
	who, _, err := store.Register(ctx, "Source Agent", "source-agent@example.com", "long-test-password-42")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SetPlatformAdmin(ctx, who.Email, true); err != nil {
		t.Fatal(err)
	}
	admin, _, err := store.Login(ctx, who.Email, "long-test-password-42")
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := store.AdminNodes(ctx, admin)
	if err != nil || len(nodes) == 0 {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}
	node := nodes[0]
	if err = store.AdminUpdateNode(ctx, admin, platform.NodeInput{ID: node.ID, Name: node.Name, Region: node.Region, ListenAddress: "203.0.113.10", PublicAddress: "203.0.113.10", CapacityMbps: node.CapacityMbps, ResourceMode: node.ResourceMode, DedicatedTenantID: node.DedicatedTenantID}, node.Revision); err != nil {
		t.Fatal(err)
	}
	rule, err := store.CreateRule(ctx, who, "source-agent-rule", platform.RuleInput{Name: "Relay", Protocol: "tcp", TargetHost: "198.51.100.20", TargetPort: 443, BandwidthMbps: 100, NodeID: "hk-01"})
	if err != nil {
		t.Fatal(err)
	}
	heartbeat := platform.AgentHeartbeat{SourceFlowSupported: true, Version: "source-flow-agent", Mode: "apply", StartedAt: time.Now().Add(-time.Minute), CertificateNotAfter: cert.NotAfter, RotationState: "idle"}
	if err = store.ReportAgentHeartbeat(ctx, "hk-01", nodeauth.Fingerprint(cert), heartbeat); err != nil {
		t.Fatal(err)
	}
	if _, err = store.AdminSetRuleSourceLogging(ctx, admin, who.TenantID, rule.ID, rule.DesiredVersion, true); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimConfig(ctx, "hk-01", "source-flow-agent")
	if err != nil || job == nil {
		t.Fatalf("claim=%+v err=%v", job, err)
	}
	if err = store.BeginApplication(ctx, "hk-01", job.ID, platform.ConfigReport{LeaseToken: job.LeaseToken, Checksum: job.Checksum, Outcome: "applying"}); err != nil {
		t.Fatal(err)
	}
	var cfg platform.NodeConfig
	if err = json.Unmarshal(job.Payload, &cfg); err != nil {
		t.Fatal(err)
	}
	configRule := cfg.Rules[0]
	allocation := cfg.Allocations[0]
	manifest, digest, err := sourceflow.NewManifest("hk-01", cfg.Generation, []sourceflow.AppliedRule{{AllocationID: allocation.ID, TenantID: configRule.TenantID, RuleID: configRule.ID, RuleVersion: configRule.Version, SourceLoggingEnabled: true, Protocol: allocation.Protocol, RelayIP: allocation.Address, RelayPort: allocation.Port, TargetIP: configRule.TargetHost, TargetPort: configRule.TargetPort}})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.ReportConfig(ctx, "hk-01", job.ID, platform.ConfigReport{LeaseToken: job.LeaseToken, Checksum: job.Checksum, Outcome: "applied", SourceFlowManifest: &manifest, SourceFlowManifestDigest: digest}); err != nil {
		t.Fatal(err)
	}
	event := sourceflow.Event{EventID: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", TenantID: configRule.TenantID, RuleID: configRule.ID, NodeID: "hk-01", AllocationID: allocation.ID, ConfigGeneration: cfg.Generation, RuleVersion: configRule.Version, Protocol: allocation.Protocol, SourceIP: "192.0.2.40", SourcePort: 53000, RelayIP: allocation.Address, RelayPort: allocation.Port, TargetIP: configRule.TargetHost, TargetPort: configRule.TargetPort, ObservedAt: time.Now().UTC().Add(-time.Second).Truncate(time.Microsecond), KernelBootID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd", ConntrackID: 42}
	batch := sourceflow.Batch{NodeID: "hk-01", Events: []sourceflow.Event{event}}
	body, _ := json.Marshal(batch)

	serverTLS, _ := nodeauth.ServerTLS(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key"))
	srv := httptest.NewUnstartedServer(NewAgent(store, nil))
	srv.TLS = serverTLS
	srv.StartTLS()
	defer srv.Close()
	clientTLS, _ := nodeauth.ClientTLS(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "agent.crt"), filepath.Join(dir, "agent.key"))
	transport := &http.Transport{TLSClientConfig: clientTLS}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	request, _ := http.NewRequest(http.MethodPost, srv.URL+"/agent/source-flows", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", response.StatusCode)
	}
	var receipt sourceflow.Receipt
	if err = json.NewDecoder(response.Body).Decode(&receipt); err != nil || len(receipt.EventIDs) != 1 || receipt.EventIDs[0] != event.EventID {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}

	noCert := clientTLS.Clone()
	noCert.Certificates = nil
	noCert.MinVersion = tls.VersionTLS13
	noCertTransport := &http.Transport{TLSClientConfig: noCert}
	defer noCertTransport.CloseIdleConnections()
	unauthorized, _ := http.NewRequest(http.MethodPost, srv.URL+"/agent/source-flows", bytes.NewReader(body))
	unauthorized.Header.Set("Content-Type", "application/json")
	if response, requestErr := (&http.Client{Transport: noCertTransport}).Do(unauthorized); requestErr == nil {
		response.Body.Close()
		t.Fatal("source-flow upload without client certificate accepted")
	}
}
