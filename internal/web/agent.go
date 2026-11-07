package web

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"reladock/internal/nodeauth"
	"reladock/internal/platform"
	"reladock/internal/probe"
)

// Agent requests use a distinct mTLS-only listener, never a tenant cookie.
func NewAgent(store *platform.Store, signer platform.BootstrapSigner) http.Handler {
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
		if r.URL.Path == "/agent/certificate/activate" {
			err = store.AuthenticateNodeForActivation(ctx, id, fingerprint)
		} else {
			err = store.AuthenticateNode(ctx, id, fingerprint)
		}
		if err != nil {
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
		if r.URL.Path == "/agent/certificate/renew" {
			var input struct {
				CSR string `json:"csr"`
			}
			if !decode(w, r, &input) {
				return
			}
			if signer == nil {
				respond(w, http.StatusServiceUnavailable, map[string]string{"error": "certificate renewal unavailable"})
				return
			}
			rotation, renewErr := store.RenewNodeCertificate(ctx, id, fingerprint, []byte(input.CSR), signer)
			if renewErr != nil {
				failure(w, renewErr)
				return
			}
			respond(w, http.StatusOK, map[string]any{"certificate": string(rotation.Certificate), "expiresAt": rotation.ExpiresAt})
			return
		}
		if r.URL.Path == "/agent/certificate/activate" {
			if err = store.ActivateNodeCertificate(ctx, id, fingerprint); err != nil {
				failure(w, err)
				return
			}
			respond(w, http.StatusOK, map[string]bool{"ok": true})
			return
		}
		if r.URL.Path == "/agent/heartbeat" {
			var heartbeat platform.AgentHeartbeat
			if !decode(w, r, &heartbeat) {
				return
			}
			heartbeat.CertificateRotationSupported = r.Header.Get("X-RelaDock-Certificate-Rotation") == "1"
			// Certificate expiry is observed from the verified TLS peer rather than
			// accepted as an Agent claim.
			if !heartbeat.CertificateNotAfter.Equal(cert.NotAfter) {
				failure(w, platform.ErrInvalid)
				return
			}
			if err = store.ReportAgentHeartbeat(ctx, id, fingerprint, heartbeat); err != nil {
				failure(w, err)
				return
			}
			desired, err := store.NodeCertificateRenewalRequest(ctx, id, fingerprint)
			if err != nil {
				failure(w, err)
				return
			}
			respond(w, 200, struct {
				OK                            bool   `json:"ok"`
				CertificateRenewalFingerprint string `json:"certificateRenewalFingerprint,omitempty"`
			}{true, desired})
			return
		}
		if r.URL.Path == "/agent/drain-status" {
			body, bodyErr := io.ReadAll(io.LimitReader(r.Body, 2))
			if bodyErr != nil || len(body) != 0 {
				failure(w, platform.ErrInvalid)
				return
			}
			draining, statusErr := store.NodeDrainRequested(ctx, id)
			if statusErr != nil {
				failure(w, statusErr)
				return
			}
			respond(w, http.StatusOK, map[string]bool{"draining": draining})
			return
		}
		if r.URL.Path == "/agent/upgrade/claim" {
			var input struct {
				Arch           string `json:"arch"`
				CurrentVersion string `json:"currentVersion"`
			}
			if !decode(w, r, &input) {
				return
			}
			claim, claimErr := store.ClaimAgentUpgrade(ctx, id, fingerprint, input.Arch, input.CurrentVersion)
			if claimErr != nil {
				failure(w, claimErr)
				return
			}
			if claim == nil {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			respond(w, http.StatusOK, claim)
			return
		}
		if r.URL.Path == "/agent/upgrade/report" {
			var report platform.UpgradeReport
			if !decode(w, r, &report) {
				return
			}
			value, reportErr := store.ReportAgentUpgrade(ctx, id, fingerprint, report)
			if reportErr != nil {
				failure(w, reportErr)
				return
			}
			respond(w, http.StatusOK, value)
			return
		}
		if r.URL.Path == "/agent/retirements/claim" {
			body, bodyErr := io.ReadAll(io.LimitReader(r.Body, 2))
			if bodyErr != nil || len(body) != 0 {
				failure(w, platform.ErrInvalid)
				return
			}
			claim, claimErr := store.ClaimRetirement(ctx, id, fingerprint)
			if claimErr != nil {
				failure(w, claimErr)
				return
			}
			if claim == nil {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			respond(w, http.StatusOK, claim)
			return
		}
		if r.URL.Path == "/agent/retirements/begin" {
			var input struct {
				TaskID     string `json:"taskId"`
				LeaseToken string `json:"leaseToken"`
			}
			if !decode(w, r, &input) {
				return
			}
			if err = store.BeginRetirementPurge(ctx, id, fingerprint, input.TaskID, input.LeaseToken); err != nil {
				failure(w, err)
				return
			}
			respond(w, http.StatusOK, map[string]bool{"ok": true})
			return
		}
		if r.URL.Path == "/agent/retirements/report" {
			var input struct {
				TaskID     string                     `json:"taskId"`
				LeaseToken string                     `json:"leaseToken,omitempty"`
				ErrorCode  string                     `json:"errorCode,omitempty"`
				Report     *platform.RetirementReport `json:"report,omitempty"`
			}
			if !decode(w, r, &input) {
				return
			}
			if input.Report != nil && (input.LeaseToken != "" || input.ErrorCode != "") || input.Report == nil && (input.LeaseToken == "" || input.ErrorCode == "") {
				failure(w, platform.ErrInvalid)
				return
			}
			if input.Report != nil {
				err = store.ReportRetirement(ctx, id, fingerprint, input.TaskID, *input.Report)
			} else {
				err = store.ReportRetirementFailure(ctx, id, fingerprint, input.TaskID, input.LeaseToken, input.ErrorCode)
			}
			if err != nil {
				failure(w, err)
				return
			}
			respond(w, http.StatusOK, map[string]bool{"ok": true})
			return
		}
		if r.URL.Path == "/agent/drain-attestation" {
			var attestation platform.DrainAttestation
			if !decode(w, r, &attestation) {
				return
			}
			if err = store.ReportNodeDrainAttestation(ctx, id, fingerprint, attestation); err != nil {
				failure(w, err)
				return
			}
			respond(w, http.StatusOK, map[string]bool{"ok": true})
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
		if r.URL.Path == "/agent/preflight" {
			var report json.RawMessage
			if !decode(w, r, &report) {
				return
			}
			if err := store.ReportPreflight(ctx, id, report, fingerprint); err != nil {
				failure(w, err)
				return
			}
			respond(w, 200, map[string]bool{"ok": true})
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
