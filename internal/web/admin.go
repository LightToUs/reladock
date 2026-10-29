package web

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"reladock/internal/platform"
)

var adminNodeID = regexp.MustCompile(`^[a-z][a-z0-9-]{1,63}$`)

func adminPage(r *http.Request) (int, error) {
	value := r.URL.Query().Get("page")
	if value == "" {
		return 1, nil
	}
	page, err := strconv.Atoi(value)
	if err != nil || page < 1 || page > 100000 {
		return 0, platform.ErrInvalid
	}
	return page, nil
}
func (s *server) serveAdmin(w http.ResponseWriter, r *http.Request, who platform.Identity) {
	if !who.PlatformAdmin {
		failure(w, platform.ErrForbidden)
		return
	}
	ctx := r.Context()
	path := strings.TrimPrefix(r.URL.Path, "/api/admin/")
	switch {
	case r.Method == "GET" && path == "overview":
		value, err := s.store.AdminOverview(ctx, who)
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, 200, value)
	case r.Method == "GET" && path == "nodes":
		value, err := s.store.AdminNodes(ctx, who)
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, 200, value)
	case r.Method == "POST" && path == "nodes":
		var input platform.NodeInput
		if !decode(w, r, &input) {
			return
		}
		if err := s.store.AdminAddNode(ctx, who, input); err != nil {
			failure(w, err)
			return
		}
		respond(w, 201, map[string]bool{"ok": true})
	case strings.HasPrefix(path, "nodes/"):
		parts := strings.Split(strings.TrimPrefix(path, "nodes/"), "/")
		if len(parts) != 2 || !adminNodeID.MatchString(parts[0]) {
			failure(w, platform.ErrNotFound)
			return
		}
		id, action := parts[0], parts[1]
		if r.Method == "GET" && action == "deliveries" {
			value, err := s.store.AdminDeliveries(ctx, who, id)
			if err != nil {
				failure(w, err)
				return
			}
			respond(w, 200, value)
			return
		}
		if r.Method != "POST" {
			failure(w, platform.ErrNotFound)
			return
		}
		if action == "update" {
			var input struct {
				platform.NodeInput
				Revision int64 `json:"revision"`
			}
			if !decode(w, r, &input) {
				return
			}
			if input.ID != "" && input.ID != id {
				failure(w, platform.ErrInvalid)
				return
			}
			input.ID = id
			if err := s.store.AdminUpdateNode(ctx, who, input.NodeInput, input.Revision); err != nil {
				failure(w, err)
				return
			}
		} else {
			var input struct {
				Revision    int64  `json:"revision"`
				Certificate string `json:"certificate"`
			}
			if !decode(w, r, &input) {
				return
			}
			if err := s.store.AdminNodeAction(ctx, who, id, input.Revision, action, input.Certificate); err != nil {
				failure(w, err)
				return
			}
		}
		respond(w, 200, map[string]bool{"ok": true})
	case r.Method == "GET" && path == "tenants":
		page, err := adminPage(r)
		if err != nil {
			failure(w, err)
			return
		}
		value, err := s.store.AdminTenants(ctx, who, page)
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, 200, value)
	case r.Method == "POST" && strings.HasPrefix(path, "tenants/"):
		parts := strings.Split(strings.TrimPrefix(path, "tenants/"), "/")
		if len(parts) != 2 || !uuid.MatchString(parts[0]) || parts[1] != "access" {
			failure(w, platform.ErrNotFound)
			return
		}
		var input struct {
			Enabled  *bool `json:"enabled"`
			Revision int64 `json:"revision"`
		}
		if !decode(w, r, &input) {
			return
		}
		if input.Enabled == nil {
			failure(w, platform.ErrInvalid)
			return
		}
		if err := s.store.AdminTenantAccess(ctx, who, parts[0], *input.Enabled, input.Revision); err != nil {
			failure(w, err)
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
	case r.Method == "GET" && path == "rules":
		page, err := adminPage(r)
		tenant := r.URL.Query().Get("tenantId")
		node := r.URL.Query().Get("nodeId")
		if err != nil || tenant != "" && !uuid.MatchString(tenant) || node != "" && !adminNodeID.MatchString(node) {
			failure(w, platform.ErrInvalid)
			return
		}
		value, err := s.store.AdminRules(ctx, who, tenant, node, page)
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, 200, value)
	case r.Method == "POST" && strings.HasPrefix(path, "rules/"):
		parts := strings.Split(strings.TrimPrefix(path, "rules/"), "/")
		if len(parts) != 2 || !uuid.MatchString(parts[0]) {
			failure(w, platform.ErrNotFound)
			return
		}
		var input struct {
			TenantID string `json:"tenantId"`
			Version  int64  `json:"version"`
		}
		if !decode(w, r, &input) {
			return
		}
		if !uuid.MatchString(input.TenantID) {
			failure(w, platform.ErrInvalid)
			return
		}
		value, err := s.store.AdminChangeRule(ctx, who, input.TenantID, parts[0], parts[1], input.Version)
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, 200, value)
	case r.Method == "GET" && path == "audit":
		value, err := s.store.AdminAudit(ctx, who)
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, 200, value)
	default:
		failure(w, platform.ErrNotFound)
	}
}
