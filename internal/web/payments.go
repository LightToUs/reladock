package web

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"reladock/internal/payments"
	"reladock/internal/platform"
)

// Payments is an optional extension. The parent resolves the session/API key,
// enforces Host and rate limits, then passes the identity to Checkout. Webhook
// must be dispatched before cookie authentication and browser CSRF middleware.
type Payments struct {
	store    *platform.Store
	provider payments.Provider
	secret   string
	origin   string
	live     bool
	mu       sync.Mutex
	limits   map[string]window
}

// NewPayments is disabled unless both provider and webhook secret are supplied.
// Supply the configured public origin as the optional fourth argument behind a
// TLS-terminating proxy; forwarded headers are deliberately never trusted here.
func NewPayments(store *platform.Store, provider payments.Provider, secret string, origin ...string) *Payments {
	p := &Payments{store: store, provider: provider, secret: secret, limits: make(map[string]window)}
	if mode, ok := provider.(interface{ LiveMode() bool }); ok {
		p.live = mode.LiveMode()
	}
	if len(origin) > 0 {
		p.origin = origin[0]
	}
	return p
}
func (p *Payments) Enabled() bool { return p != nil && p.provider != nil && p.secret != "" }
func (p *Payments) available(w http.ResponseWriter) bool {
	w.Header().Set("Cache-Control", "no-store")
	if !p.Enabled() {
		respond(w, 503, map[string]string{"error": "支付连接器未配置"})
		return false
	}
	return true
}
func (p *Payments) Checkout(w http.ResponseWriter, r *http.Request, who platform.Identity) {
	if !p.available(w) {
		return
	}
	if r.Method != "POST" {
		respond(w, 405, map[string]string{"error": "方法不支持"})
		return
	}
	if who.MemberID == "" || who.TenantID == "" {
		failure(w, platform.ErrUnauthorized)
		return
	}
	origin := p.origin
	if origin == "" {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		origin = scheme + "://" + r.Host
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host != r.Host || r.Header.Get("Origin") != origin || r.Header.Get("X-Requested-With") != "RelaDock" || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		failure(w, platform.ErrForbidden)
		return
	}
	if len(r.URL.Query()) != 0 {
		failure(w, platform.ErrInvalid)
		return
	}
	var in struct {
		AmountCents int64 `json:"amountCents"`
	}
	if !decode(w, r, &in) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	order, err := p.store.CheckoutPayment(ctx, who, r.Header.Get("Idempotency-Key"), in.AmountCents, p.provider)
	if errors.Is(err, payments.ErrProvider) {
		respond(w, 503, map[string]string{"error": "支付渠道暂时不可用，请使用相同请求键重试"})
		return
	}
	if err != nil {
		failure(w, err)
		return
	}
	respond(w, 201, order)
}
func (p *Payments) Webhook(w http.ResponseWriter, r *http.Request) {
	if !p.available(w) {
		return
	}
	if r.Method != "POST" {
		respond(w, 405, map[string]string{"error": "方法不支持"})
		return
	}
	if !p.webhookAllowed(r) {
		w.Header().Set("Retry-After", "60")
		respond(w, 429, map[string]string{"error": "支付事件过于频繁"})
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, payments.MaxWebhookBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			respond(w, 413, map[string]string{"error": "支付事件超过 256 KiB"})
		} else {
			failure(w, platform.ErrInvalid)
		}
		return
	}
	event, err := payments.VerifyEvent(raw, r.Header.Get("Stripe-Signature"), p.secret, time.Now())
	if err != nil {
		respond(w, 400, map[string]string{"error": "支付事件验签失败"})
		return
	}
	if e := event.Event(); e.Account != "" || e.Livemode != p.live {
		respond(w, 400, map[string]string{"error": "支付事件账户或模式不匹配"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err = p.store.ApplyStripeEvent(ctx, event); err != nil {
		failure(w, err)
		return
	}
	respond(w, 200, map[string]bool{"received": true})
}

// The webhook has its own bounded per-process budget, separate from browser
// API limits. Only the direct peer IP is trusted. A reverse proxy should also
// enforce body/rate limits; forwarded IP headers cannot bypass this budget.
func (p *Payments) webhookAllowed(r *http.Request) bool {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	current := p.limits[ip]
	if now.After(current.until) {
		current = window{until: now.Add(time.Minute)}
	}
	if current.count >= 120 {
		return false
	}
	if _, exists := p.limits[ip]; !exists && len(p.limits) >= 10000 {
		for key, value := range p.limits {
			if now.After(value.until) {
				delete(p.limits, key)
			}
		}
		if len(p.limits) >= 10000 {
			return false
		}
	}
	current.count++
	p.limits[ip] = current
	return true
}
