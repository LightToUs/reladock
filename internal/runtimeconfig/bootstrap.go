package runtimeconfig

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

type BootstrapConfig struct{ CACert, CAKey, CAChain, AgentOrigin, Version, AMD64SHA256, ARM64SHA256 string }

func (b BootstrapConfig) Enabled() bool { return b.CACert != "" }

var releaseVersion = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(?:-[a-zA-Z0-9.-]+)?$`)
var releaseHash = regexp.MustCompile(`^[a-f0-9]{64}$`)
var bootstrapOrigin = regexp.MustCompile(`^https://[A-Za-z0-9][A-Za-z0-9.-]*(?::[0-9]+)?$`)

func loadBootstrap(get func(string) string, c Config) (BootstrapConfig, error) {
	b := BootstrapConfig{get("NODE_BOOTSTRAP_CA_CERT"), get("NODE_BOOTSTRAP_CA_KEY"), get("NODE_BOOTSTRAP_CA_CHAIN"), get("AGENT_PUBLIC_ORIGIN"), get("NODE_RELEASE_VERSION"), get("NODE_RELEASE_AMD64_SHA256"), get("NODE_RELEASE_ARM64_SHA256")}
	values := []string{b.CACert, b.CAKey, b.CAChain, b.AgentOrigin, b.Version, b.AMD64SHA256, b.ARM64SHA256}
	n := 0
	for _, v := range values {
		if v != "" {
			n++
		}
	}
	if n == 0 {
		return b, nil
	}
	if n != len(values) {
		return b, errors.New("node bootstrap requires all signer, public origin and pinned release settings")
	}
	u, e := url.Parse(b.AgentOrigin)
	if e != nil || u.Scheme != "https" || u.Hostname() == "" || u.Path != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || strings.ContainsAny(b.AgentOrigin, "\r\n\t '") || u.Port() != "" && !validPort(u.Port()) {
		return b, errors.New("invalid AGENT_PUBLIC_ORIGIN")
	}
	if !bootstrapOrigin.MatchString(b.AgentOrigin) || !bootstrapOrigin.MatchString(c.Origin) || c.AgentTLSDir == "" {
		return b, errors.New("node bootstrap requires HTTPS control and enabled mTLS listener")
	}
	if !releaseVersion.MatchString(b.Version) || !releaseHash.MatchString(b.AMD64SHA256) || !releaseHash.MatchString(b.ARM64SHA256) {
		return b, errors.New("invalid pinned node release version or SHA256")
	}
	return b, nil
}
