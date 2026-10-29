package web

import (
	"net/http"
	"strings"
	"time"

	"reladock/internal/platform"
)

// Parent invokes serveCosts after shared security/authentication middleware and
// BEFORE generic admin routing. Store authorization also denies key contexts.
func (s *server) serveCosts(w http.ResponseWriter, r *http.Request, who platform.Identity) bool {
	if r.URL.Path != "/api/admin/costs" && !strings.HasPrefix(r.URL.Path, "/api/admin/costs/") {
		return false
	}
	w.Header().Set("Cache-Control", "no-store")
	if !who.PlatformAdmin {
		failure(w, platform.ErrForbidden)
		return true
	}
	switch {
	case r.Method == "POST" && r.URL.Path == "/api/admin/costs":
		var input platform.NodeCostInput
		if !decode(w, r, &input) {
			return true
		}
		value, err := s.store.AdminRecordNodeCost(r.Context(), who, r.Header.Get("Idempotency-Key"), input)
		if err != nil {
			failure(w, err)
		} else {
			respond(w, 201, value)
		}
	case r.Method == "GET" && (r.URL.Path == "/api/admin/costs" || r.URL.Path == "/api/admin/costs/margin"):
		from, err := time.Parse(time.RFC3339Nano, r.URL.Query().Get("from"))
		if err != nil {
			failure(w, platform.ErrInvalid)
			return true
		}
		to, err := time.Parse(time.RFC3339Nano, r.URL.Query().Get("to"))
		if err != nil {
			failure(w, platform.ErrInvalid)
			return true
		}
		page, err := adminPage(r)
		if err != nil {
			failure(w, err)
			return true
		}
		if r.URL.Path == "/api/admin/costs/margin" {
			value, err := s.store.AdminCashMargin(r.Context(), who, from, to, page)
			if err != nil {
				failure(w, err)
			} else {
				respond(w, 200, value)
			}
		} else {
			value, err := s.store.AdminNodeCosts(r.Context(), who, from, to, page)
			if err != nil {
				failure(w, err)
			} else {
				respond(w, 200, value)
			}
		}
	default:
		failure(w, platform.ErrNotFound)
	}
	return true
}
