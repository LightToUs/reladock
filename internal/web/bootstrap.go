package web

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	deployment "reladock/deploy"
	"reladock/internal/nodeauth"
	"reladock/internal/platform"
	"reladock/internal/runtimeconfig"
	"strings"
	"time"
)

type BootstrapOptions struct {
	Config runtimeconfig.BootstrapConfig
	Signer *nodeauth.BootstrapCA
}

func (s *server) bootstrapEnabled() bool {
	return s.bootstrap != nil && s.bootstrap.Signer != nil && s.bootstrap.Config.Enabled()
}
func (s *server) bootstrapUnavailable(w http.ResponseWriter) {
	respond(w, 503, map[string]string{"error": "节点自动安装未配置：需要签发 CA、公开 HTTPS 地址和已发布版本的固定摘要"})
}
func (s *server) serveBootstrap(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != "/install/agent.sh" && !strings.HasPrefix(r.URL.Path, "/api/node-bootstrap/") {
		return false
	}
	if !s.bootstrapEnabled() {
		s.bootstrapUnavailable(w)
		return true
	}
	if !s.allowed(r) {
		respond(w, 429, map[string]string{"error": "接入请求过于频繁"})
		return true
	}
	c := s.bootstrap.Config
	if r.Method == "GET" && r.URL.Path == "/install/agent.sh" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write(deployment.BootstrapScript)
		return true
	}
	if r.Method == "GET" && r.URL.Path == "/api/node-bootstrap/release" {
		// Fixed ASCII fields for a dependency-free POSIX shell installer.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "%s %s %s\n", c.Version, c.AMD64SHA256, c.ARM64SHA256)
		return true
	}
	if r.Method == "POST" && r.URL.Path == "/api/node-bootstrap/redeem" {
		// Browser cookies never authorize this exception to browser CSRF protection.
		if r.Header.Get("Origin") != "" || r.Header.Get("Cookie") != "" {
			failure(w, platform.ErrForbidden)
			return true
		}
		var in struct {
			CSR string `json:"csr"`
		}
		if !decode(w, r, &in) {
			return true
		}
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			failure(w, platform.ErrUnauthorized)
			return true
		}
		cert, err := s.store.RedeemNodeBootstrap(r.Context(), strings.TrimPrefix(auth, "Bearer "), []byte(in.CSR), s.bootstrap.Signer.Sign)
		if err != nil {
			failure(w, err)
			return true
		}
		block, _ := pem.Decode(cert.Certificate)
		leaf, _ := x509.ParseCertificate(block.Bytes)
		respond(w, 200, map[string]any{"nodeId": cert.NodeID, "certificate": string(cert.Certificate), "ca": string(s.bootstrap.Signer.Chain), "control": c.AgentOrigin, "expiresAt": leaf.NotAfter})
		return true
	}
	failure(w, platform.ErrNotFound)
	return true
}

func (s *server) adminBootstrap(w http.ResponseWriter, r *http.Request, who platform.Identity, id, action string) bool {
	if action != "bootstrap" && action != "bootstrap-revoke" && action != "preflight" {
		return false
	}
	if action == "preflight" && r.Method == "GET" {
		v, err := s.store.AdminPreflight(r.Context(), who, id)
		if err != nil {
			failure(w, err)
		} else {
			respond(w, 200, v)
		}
		return true
	}
	if r.Method != "POST" {
		failure(w, platform.ErrNotFound)
		return true
	}
	if !s.bootstrapEnabled() {
		s.bootstrapUnavailable(w)
		return true
	}
	var in struct {
		Revision int64 `json:"revision"`
	}
	if !decode(w, r, &in) {
		return true
	}
	if action == "bootstrap-revoke" {
		if err := s.store.RevokeNodeBootstrap(r.Context(), who, id, in.Revision); err != nil {
			failure(w, err)
		} else {
			respond(w, 200, map[string]bool{"ok": true})
		}
		return true
	}
	grant, err := s.store.CreateNodeBootstrap(r.Context(), who, id, in.Revision)
	if err != nil {
		failure(w, err)
		return true
	}
	// The URL is encoded with shell-safe quoting; token is hex, never a URL parameter.
	quote := func(v string) string { return "'" + strings.ReplaceAll(v, "'", "'\"'\"'") + "'" }
	command := fmt.Sprintf("(umask 077; f=$(mktemp /tmp/reladock-install.XXXXXX) || exit 1; trap 'rm -f \"$f\"' EXIT HUP INT TERM; curl --fail --silent --show-error --proto '=https' --max-time 60 %s -o \"$f\" && printf '%%s\\n' %s | sudo sh \"$f\" %s)", quote(s.origin+"/install/agent.sh"), quote(grant.Token), quote(s.origin))
	respond(w, 201, map[string]any{"command": command, "expiresAt": grant.ExpiresAt, "revision": grant.Revision, "nodeId": id, "mode": "validation-only", "certificateLifetimeDays": int((30 * 24 * time.Hour).Hours() / 24)})
	return true
}
