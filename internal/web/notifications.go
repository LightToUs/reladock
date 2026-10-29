package web

import (
	"net/http"
	"net/url"
	"strings"

	"reladock/internal/platform"
)

// Parent dispatches after shared Host/CSRF/authentication/rate-limit middleware
// and before generic /api/admin/* routing. Store calls recheck current platform
// privileges and reject bearer/API-key contexts; no delivery token is exposed.
func (s *server) serveNotifications(w http.ResponseWriter, r *http.Request, who platform.Identity) bool {
	if r.URL.Path != "/api/admin/notifications" && !strings.HasPrefix(r.URL.Path, "/api/admin/notifications/") {
		return false
	}
	w.Header().Set("Cache-Control", "no-store")
	if !who.PlatformAdmin {
		failure(w, platform.ErrForbidden)
		return true
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		failure(w, platform.ErrInvalid)
		return true
	}
	switch r.URL.Path {
	case "/api/admin/notifications":
		if r.Method != "GET" {
			respond(w, 405, map[string]string{"error": "方法不支持"})
			return true
		}
		if len(query) > 1 || len(query) == 1 && (len(query["page"]) != 1 || query.Get("page") == "") {
			failure(w, platform.ErrInvalid)
			return true
		}
		page, err := adminPage(r)
		if err != nil {
			failure(w, err)
			return true
		}
		value, err := s.store.AdminNotificationDeliveries(r.Context(), who, page)
		if err != nil {
			failure(w, err)
		} else {
			respond(w, 200, value)
		}
	case "/api/admin/notifications/retry":
		if r.Method != "POST" {
			respond(w, 405, map[string]string{"error": "方法不支持"})
			return true
		}
		if len(query) != 0 {
			failure(w, platform.ErrInvalid)
			return true
		}
		var input struct {
			EventID string `json:"eventId"`
		}
		if !decode(w, r, &input) {
			return true
		}
		if err := s.store.AdminRetryNotification(r.Context(), who, input.EventID); err != nil {
			failure(w, err)
		} else {
			respond(w, 200, map[string]bool{"ok": true})
		}
	default:
		http.NotFound(w, r)
	}
	return true
}
