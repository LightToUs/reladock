package runtimeconfig

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestManagedEnrollmentConfiguration(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pki")
	values := map[string]string{"NODE_MANAGED_PKI_DIR": dir, "PUBLIC_ORIGIN": "https://console.reladock.com", "TLS_CERT_FILE": "/tls.crt", "TLS_KEY_FILE": "/tls.key", "NODE_RELEASE_VERSION": "v0.9.9", "NODE_RELEASE_AMD64_SHA256": strings.Repeat("a", 64), "NODE_RELEASE_ARM64_SHA256": strings.Repeat("b", 64)}
	get := func(k string) string { return values[k] }
	c, err := Load(get)
	if err != nil {
		t.Fatal(err)
	}
	if c.ManagedPKIDir != dir || c.AgentTLSDir != filepath.Join(dir, "server") || c.Bootstrap.CAKey != filepath.Join(dir, "issuer", "ca.key") || c.Bootstrap.AgentOrigin != "https://console.reladock.com:9443" || !c.Bootstrap.Enabled() {
		t.Fatalf("unexpected managed config: %+v", c)
	}
	values["NODE_BOOTSTRAP_CA_KEY"] = "/manual.key"
	if _, err = Load(get); err == nil {
		t.Fatal("mixed manual and automatic PKI accepted")
	}
	delete(values, "NODE_BOOTSTRAP_CA_KEY")
	values["NODE_MANAGED_PKI_DIR"] = "relative"
	if _, err = Load(get); err == nil {
		t.Fatal("relative managed directory accepted")
	}
	values["NODE_MANAGED_PKI_DIR"] = dir
	values["PUBLIC_ORIGIN"] = "http://127.0.0.1:3012"
	delete(values, "TLS_CERT_FILE")
	delete(values, "TLS_KEY_FILE")
	if _, err = Load(get); err == nil {
		t.Fatal("HTTP enrollment accepted")
	}
}
