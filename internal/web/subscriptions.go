package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"

	"reladock/internal/platform"
)

type adminPlanRequest struct {
	OfferingID    string                `json:"offeringId"`
	Code          string                `json:"code"`
	Version       json.RawMessage       `json:"version"`
	Name          string                `json:"name"`
	PriceCents    int64                 `json:"priceCents"`
	QuotaBytes    int64                 `json:"quotaBytes"`
	BandwidthMbps int                   `json:"bandwidthMbps"`
	PeriodSeconds int64                 `json:"periodSeconds"`
	ResourceMode  platform.ResourceMode `json:"resourceMode"`
}

// serveSubscriptions is wired by the parent after session, same-origin and rate
// checks, BEFORE /api/admin/* dispatch. GET /api/plans, /api/admin/plans and
// /api/admin/offerings accept page. POST /api/admin/offerings creates a concrete
// node-bound line; POST /api/admin/offerings/state changes only its sale state;
// POST /api/admin/offerings/delete removes only an unreferenced draft.
// POST /api/admin/plans creates an immutable version, automatically assigning
// the next version when the field is omitted or zero. GET
// /api/subscriptions requires ruleId and accepts an exclusive before ordinal.
// POST /api/subscriptions/purchase uses Idempotency-Key and {ruleId,priceVersionId}.
// Purchase requests a configuration; it does not assert that traffic is running.
func (s *server) serveSubscriptions(w http.ResponseWriter, r *http.Request, who platform.Identity) bool {
	path := r.URL.Path
	if path == "/api/subscriptions/auto-renew" {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != "POST" {
			w.Header().Set("Allow", "POST")
			respond(w, 405, map[string]string{"error": "method not supported"})
			return true
		}
		if r.URL.RawQuery != "" {
			failure(w, platform.ErrInvalid)
			return true
		}
		var input struct {
			SubscriptionID string `json:"subscriptionId"`
			Enabled        *bool  `json:"enabled"`
		}
		if !decode(w, r, &input) {
			return true
		}
		if input.Enabled == nil {
			failure(w, platform.ErrInvalid)
			return true
		}
		p, err := s.store.SetSubscriptionAutoRenew(r.Context(), who, input.SubscriptionID, *input.Enabled)
		if err != nil {
			failure(w, err)
			return true
		}
		respond(w, 200, p)
		return true
	}
	if path != "/api/plans" && path != "/api/admin/plans" && path != "/api/admin/offerings" && path != "/api/admin/offerings/state" && path != "/api/admin/offerings/delete" && path != "/api/subscriptions" && path != "/api/subscriptions/purchase" {
		return false
	}
	w.Header().Set("Cache-Control", "no-store")
	allowedMethods := "GET"
	if path == "/api/admin/plans" || path == "/api/admin/offerings" {
		allowedMethods = "GET, POST"
	}
	if path == "/api/subscriptions/purchase" || path == "/api/admin/offerings/state" || path == "/api/admin/offerings/delete" {
		allowedMethods = "POST"
	}
	validMethod := r.Method == "GET" && path != "/api/subscriptions/purchase" && path != "/api/admin/offerings/state" && path != "/api/admin/offerings/delete" || r.Method == "POST" && (path == "/api/admin/plans" || path == "/api/admin/offerings" || path == "/api/admin/offerings/state" || path == "/api/admin/offerings/delete" || path == "/api/subscriptions/purchase")
	if !validMethod {
		w.Header().Set("Allow", allowedMethods)
		respond(w, 405, map[string]string{"error": "method not supported"})
		return true
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		failure(w, platform.ErrInvalid)
		return true
	}
	allowed := map[string]bool{}
	if r.Method == "GET" {
		if path == "/api/subscriptions" {
			allowed["ruleId"] = true
			allowed["before"] = true
			allowed["unbound"] = true
			allowed["all"] = true
		} else {
			allowed["page"] = true
			if path == "/api/plans" {
				allowed["ruleId"] = true
			}
		}
	}
	for key, values := range q {
		if !allowed[key] || len(values) != 1 || values[0] == "" {
			failure(w, platform.ErrInvalid)
			return true
		}
	}
	ctx := r.Context()
	switch {
	case r.Method == "POST" && path == "/api/admin/offerings":
		var input platform.LineOfferingInput
		if !decode(w, r, &input) {
			return true
		}
		offering, err := s.store.AdminCreateOffering(ctx, who, input)
		if err != nil {
			failure(w, err)
			return true
		}
		respond(w, 201, offering)
	case r.Method == "POST" && path == "/api/admin/offerings/state":
		var input struct {
			OfferingID       string                 `json:"offeringId"`
			ExpectedRevision int64                  `json:"expectedRevision"`
			State            platform.OfferingState `json:"state"`
		}
		if !decode(w, r, &input) {
			return true
		}
		offering, err := s.store.AdminSetOfferingState(ctx, who, input.OfferingID, platform.OfferingStateInput{ExpectedRevision: input.ExpectedRevision, State: input.State})
		if err != nil {
			failure(w, err)
			return true
		}
		respond(w, 200, offering)
	case r.Method == "POST" && path == "/api/admin/offerings/delete":
		var input struct {
			OfferingID       string `json:"offeringId"`
			ExpectedRevision int64  `json:"expectedRevision"`
		}
		if !decode(w, r, &input) {
			return true
		}
		if err := s.store.AdminDeleteOffering(ctx, who, input.OfferingID, input.ExpectedRevision); err != nil {
			failure(w, err)
			return true
		}
		respond(w, 200, map[string]string{"status": "deleted"})
	case r.Method == "POST" && path == "/api/admin/plans":
		var request adminPlanRequest
		if !decode(w, r, &request) {
			return true
		}
		input := platform.PlanInput{
			OfferingID:    request.OfferingID,
			Code:          request.Code,
			Name:          request.Name,
			PriceCents:    request.PriceCents,
			QuotaBytes:    request.QuotaBytes,
			BandwidthMbps: request.BandwidthMbps,
			PeriodSeconds: request.PeriodSeconds,
			ResourceMode:  request.ResourceMode,
		}
		if raw := bytes.TrimSpace(request.Version); len(raw) > 0 {
			if bytes.Equal(raw, []byte("null")) || json.Unmarshal(raw, &input.Version) != nil {
				failure(w, platform.ErrInvalid)
				return true
			}
		}
		plan, err := s.store.AdminCreatePlan(ctx, who, input)
		if err != nil {
			failure(w, err)
			return true
		}
		respond(w, 201, plan)
	case r.Method == "POST" && path == "/api/subscriptions/purchase":
		var input platform.PurchasePlanInput
		if !decode(w, r, &input) {
			return true
		}
		period, err := s.store.PurchasePlan(ctx, who, r.Header.Get("Idempotency-Key"), input)
		if err != nil {
			failure(w, err)
			return true
		}
		respond(w, 201, period)
	case path == "/api/subscriptions":
		if q.Get("all") != "" {
			if q.Get("all") != "true" || q.Get("ruleId") != "" || q.Get("unbound") != "" {
				failure(w, platform.ErrInvalid)
				return true
			}
			var before int64
			if raw := q.Get("before"); raw != "" {
				before, err = strconv.ParseInt(raw, 10, 64)
				if err != nil || before < 0 {
					failure(w, platform.ErrInvalid)
					return true
				}
			}
			periods, err := s.store.TenantSubscriptions(r.Context(), who, before)
			if err != nil {
				failure(w, err)
				return true
			}
			respond(w, 200, periods)
			return true
		}
		if raw := q.Get("unbound"); raw != "" {
			if raw != "true" || q.Get("ruleId") != "" || q.Get("before") != "" {
				failure(w, platform.ErrInvalid)
				return true
			}
			periods, err := s.store.UnboundSubscriptions(ctx, who)
			if err != nil {
				failure(w, err)
				return true
			}
			respond(w, 200, periods)
			return true
		}
		var before int64
		if raw := q.Get("before"); raw != "" {
			before, err = strconv.ParseInt(raw, 10, 64)
			if err != nil || before < 0 {
				failure(w, platform.ErrInvalid)
				return true
			}
		}
		periods, err := s.store.RuleSubscriptions(ctx, who, q.Get("ruleId"), before)
		if err != nil {
			failure(w, err)
			return true
		}
		respond(w, 200, periods)
	case path == "/api/admin/offerings":
		page, err := adminPage(r)
		if err != nil {
			failure(w, err)
			return true
		}
		offerings, err := s.store.AdminOfferings(ctx, who, page)
		if err != nil {
			failure(w, err)
			return true
		}
		respond(w, 200, offerings)
	default:
		page, err := adminPage(r)
		if err != nil {
			failure(w, err)
			return true
		}
		var plans platform.Page[platform.PlanPriceVersion]
		if path == "/api/admin/plans" {
			plans, err = s.store.AdminPlans(ctx, who, page)
		} else {
			plans, err = s.store.ListPlans(ctx, who, q.Get("ruleId"), page)
		}
		if err != nil {
			failure(w, err)
			return true
		}
		respond(w, 200, plans)
	}
	return true
}
