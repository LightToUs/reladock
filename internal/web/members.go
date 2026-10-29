package web

import (
	"net"
	"net/http"
	"strings"
	"time"

	"reladock/internal/platform"
)

// serveMembers is invoked after the parent's shared Host, origin, rate limit
// and authentication middleware. Returns false only for unrelated route paths.
// Secrets appear only in creation responses; no endpoint retrieves them again.
func (s *server) serveMembers(w http.ResponseWriter, r *http.Request, who platform.Identity) bool {
	path := r.URL.Path
	if path != "/api/members" && !strings.HasPrefix(path, "/api/members/") && path != "/api/invitations" && !strings.HasPrefix(path, "/api/invitations/") && path != "/api/api-keys" && !strings.HasPrefix(path, "/api/api-keys/") {
		return false
	}
	w.Header().Set("Cache-Control", "no-store")
	ctx := r.Context()
	switch {
	case r.Method == "GET" && path == "/api/members":
		v, err := s.store.ListMembers(ctx, who)
		if err != nil {
			failure(w, err)
		} else {
			respond(w, 200, v)
		}
	case r.Method == "POST" && strings.HasPrefix(path, "/api/members/"):
		id, ok := memberActionID(path, "/api/members/", "role")
		if !ok {
			failure(w, platform.ErrNotFound)
			return true
		}
		var input struct {
			Role string `json:"role"`
		}
		if !decode(w, r, &input) {
			return true
		}
		if err := s.store.ChangeMemberRole(ctx, who, id, input.Role); err != nil {
			failure(w, err)
		} else {
			respond(w, 200, map[string]bool{"ok": true})
		}
	case r.Method == "GET" && path == "/api/invitations":
		v, err := s.store.ListInvitations(ctx, who)
		if err != nil {
			failure(w, err)
		} else {
			respond(w, 200, v)
		}
	case r.Method == "POST" && path == "/api/invitations":
		var input struct {
			Email     string    `json:"email"`
			Role      string    `json:"role"`
			ExpiresAt time.Time `json:"expiresAt"`
		}
		if !decode(w, r, &input) {
			return true
		}
		v, token, err := s.store.CreateInvitation(ctx, who, input.Email, input.Role, input.ExpiresAt)
		if err != nil {
			failure(w, err)
		} else {
			respond(w, 201, map[string]any{"invitation": v, "token": token})
		}
	case r.Method == "POST" && strings.HasPrefix(path, "/api/invitations/"):
		id, ok := memberActionID(path, "/api/invitations/", "revoke")
		if !ok {
			failure(w, platform.ErrNotFound)
			return true
		}
		var input struct{}
		if !decode(w, r, &input) {
			return true
		}
		if err := s.store.RevokeInvitation(ctx, who, id); err != nil {
			failure(w, err)
		} else {
			respond(w, 200, map[string]bool{"ok": true})
		}
	case r.Method == "GET" && path == "/api/api-keys":
		v, err := s.store.ListAPIKeys(ctx, who)
		if err != nil {
			failure(w, err)
		} else {
			respond(w, 200, v)
		}
	case r.Method == "POST" && path == "/api/api-keys":
		var input struct {
			Name      string    `json:"name"`
			Scope     string    `json:"scope"`
			ExpiresAt time.Time `json:"expiresAt"`
		}
		if !decode(w, r, &input) {
			return true
		}
		v, token, err := s.store.CreateAPIKey(ctx, who, input.Name, input.Scope, input.ExpiresAt)
		if err != nil {
			failure(w, err)
		} else {
			respond(w, 201, map[string]any{"key": v, "token": token})
		}
	case r.Method == "POST" && strings.HasPrefix(path, "/api/api-keys/"):
		id, ok := memberActionID(path, "/api/api-keys/", "revoke")
		if !ok {
			failure(w, platform.ErrNotFound)
			return true
		}
		var input struct{}
		if !decode(w, r, &input) {
			return true
		}
		if err := s.store.RevokeAPIKey(ctx, who, id); err != nil {
			failure(w, err)
		} else {
			respond(w, 200, map[string]bool{"ok": true})
		}
	default:
		respond(w, 405, map[string]string{"error": "方法或路径不支持"})
	}
	return true
}
func memberActionID(path, prefix, action string) (string, bool) {
	parts := strings.Split(strings.TrimPrefix(path, prefix), "/")
	if len(parts) != 2 || !uuid.MatchString(parts[0]) || parts[1] != action {
		return "", false
	}
	return parts[0], true
}

// serveInvitationAccept must run before session authentication, after shared
// Host/origin middleware. Its separate password-work budget is 10/min/direct IP.
// No bearer credential is needed; the email-bound invitation is the credential.
func (s *server) serveInvitationAccept(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != "/api/auth/invitations/accept" {
		return false
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != "POST" {
		w.Header().Set("Allow", "POST")
		respond(w, 405, map[string]string{"error": "方法不支持"})
		return true
	}
	if !s.invitationAcceptAllowed(r) {
		w.Header().Set("Retry-After", "60")
		respond(w, 429, map[string]string{"error": "操作过于频繁，请一分钟后重试"})
		return true
	}
	var input struct {
		Token    string `json:"token"`
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !decode(w, r, &input) {
		return true
	}
	who, token, err := s.store.AcceptInvitation(r.Context(), input.Token, input.Email, input.Password)
	if err != nil {
		failure(w, err)
		return true
	}
	s.sessionCookie(w, token)
	respond(w, 201, who)
	return true
}
func (s *server) invitationAcceptAllowed(r *http.Request) bool {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	key := ip + ":invitation-accept"
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.limits == nil {
		s.limits = map[string]window{}
	}
	current := s.limits[key]
	if now.After(current.until) {
		current = window{until: now.Add(time.Minute)}
	}
	if current.count >= 10 {
		return false
	}
	if len(s.limits) >= 10000 {
		for k, v := range s.limits {
			if now.After(v.until) {
				delete(s.limits, k)
			}
		}
		if len(s.limits) >= 10000 {
			return false
		}
	}
	current.count++
	s.limits[key] = current
	return true
}
