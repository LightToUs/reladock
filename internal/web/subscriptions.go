package web

import (
	"net/http"
	"net/url"
	"strconv"

	"reladock/internal/platform"
)

// serveSubscriptions is wired by the parent after session, same-origin and rate
// checks, BEFORE /api/admin/* dispatch. GET /api/plans and /api/admin/plans accept
// page; POST /api/admin/plans creates an explicit immutable version. GET
// /api/subscriptions requires ruleId and accepts an exclusive before ordinal.
// POST /api/subscriptions/purchase uses Idempotency-Key and {ruleId,priceVersionId}.
// Purchase requests a configuration; it does not assert that traffic is running.
func (s *server) serveSubscriptions(w http.ResponseWriter, r *http.Request, who platform.Identity) bool {
	path := r.URL.Path
	if path != "/api/plans" && path != "/api/admin/plans" && path != "/api/subscriptions" && path != "/api/subscriptions/purchase" {
		return false
	}
	w.Header().Set("Cache-Control", "no-store")
	allowedMethods := "GET"
	if path == "/api/admin/plans" {
		allowedMethods = "GET, POST"
	}
	if path == "/api/subscriptions/purchase" {
		allowedMethods = "POST"
	}
	validMethod := r.Method == "GET" && path != "/api/subscriptions/purchase" || r.Method == "POST" && (path == "/api/admin/plans" || path == "/api/subscriptions/purchase")
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
		} else {
			allowed["page"] = true
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
	case r.Method == "POST" && path == "/api/admin/plans":
		var input platform.PlanInput
		if !decode(w, r, &input) {
			return true
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
			plans, err = s.store.ListPlans(ctx, who, page)
		}
		if err != nil {
			failure(w, err)
			return true
		}
		respond(w, 200, plans)
	}
	return true
}
