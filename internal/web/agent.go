package web

import (
	"context"
	"crypto/tls"
	"net/http"
	"strings"
	"time"

	"reladock/internal/nodeauth"
	"reladock/internal/platform"
	"reladock/internal/probe"
)

// Agent requests use a distinct mTLS-only listener, never a tenant cookie.
func NewAgent(store *platform.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.TLS == nil || r.TLS.Version < tls.VersionTLS13 || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.VerifiedChains[0]) == 0 {
			failure(w, platform.ErrUnauthorized)
			return
		}
		cert := r.TLS.VerifiedChains[0][0]
		id, err := nodeauth.NodeIdentity(cert)
		if err != nil {
			failure(w, platform.ErrUnauthorized)
			return
		}
		fingerprint := nodeauth.Fingerprint(cert)
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		r = r.WithContext(ctx)
		if err = store.AuthenticateNode(ctx, id, fingerprint); err != nil {
			failure(w, err)
			return
		}
		if r.Header.Get("Origin") != "" {
			failure(w, platform.ErrForbidden)
			return
		}
		// Collector traffic is per authenticated node, not the browser's shared
		// IP budget. Persist across control replicas/restarts; fail closed on DB
		// errors. This budget is a guardrail, not a tested capacity promise.
		allowed, limitErr := store.AllowRequest(ctx, "agent:"+id, "api", 6000)
		if limitErr != nil {
			failure(w, limitErr)
			return
		}
		if !allowed {
			w.Header().Set("Retry-After", "60")
			respond(w, 429, map[string]string{"error": "agent rate limit"})
			return
		}
		if r.Method != "POST" {
			respond(w, 405, map[string]string{"error": "POST required"})
			return
		}
		if r.URL.Path == "/agent/config/claim" {
			job, err := store.ClaimConfig(ctx, id, r.Header.Get("X-RelaDock-Agent-Version"), fingerprint)
			if err != nil {
				failure(w, err)
				return
			}
			if job == nil {
				w.WriteHeader(204)
				return
			}
			respond(w, 200, job)
			return
		}
		if r.URL.Path == "/agent/usage" {
			var report platform.UsageReport
			if !decode(w, r, &report) {
				return
			}
			receipt, err := store.ReportUsage(ctx, id, report, fingerprint)
			if err != nil {
				failure(w, err)
				return
			}
			respond(w, 200, receipt)
			return
		}
		if r.URL.Path == "/agent/probe" {
			var result probe.Result
			if !decode(w, r, &result) {
				return
			}
			if err = store.ReportProbe(ctx, id, result, fingerprint); err != nil {
				failure(w, err)
				return
			}
			respond(w, 200, map[string]bool{"ok": true})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/agent/config/") && (strings.HasSuffix(r.URL.Path, "/report") || strings.HasSuffix(r.URL.Path, "/begin")) {
			suffix := "/report"
			if strings.HasSuffix(r.URL.Path, "/begin") {
				suffix = "/begin"
			}
			jobID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/agent/config/"), suffix)
			if !uuid.MatchString(jobID) {
				failure(w, platform.ErrNotFound)
				return
			}
			var report platform.ConfigReport
			if !decode(w, r, &report) {
				return
			}
			if suffix == "/begin" {
				err = store.BeginApplication(ctx, id, jobID, report, fingerprint)
			} else {
				err = store.ReportConfig(ctx, id, jobID, report, fingerprint)
			}
			if err != nil {
				failure(w, err)
				return
			}
			respond(w, 200, map[string]bool{"ok": true})
			return
		}
		failure(w, platform.ErrNotFound)
	})
}
