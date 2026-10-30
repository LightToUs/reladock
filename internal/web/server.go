package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"reladock/internal/platform"
)

const cookieName = "reladock_session"

type window struct {
	count int
	until time.Time
}
type server struct {
	store            *platform.Store
	assets           fs.FS
	origin           string
	host             string
	mu               sync.Mutex
	limits           map[string]window
	persistentLimits bool
	payments         *Payments
	bootstrap        *BootstrapOptions
}

type Options struct {
	PersistentRateLimit bool
	Payments            *Payments
	Bootstrap           *BootstrapOptions
}

var uuid = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func New(store *platform.Store, assets fs.FS, origin string) http.Handler {
	return NewWithOptions(store, assets, origin, Options{})
}

func NewWithOptions(store *platform.Store, assets fs.FS, origin string, options Options) http.Handler {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" {
		panic("invalid local origin")
	}
	s := &server{store: store, assets: assets, origin: origin, host: parsed.Host, limits: make(map[string]window), persistentLimits: options.PersistentRateLimit, payments: options.Payments}
	s.bootstrap = options.Bootstrap
	return http.HandlerFunc(s.serve)
}
func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}
func failure(w http.ResponseWriter, err error) {
	status := 500
	switch {
	case errors.Is(err, platform.ErrInvalid):
		status = 400
	case errors.Is(err, platform.ErrUnauthorized):
		status = 401
	case errors.Is(err, platform.ErrForbidden):
		status = 403
	case errors.Is(err, platform.ErrNotFound):
		status = 404
	case errors.Is(err, platform.ErrConflict):
		status = 409
	case errors.Is(err, platform.ErrCapacity):
		status = 409
	}
	message := err.Error()
	if status == 500 {
		log.Printf("request failed: %v", err)
		message = "服务暂时不可用，请稍后重试"
	}
	respond(w, status, map[string]string{"error": message})
}
func decode(w http.ResponseWriter, r *http.Request, value any) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		respond(w, 415, map[string]string{"error": "需要 application/json"})
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	err = decoder.Decode(value)
	if err == nil {
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			err = platform.ErrInvalid
		}
	}
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			respond(w, 413, map[string]string{"error": "请求超过 8 KiB"})
		} else {
			failure(w, platform.ErrInvalid)
		}
		return false
	}
	return true
}
func (s *server) allowed(r *http.Request) bool {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	category := "api"
	max := 120
	if strings.HasPrefix(r.URL.Path, "/api/auth/login") || strings.HasPrefix(r.URL.Path, "/api/auth/register") {
		category = "auth"
		max = 10
	}
	if r.URL.Path == "/api/auth/invitations/accept" {
		category = "auth"
		max = 10
	}
	if s.persistentLimits {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		ok, err := s.store.AllowRequest(ctx, ip, category, max)
		return err == nil && ok
	}
	now := time.Now()
	key := ip + ":" + category
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.limits[key]
	if now.After(current.until) {
		current = window{until: now.Add(time.Minute)}
	}
	if current.count >= max {
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
func (s *server) sessionCookie(w http.ResponseWriter, token string) {
	cookie := &http.Cookie{Name: cookieName, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: strings.HasPrefix(s.origin, "https:"), MaxAge: int(platform.SessionLifetime.Seconds()), Expires: time.Now().Add(platform.SessionLifetime)}
	if token == "" {
		cookie.MaxAge = -1
		cookie.Expires = time.Unix(1, 0)
	}
	http.SetCookie(w, cookie)
}
func (s *server) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	if strings.HasPrefix(s.origin, "https://") {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
	}
	if r.Host != s.host {
		failure(w, platform.ErrForbidden)
		return
	}
	if r.URL.Path == "/api/payments/stripe/webhook" {
		if s.payments == nil {
			respond(w, 503, map[string]string{"error": "支付连接器未配置"})
			return
		}
		s.payments.Webhook(w, r)
		return
	}
	if s.serveBootstrap(w, r) {
		return
	}
	if r.URL.Path == "/node-bootstrap.js" && (r.Method == "GET" || r.Method == "HEAD") {
		body, err := fs.ReadFile(s.assets, "node-bootstrap.js")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		if r.Method != "HEAD" {
			w.Write(body)
		}
		return
	}
	if r.URL.Path == "/" || r.URL.Path == "/admin" || r.URL.Path == "/admin.js" || r.URL.Path == "/admin.css" || r.URL.Path == "/app.js" || r.URL.Path == "/app.css" || r.URL.Path == "/rule-submission.js" || r.URL.Path == "/account-tools.js" || r.URL.Path == "/business-tools.js" || r.URL.Path == "/operations-tools.js" || r.URL.Path == "/node-contact-status.js" {
		if r.Method != "GET" && r.Method != "HEAD" {
			respond(w, 405, map[string]string{"error": "方法不支持"})
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}
		if path == "admin" {
			path = "admin.html"
		}
		body, err := fs.ReadFile(s.assets, path)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		contentType := map[string]string{"index.html": "text/html; charset=utf-8", "admin.html": "text/html; charset=utf-8", "admin.js": "text/javascript; charset=utf-8", "admin.css": "text/css; charset=utf-8", "app.js": "text/javascript; charset=utf-8", "rule-submission.js": "text/javascript; charset=utf-8", "app.css": "text/css; charset=utf-8"}
		contentType["account-tools.js"] = "text/javascript; charset=utf-8"
		contentType["business-tools.js"] = "text/javascript; charset=utf-8"
		contentType["operations-tools.js"] = "text/javascript; charset=utf-8"
		contentType["node-contact-status.js"] = "text/javascript; charset=utf-8"
		w.Header().Set("Content-Type", contentType[path])
		w.WriteHeader(200)
		if r.Method != "HEAD" {
			w.Write(body)
		}
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		http.NotFound(w, r)
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		origin := r.Header.Get("Origin")
		if r.Header.Get("X-Requested-With") != "RelaDock" || (origin != "" && origin != s.origin) || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			failure(w, platform.ErrForbidden)
			return
		}
	}
	if !s.allowed(r) {
		w.Header().Set("Retry-After", "60")
		respond(w, 429, map[string]string{"error": "操作过于频繁，请一分钟后重试"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	if s.serveInvitationAccept(w, r) {
		return
	}
	if r.Method == "GET" && r.URL.Path == "/api/health" {
		if err := s.store.Ping(ctx); err != nil {
			respond(w, 503, map[string]string{"status": "unavailable"})
			return
		}
		respond(w, 200, map[string]string{"status": "ok", "storage": "postgresql"})
		return
	}
	if r.Method == "POST" && (r.URL.Path == "/api/auth/register" || r.URL.Path == "/api/auth/login") {
		var who platform.Identity
		var token string
		var err error
		status := 200
		if r.URL.Path == "/api/auth/register" {
			var input struct {
				Company  string `json:"company"`
				Email    string `json:"email"`
				Password string `json:"password"`
			}
			if !decode(w, r, &input) {
				return
			}
			who, token, err = s.store.Register(ctx, input.Company, input.Email, input.Password)
			status = 201
		} else {
			var input struct {
				Email    string `json:"email"`
				Password string `json:"password"`
			}
			if !decode(w, r, &input) {
				return
			}
			who, token, err = s.store.Login(ctx, input.Email, input.Password)
		}
		if err != nil {
			failure(w, err)
			return
		}
		s.sessionCookie(w, token)
		respond(w, status, who)
		return
	}
	var who platform.Identity
	var err error
	cookie, cookieErr := r.Cookie(cookieName)
	bearer := r.Header.Get("Authorization")
	if bearer != "" {
		if !strings.HasPrefix(bearer, "Bearer ") || cookieErr == nil {
			failure(w, platform.ErrUnauthorized)
			return
		}
		token := strings.TrimPrefix(bearer, "Bearer ")
		ctx = platform.WithAPIKey(ctx, token)
		r = r.WithContext(ctx)
		who, err = s.store.APIKey(ctx, token)
		if strings.HasPrefix(r.URL.Path, "/api/admin/") || r.URL.Path == "/api/auth/logout" {
			failure(w, platform.ErrForbidden)
			return
		}
	} else {
		if cookieErr != nil {
			failure(w, platform.ErrUnauthorized)
			return
		}
		who, err = s.store.Session(ctx, cookie.Value)
	}
	if err != nil {
		failure(w, err)
		return
	}
	if s.serveSubscriptions(w, r, who) || s.serveOperations(w, r, who) || s.serveCosts(w, r, who) || s.serveNotifications(w, r, who) {
		return
	}
	if r.URL.Path == "/api/payments/status" && r.Method == "GET" {
		respond(w, 200, map[string]bool{"enabled": s.payments.Enabled()})
		return
	}
	if r.URL.Path == "/api/payments/checkout" {
		if s.payments == nil {
			respond(w, 503, map[string]string{"error": "支付连接器未配置"})
			return
		}
		s.payments.Checkout(w, r, who)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/admin/") {
		s.serveAdmin(w, r, who)
		return
	}
	if s.serveMembers(w, r, who) || s.serveBilling(w, r, who) {
		return
	}
	switch {
	case r.Method == "GET" && r.URL.Path == "/api/auth/me":
		respond(w, 200, who)
	case r.Method == "POST" && r.URL.Path == "/api/auth/logout":
		if err = s.store.Logout(ctx, cookie.Value); err != nil {
			failure(w, err)
			return
		}
		s.sessionCookie(w, "")
		respond(w, 200, map[string]bool{"ok": true})
	case r.Method == "GET" && r.URL.Path == "/api/overview":
		rules, err := s.store.ListRules(ctx, who)
		if err != nil {
			failure(w, err)
			return
		}
		logs, err := s.store.AuditLogs(ctx, who)
		if err != nil {
			failure(w, err)
			return
		}
		nodes, err := s.store.ListNodes(ctx)
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, 200, map[string]any{"account": who, "nodes": nodes, "rules": rules, "auditLogs": logs, "storage": "postgresql", "services": map[string]string{"agent": "validation-only-supported", "metering": "ingestion-supported", "collector": "not-connected", "payments": "not-connected"}})
	case r.Method == "GET" && r.URL.Path == "/api/nodes":
		nodes, err := s.store.ListNodes(ctx)
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, 200, nodes)
	case r.Method == "GET" && r.URL.Path == "/api/rules":
		rules, err := s.store.ListRules(ctx, who)
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, 200, rules)
	case r.Method == "POST" && r.URL.Path == "/api/rules":
		var input platform.RuleInput
		if !decode(w, r, &input) {
			return
		}
		rule, err := s.store.CreateRule(ctx, who, r.Header.Get("Idempotency-Key"), input)
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, 201, rule)
	case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/api/rules/"):
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/rules/"), "/")
		if len(parts) != 2 || !uuid.MatchString(parts[0]) {
			failure(w, platform.ErrNotFound)
			return
		}
		if parts[1] == "resume" {
			rule, err := s.store.ResumeRule(ctx, who, parts[0])
			if err != nil {
				failure(w, err)
				return
			}
			respond(w, 200, rule)
		} else if parts[1] == "pause" {
			rule, err := s.store.PauseRule(ctx, who, parts[0])
			if err != nil {
				failure(w, err)
				return
			}
			respond(w, 200, rule)
		} else if parts[1] == "delete" {
			if err := s.store.DeleteRule(ctx, who, parts[0]); err != nil {
				failure(w, err)
				return
			}
			respond(w, 200, map[string]bool{"ok": true})
		} else {
			failure(w, platform.ErrNotFound)
		}
	default:
		respond(w, 404, map[string]string{"error": "接口不存在"})
	}
}
