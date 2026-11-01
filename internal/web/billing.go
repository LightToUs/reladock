package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"reladock/internal/platform"
)

// serveBilling is an authenticated extension point for server.serve, after its
// session, Host/origin and rate-limit checks. Integration is owned by the parent.
// GET /api/billing reports readiness, /wallet reads the monetary wallet,
// /ledger accepts optional ruleId and before (exclusive ledger-ID cursor),
// /usage requires ruleId, and /telemetry returns truthful tenant-scoped display
// facts. There is deliberately no tenant usage pool, entitlement, public top-up,
// payment simulation, or wallet mutation route.
func (s *server) serveBilling(w http.ResponseWriter, r *http.Request, who platform.Identity) bool {
	path := r.URL.Path
	if path != "/api/billing" && !strings.HasPrefix(path, "/api/billing/") {
		return false
	}
	w.Header().Set("Cache-Control", "no-store")
	if path != "/api/billing" && path != "/api/billing/wallet" && path != "/api/billing/ledger" && path != "/api/billing/usage" && path != "/api/billing/telemetry" {
		failure(w, platform.ErrNotFound)
		return true
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		respond(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET required"})
		return true
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		failure(w, platform.ErrInvalid)
		return true
	}
	allowed := map[string]bool{}
	if path == "/api/billing/ledger" {
		allowed["ruleId"] = true
		allowed["before"] = true
	}
	if path == "/api/billing/usage" {
		allowed["ruleId"] = true
	}
	if path == "/api/billing/telemetry" {
		allowed["ruleId"] = true
		allowed["window"] = true
	}
	for key, values := range q {
		if !allowed[key] || len(values) != 1 || values[0] == "" {
			failure(w, platform.ErrInvalid)
			return true
		}
	}
	ctx := r.Context()
	switch path {
	case "/api/billing", "/api/billing/wallet":
		wallet, err := s.store.Wallet(ctx, who)
		if err != nil {
			failure(w, err)
			return true
		}
		if path == "/api/billing/wallet" {
			respond(w, 200, wallet)
		} else {
			respond(w, 200, map[string]string{"payments": "not-connected", "pricing": "plans-supported", "metering": "ingestion-supported", "collector": "not-connected", "usageScope": "rule"})
		}
	case "/api/billing/ledger":
		var before int64
		if raw := q.Get("before"); raw != "" {
			before, err = strconv.ParseInt(raw, 10, 64)
			if err != nil || before < 0 {
				failure(w, platform.ErrInvalid)
				return true
			}
		}
		entries, err := s.store.WalletLedger(ctx, who, q.Get("ruleId"), before)
		if err != nil {
			failure(w, err)
			return true
		}
		respond(w, 200, entries)
	case "/api/billing/usage":
		usage, err := s.store.RuleUsage(ctx, who, q.Get("ruleId"))
		if err != nil {
			failure(w, err)
			return true
		}
		respond(w, 200, usage)
	case "/api/billing/telemetry":
		ruleID, window := q.Get("ruleId"), q.Get("window")
		if (ruleID == "") != (window == "") {
			failure(w, platform.ErrInvalid)
			return true
		}
		if ruleID == "" {
			items, err := s.store.RuleTelemetry(ctx, who)
			if err != nil {
				failure(w, err)
				return true
			}
			respond(w, 200, items)
			return true
		}
		if !uuid.MatchString(ruleID) {
			failure(w, platform.ErrInvalid)
			return true
		}
		series, err := s.store.RuleTelemetryHistory(ctx, who, ruleID, window)
		if err != nil {
			failure(w, err)
			return true
		}
		respond(w, 200, series)
	}
	return true
}
