package web

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reladock/internal/agent"
	"reladock/internal/nodeauth"
	"reladock/internal/runtimeconfig"
	"strings"
	"testing"
	"testing/fstest"
)

func TestBootstrapHTTPSRegistrationAndMTLSValidation(t *testing.T) {
	store := testWebStore(t)
	ctx := context.Background()
	who, _, err := store.Register(ctx, "Bootstrap", "bootstrap-integration@example.com", "long-bootstrap-test-password")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SetPlatformAdmin(ctx, who.Email, true); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "ca")
	if err = nodeauth.CreateLocalBundle(dir, "hk-01"); err != nil {
		t.Fatal(err)
	}
	signer, err := nodeauth.LoadBootstrapSigner(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key"), filepath.Join(dir, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	mtls, e := nodeauth.ServerTLS(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key"))
	if e != nil {
		t.Fatal(e)
	}
	nodeServer := httptest.NewUnstartedServer(NewAgent(store))
	nodeServer.TLS = mtls
	nodeServer.StartTLS()
	defer nodeServer.Close()
	pair, _ := tls.LoadX509KeyPair(filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key"))
	control := httptest.NewUnstartedServer(nil)
	origin := "https://" + control.Listener.Addr().String()
	options := BootstrapOptions{Signer: signer, Config: runtimeconfig.BootstrapConfig{CACert: "test", AgentOrigin: nodeServer.URL, Version: "v0.7.0", AMD64SHA256: strings.Repeat("a", 64), ARM64SHA256: strings.Repeat("b", 64)}}
	control.Config.Handler = NewWithOptions(store, fstest.MapFS{}, origin, Options{Bootstrap: &options})
	control.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}}
	control.StartTLS()
	defer control.Close()
	g, err := store.CreateNodeBootstrap(ctx, who, "hk-01", 1)
	if err != nil {
		t.Fatal(err)
	}
	nodeDir := filepath.Join(t.TempDir(), "node-pki")
	env := filepath.Join(filepath.Dir(nodeDir), "agent.env")
	if err = agent.Bootstrap(ctx, agent.BootstrapOptions{Origin: control.URL, PKIDir: nodeDir, EnvFile: env, Token: strings.NewReader(g.Token + "\n"), HTTP: control.Client()}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(env); !strings.Contains(string(b), nodeServer.URL) {
		t.Fatal("wrong control endpoint saved")
	}
	config, err := nodeauth.ClientTLS(filepath.Join(nodeDir, "ca.crt"), filepath.Join(nodeDir, "agent.crt"), filepath.Join(nodeDir, "agent.key"))
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{TLSClientConfig: config}
	defer transport.CloseIdleConnections()
	client := agent.Client{HTTP: &http.Client{Transport: transport}, BaseURL: nodeServer.URL, NodeID: "hk-01"}
	done, err := client.Once(ctx)
	if err != nil || !done {
		t.Fatalf("mTLS validation: %v %v", done, err)
	}
	if err = client.ReportPreflight(ctx); err != nil {
		t.Fatal(err)
	}
	report, err := store.AdminPreflight(ctx, who, "hk-01")
	if err != nil || report.ReportedAt == nil {
		t.Fatal("no contact report")
	}
}

type bootstrapRoundTrip func(*http.Request) (*http.Response, error)

func (f bootstrapRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestBootstrapResponseLossAndPartialWriteRecovery(t *testing.T) {
	store := testWebStore(t)
	ctx := context.Background()
	who, _, err := store.Register(ctx, "Recovery", "bootstrap-recovery@example.com", "long-recovery-test-password")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SetPlatformAdmin(ctx, who.Email, true); err != nil {
		t.Fatal(err)
	}
	caDir := filepath.Join(t.TempDir(), "ca")
	if err = nodeauth.CreateLocalBundle(caDir, "hk-01"); err != nil {
		t.Fatal(err)
	}
	signer, err := nodeauth.LoadBootstrapSigner(filepath.Join(caDir, "ca.crt"), filepath.Join(caDir, "ca.key"), filepath.Join(caDir, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(nil)
	origin := "https://" + srv.Listener.Addr().String()
	srv.Config.Handler = NewWithOptions(store, fstest.MapFS{}, origin, Options{Bootstrap: &BootstrapOptions{Signer: signer, Config: runtimeconfig.BootstrapConfig{CACert: "test", AgentOrigin: "https://agent.example.com:9443"}}})
	srv.StartTLS()
	defer srv.Close()
	grant, err := store.CreateNodeBootstrap(ctx, who, "hk-01", 1)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "pki")
	env := filepath.Join(filepath.Dir(dir), "agent.env")
	base := srv.Client().Transport
	attempt := 0
	client := &http.Client{Transport: bootstrapRoundTrip(func(r *http.Request) (*http.Response, error) {
		res, e := base.RoundTrip(r)
		if e != nil {
			return nil, e
		}
		attempt++
		if attempt == 1 {
			res.Body.Close()
			return nil, io.ErrUnexpectedEOF
		}
		if attempt == 2 {
			if err := os.Mkdir(filepath.Join(dir, "agent.crt"), 0700); err != nil {
				t.Fatal(err)
			}
		}
		return res, nil
	})}
	options := agent.BootstrapOptions{Origin: origin, PKIDir: dir, EnvFile: env, HTTP: client}
	options.Token = strings.NewReader(grant.Token)
	if err = agent.Bootstrap(ctx, options); err == nil {
		t.Fatal("expected response loss")
	}
	key, _ := os.ReadFile(filepath.Join(dir, "agent.key"))
	options.Token = strings.NewReader(grant.Token)
	if err = agent.Bootstrap(ctx, options); err == nil {
		t.Fatal("expected partial file failure")
	}
	if err = os.Remove(filepath.Join(dir, "agent.crt")); err != nil {
		t.Fatal(err)
	} // Remove this test's injected empty directory, not a credential.
	options.Token = strings.NewReader(grant.Token)
	if err = agent.Bootstrap(ctx, options); err != nil {
		t.Fatalf("cannot recover identical partial enrollment: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "agent.key"))
	if string(key) != string(after) {
		t.Fatal("private key changed during recovery")
	}
}
