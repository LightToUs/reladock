package runtimeconfig

import (
	"errors"
	"net"
	"net/url"
	"path/filepath"
)

// Managed enrollment derives private PKI locations without touching the filesystem.
// Existing manual installations must keep their current trust roots.
func managedEnrollmentEnv(get func(string) string) (func(string) string, error) {
	dir := get("NODE_MANAGED_PKI_DIR")
	if dir == "" {
		return get, nil
	}
	if !filepath.IsAbs(dir) || filepath.Clean(dir) == string(filepath.Separator) {
		return nil, errors.New("NODE_MANAGED_PKI_DIR requires a private absolute directory")
	}
	for _, key := range []string{"NODE_BOOTSTRAP_CA_CERT", "NODE_BOOTSTRAP_CA_KEY", "NODE_BOOTSTRAP_CA_CHAIN", "AGENT_TLS_DIR"} {
		if get(key) != "" {
			return nil, errors.New("managed PKI cannot replace or mix with manual Agent certificates")
		}
	}
	origin, err := url.Parse(get("PUBLIC_ORIGIN"))
	if err != nil || origin.Scheme != "https" || origin.Hostname() == "" {
		return nil, errors.New("managed enrollment requires the reachable HTTPS PUBLIC_ORIGIN")
	}
	defaults := map[string]string{
		"AGENT_TLS_DIR":           filepath.Join(dir, "server"),
		"NODE_BOOTSTRAP_CA_CERT":  filepath.Join(dir, "issuer", "ca.crt"),
		"NODE_BOOTSTRAP_CA_KEY":   filepath.Join(dir, "issuer", "ca.key"),
		"NODE_BOOTSTRAP_CA_CHAIN": filepath.Join(dir, "issuer", "chain.crt"),
		"AGENT_LISTEN_ADDRESS":    "0.0.0.0:9443",
		"AGENT_PUBLIC_ORIGIN":     "https://" + net.JoinHostPort(origin.Hostname(), "9443"),
	}
	return func(key string) string {
		if value := get(key); value != "" {
			return value
		}
		return defaults[key]
	}, nil
}
