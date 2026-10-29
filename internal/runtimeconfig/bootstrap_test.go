package runtimeconfig

import (
	"strings"
	"testing"
)

func TestBootstrapConfigRejectsPartialAndUnsafeOrigins(t *testing.T) {
	base := map[string]string{"NODE_BOOTSTRAP_CA_CERT": "ca.crt"}
	if _, err := Load(func(k string) string { return base[k] }); err == nil {
		t.Fatal("partial bootstrap accepted")
	}
	for k, v := range map[string]string{"NODE_BOOTSTRAP_CA_KEY": "key", "NODE_BOOTSTRAP_CA_CHAIN": "chain", "AGENT_PUBLIC_ORIGIN": "http://example.com:9443", "NODE_RELEASE_VERSION": "v0.7.0", "NODE_RELEASE_AMD64_SHA256": strings.Repeat("a", 64), "NODE_RELEASE_ARM64_SHA256": strings.Repeat("b", 64)} {
		base[k] = v
	}
	if _, err := Load(func(k string) string { return base[k] }); err == nil {
		t.Fatal("unsafe bootstrap accepted")
	}
	base["AGENT_PUBLIC_ORIGIN"] = "https://example.com:9443"
	base["AGENT_TLS_DIR"] = "pki"
	base["PUBLIC_ORIGIN"] = "https://console.example.com"
	base["TLS_CERT_FILE"] = "cert"
	base["TLS_KEY_FILE"] = "key"
	if _, err := Load(func(k string) string { return base[k] }); err != nil {
		t.Fatal(err)
	}
	for _, origin := range []string{"https://agent$unexpected.example", "https://[::1]:9443", "https://agent.example.com\\bad"} {
		base["AGENT_PUBLIC_ORIGIN"] = origin
		if _, err := Load(func(k string) string { return base[k] }); err == nil {
			t.Fatalf("unsafe/unsupported bootstrap origin accepted: %s", origin)
		}
	}
	base["AGENT_PUBLIC_ORIGIN"] = "https://example.com:9443"
	base["NODE_RELEASE_VERSION"] = "v1; touch /tmp/oops"
	if _, err := Load(func(k string) string { return base[k] }); err == nil {
		t.Fatal("unsafe release accepted")
	}
}
