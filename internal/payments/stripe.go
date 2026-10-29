// Package payments implements the optional Stripe transport and verification.
// It never owns wallet state or treats a browser return URL as proof of payment.
package payments

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const MaxWebhookBytes = 256 << 10
const MinTopUpCents int64 = 50
const MaxTopUpCents int64 = 1000000 // Local safety bound: USD 10,000, not a price.
var ErrProvider = errors.New("payment provider unavailable")
var ErrSignature = errors.New("invalid payment signature or event")

type CheckoutRequest struct {
	OrderID, TenantID string
	AmountCents       int64
}
type Session struct {
	ID          string            `json:"id"`
	URL         string            `json:"url"`
	Currency    string            `json:"currency"`
	AmountTotal int64             `json:"amount_total"`
	Metadata    map[string]string `json:"metadata"`
}
type Provider interface {
	CreateCheckout(context.Context, CheckoutRequest) (Session, error)
}

type Config struct {
	SecretKey     string `json:"-"`
	WebhookSecret string `json:"-"`
	SuccessURL    string `json:"successURL"`
	CancelURL     string `json:"cancelURL"`
}

// ConfigFromEnv is disabled when all four variables are absent, and fails closed
// for partial configuration. Secrets are never included in errors or JSON.
func ConfigFromEnv() (Config, bool, error) {
	c := Config{os.Getenv("STRIPE_SECRET_KEY"), os.Getenv("STRIPE_WEBHOOK_SECRET"), os.Getenv("STRIPE_SUCCESS_URL"), os.Getenv("STRIPE_CANCEL_URL")}
	if c == (Config{}) {
		return c, false, nil
	}
	if !validConfig(c) {
		return Config{}, false, ErrProvider
	}
	return c, true, nil
}
func validConfig(c Config) bool {
	return (strings.HasPrefix(c.SecretKey, "sk_test_") || strings.HasPrefix(c.SecretKey, "sk_live_")) && strings.HasPrefix(c.WebhookSecret, "whsec_") && validReturnURL(c.SuccessURL) && validReturnURL(c.CancelURL)
}
func validReturnURL(raw string) bool {
	u, e := url.Parse(raw)
	return e == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.Fragment == ""
}
func ValidCheckoutURL(raw string) bool {
	u, e := url.Parse(raw)
	return e == nil && u.Scheme == "https" && u.Host == "checkout.stripe.com" && u.User == nil
}

type Stripe struct {
	config Config
	client *http.Client
}

func (s *Stripe) LiveMode() bool { return strings.HasPrefix(s.config.SecretKey, "sk_live_") }

func NewStripe(c Config) (*Stripe, error) {
	if !validConfig(c) {
		return nil, ErrProvider
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // Never send merchant credentials through an environment proxy.
	transport.ResponseHeaderTimeout = 10 * time.Second
	return &Stripe{c, &http.Client{Transport: transport, Timeout: 12 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (s *Stripe) CreateCheckout(ctx context.Context, in CheckoutRequest) (Session, error) {
	if in.AmountCents < MinTopUpCents || in.AmountCents > MaxTopUpCents || in.OrderID == "" || in.TenantID == "" {
		return Session{}, ErrProvider
	}
	amount := strconv.FormatInt(in.AmountCents, 10)
	form := url.Values{"mode": {"payment"}, "payment_method_types[0]": {"card"}, "success_url": {s.config.SuccessURL}, "cancel_url": {s.config.CancelURL}, "client_reference_id": {in.OrderID}, "line_items[0][quantity]": {"1"}, "line_items[0][price_data][currency]": {"usd"}, "line_items[0][price_data][unit_amount]": {amount}, "line_items[0][price_data][product_data][name]": {"RelaDock wallet top-up"}}
	for key, value := range map[string]string{"order_id": in.OrderID, "tenant_id": in.TenantID, "amount_cents": amount, "currency": "usd"} {
		form.Set("metadata["+key+"]", value)
		form.Set("payment_intent_data[metadata]["+key+"]", value)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.stripe.com/v1/checkout/sessions", strings.NewReader(form.Encode()))
	if err != nil {
		return Session{}, ErrProvider
	}
	req.SetBasicAuth(s.config.SecretKey, "")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Idempotency-Key", "reladock-checkout-"+in.OrderID)
	req.Header.Set("Stripe-Version", "2024-06-20")
	res, err := s.client.Do(req)
	if err != nil {
		return Session{}, ErrProvider
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, MaxWebhookBytes+1))
	if err != nil || len(raw) > MaxWebhookBytes || res.StatusCode != 200 {
		return Session{}, ErrProvider
	}
	var result Session
	if json.Unmarshal(raw, &result) != nil || result.ID == "" || !ValidCheckoutURL(result.URL) {
		return Session{}, ErrProvider
	}
	return result, nil
}

type Object struct {
	ID             string            `json:"id"`
	Object         string            `json:"object"`
	Status         string            `json:"status"`
	Mode           string            `json:"mode"`
	PaymentStatus  string            `json:"payment_status"`
	PaymentIntent  string            `json:"payment_intent"`
	Currency       string            `json:"currency"`
	AmountTotal    int64             `json:"amount_total"`
	Amount         int64             `json:"amount"`
	AmountRefunded int64             `json:"amount_refunded"`
	Refunded       bool              `json:"refunded"`
	Metadata       map[string]string `json:"metadata"`
}
type Event struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Account  string `json:"account"`
	Livemode bool   `json:"livemode"`
	Data     struct {
		Object Object `json:"object"`
	} `json:"data"`
}

// VerifiedEvent cannot be built by JSON decoding or a tenant API caller.
type VerifiedEvent struct {
	event Event
	hash  string
}

func (v VerifiedEvent) Event() Event { return v.event }
func (v VerifiedEvent) Hash() string { return v.hash }

func VerifyEvent(raw []byte, header, secret string, now time.Time) (VerifiedEvent, error) {
	invalid := func() (VerifiedEvent, error) { return VerifiedEvent{}, ErrSignature }
	if secret == "" || len(raw) > MaxWebhookBytes || len(header) > 8192 {
		return invalid()
	}
	var stamp string
	var signatures []string
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			if stamp != "" {
				return invalid()
			}
			stamp = v
		case "v1":
			signatures = append(signatures, v)
		}
	}
	ts, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil || ts < now.Unix()-300 || ts > now.Unix()+300 {
		return invalid()
	}
	h := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(h, "%s.", stamp)
	h.Write(raw)
	expected := h.Sum(nil)
	match := false
	for _, sig := range signatures {
		decoded, e := hex.DecodeString(sig)
		if e == nil && hmac.Equal(expected, decoded) {
			match = true
		}
	}
	if !match {
		return invalid()
	}
	var event Event
	if json.Unmarshal(raw, &event) != nil || event.ID == "" || len(event.ID) > 255 || event.Type == "" || len(event.Type) > 255 {
		return invalid()
	}
	digest := sha256.Sum256(raw)
	return VerifiedEvent{event, hex.EncodeToString(digest[:])}, nil
}
