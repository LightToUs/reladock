package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"reladock/internal/platform"
)

// Parent calls this after shared security/authentication middleware and BEFORE
// generic /api/admin/* routing. Every store call rechecks the relevant tenancy
// or platform permission. Bearer contexts are rejected for all admin operations.
func (s *server) serveOperations(w http.ResponseWriter, r *http.Request, who platform.Identity) bool {
	admin := strings.HasPrefix(r.URL.Path, "/api/admin/operations/") || r.URL.Path == "/api/admin/operations"
	tenant := strings.HasPrefix(r.URL.Path, "/api/operations/") || r.URL.Path == "/api/operations"
	if !admin && !tenant {
		return false
	}
	w.Header().Set("Cache-Control", "no-store")
	prefix := "/api/operations/"
	if admin {
		prefix = "/api/admin/operations/"
	}
	path := strings.TrimPrefix(r.URL.Path, prefix)
	ctx := r.Context()
	switch {
	case r.Method == "GET" && path == "probes" && !admin:
		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil || len(query) != 1 || len(query["ruleId"]) != 1 || !uuid.MatchString(query.Get("ruleId")) {
			failure(w, platform.ErrInvalid)
			return true
		}
		results, err := s.store.ProbeResults(ctx, who, query.Get("ruleId"))
		if err != nil {
			failure(w, err)
		} else {
			respond(w, 200, results)
		}
	case r.Method == "GET" && path == "nodes":
		page, err := adminPage(r)
		if err != nil {
			failure(w, err)
			return true
		}
		staleAfter := platform.DefaultContactStaleAfter
		if value := r.URL.Query().Get("staleAfterSeconds"); value != "" {
			seconds, e := strconv.ParseInt(value, 10, 64)
			if e != nil || seconds < 1 || seconds > 30*24*60*60 {
				failure(w, platform.ErrInvalid)
				return true
			}
			staleAfter = time.Duration(seconds) * time.Second
		}
		value, err := s.store.ListOperationsNodes(ctx, who, admin, staleAfter, page)
		if err != nil {
			failure(w, err)
		} else {
			respond(w, 200, value)
		}
	case r.Method == "GET" && path == "incidents":
		page, err := adminPage(r)
		if err != nil {
			failure(w, err)
			return true
		}
		value, err := s.store.ListIncidents(ctx, who, admin, page)
		if err != nil {
			failure(w, err)
		} else {
			respond(w, 200, value)
		}
	case r.Method == "GET" && path == "tickets":
		page, err := adminPage(r)
		if err != nil {
			failure(w, err)
			return true
		}
		value, err := s.store.ListSupportTickets(ctx, who, admin, page)
		if err != nil {
			failure(w, err)
		} else {
			respond(w, 200, value)
		}
	case r.Method == "POST" && path == "tickets" && !admin:
		var input struct {
			Subject string `json:"subject"`
			Body    string `json:"body"`
		}
		if !decode(w, r, &input) {
			return true
		}
		value, err := s.store.CreateSupportTicket(ctx, who, input.Subject, input.Body)
		if err != nil {
			failure(w, err)
		} else {
			respond(w, 201, value)
		}
	case r.Method == "GET" && strings.HasPrefix(path, "tickets/"):
		id := strings.TrimPrefix(path, "tickets/")
		if !uuid.MatchString(id) {
			failure(w, platform.ErrNotFound)
			return true
		}
		value, err := s.store.SupportThread(ctx, who, id, admin)
		if err != nil {
			failure(w, err)
		} else {
			respond(w, 200, value)
		}
	case r.Method == "POST" && strings.HasPrefix(path, "tickets/"):
		id, ok := memberActionID(path, "tickets/", "replies")
		if !ok {
			failure(w, platform.ErrNotFound)
			return true
		}
		var input struct {
			Body     string `json:"body"`
			Status   string `json:"status"`
			Revision int64  `json:"revision"`
		}
		if !decode(w, r, &input) {
			return true
		}
		value, err := s.store.ReplySupportTicket(ctx, who, id, input.Body, input.Status, input.Revision, admin)
		if err != nil {
			failure(w, err)
		} else {
			respond(w, 200, value)
		}
	default:
		failure(w, platform.ErrNotFound)
	}
	return true
}
