package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"reladock/internal/agent"
	"reladock/internal/nodeauth"
	"reladock/internal/platform"
)

func TestMTLSAgentEnrollmentValidationAndRotation(t *testing.T) {
	store := testWebStore(t)
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "pki")
	if err := nodeauth.CreateLocalBundle(dir, "hk-01"); err != nil {
		t.Fatal(err)
	}
	cfg, err := nodeauth.ServerTLS(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(NewAgent(store))
	srv.TLS = cfg
	srv.StartTLS()
	defer srv.Close()
	clientTLS, err := nodeauth.ClientTLS(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "agent.crt"), filepath.Join(dir, "agent.key"))
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{TLSClientConfig: clientTLS}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	req, _ := http.NewRequest("POST", srv.URL+"/agent/config/claim", nil)
	req.Header.Set("X-RelaDock-Agent-Version", "validator-1")
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatalf("unregistered certificate accepted: %d", response.StatusCode)
	}
	noCertTLS := clientTLS.Clone()
	noCertTLS.Certificates = nil
	noCertTransport := &http.Transport{TLSClientConfig: noCertTLS}
	defer noCertTransport.CloseIdleConnections()
	if response, err = (&http.Client{Transport: noCertTransport}).Do(req); err == nil {
		response.Body.Close()
		t.Fatal("missing client certificate accepted")
	}
	cert, _ := nodeauth.ReadCertificate(filepath.Join(dir, "agent.crt"))
	if err = store.EnrollNode(ctx, "hk-01", nodeauth.Fingerprint(cert)); err != nil {
		t.Fatal(err)
	}
	who, _, err := store.Register(ctx, "Agent test", "agent@example.com", "long-test-password-42")
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.CreateRule(ctx, who, "agent-test-rule", platform.RuleInput{Name: "Relay", Protocol: "udp", TargetHost: "192.0.2.10", TargetPort: 443, BandwidthMbps: 1000})
	if err != nil {
		t.Fatal(err)
	}
	runner := agent.Client{HTTP: client, BaseURL: srv.URL, NodeID: "hk-01"}
	done, err := runner.Once(ctx)
	if err != nil || !done {
		t.Fatalf("agent validation: %v", err)
	}
	nodes, err := store.ListNodes(ctx)
	if err != nil || nodes[0].DesiredGeneration != nodes[0].ValidatedGeneration {
		t.Fatalf("validation missing: %+v %v", nodes, err)
	}
	rules, _ := store.ListRules(ctx, who)
	if rules[0].AppliedVersion != 0 {
		t.Fatal("validator reported actual application")
	}
	// A browser session is not an agent identity, and rotation revokes the old cert.
	if err = store.EnrollNode(ctx, "hk-01", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	if _, err = runner.Once(ctx); err == nil {
		t.Fatal("revoked certificate still authorized")
	}
}
