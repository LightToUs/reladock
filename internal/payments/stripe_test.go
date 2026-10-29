package payments

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func signed(body []byte, ts int64, secret string) string {
	h := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(h, "%d.", ts)
	h.Write(body)
	return fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(h.Sum(nil)))
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestStripeCheckoutFixedHostFormAndIdempotency(t *testing.T) {
	c := Config{SecretKey: "sk_test_dummy", WebhookSecret: "whsec_dummy", SuccessURL: "https://example.com/return", CancelURL: "https://example.com/cancel"}
	s, e := NewStripe(c)
	if e != nil {
		t.Fatal(e)
	}
	if s.client.Timeout == 0 || s.client.CheckRedirect == nil {
		t.Fatal("outbound request lacks timeout/redirect policy")
	}
	if e = s.client.CheckRedirect(nil, nil); e != http.ErrUseLastResponse {
		t.Fatal("redirect permitted")
	}
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://api.stripe.com/v1/checkout/sessions" || r.Method != "POST" {
			t.Fatalf("wrong destination: %s %s", r.Method, r.URL)
		}
		if r.Header.Get("Idempotency-Key") != "reladock-checkout-order-test" {
			t.Fatal("unstable idempotency key")
		}
		user, pass, ok := r.BasicAuth()
		if !ok || user != "sk_test_dummy" || pass != "" {
			t.Fatal("server credential missing")
		}
		if e := r.ParseForm(); e != nil {
			t.Fatal(e)
		}
		for key, value := range map[string]string{"mode": "payment", "metadata[order_id]": "order-test", "metadata[tenant_id]": "tenant-test", "metadata[amount_cents]": "500", "metadata[currency]": "usd", "payment_intent_data[metadata][order_id]": "order-test", "line_items[0][price_data][unit_amount]": "500", "line_items[0][price_data][currency]": "usd", "payment_method_types[0]": "card"} {
			if r.Form.Get(key) != value {
				t.Fatalf("%s=%q", key, r.Form.Get(key))
			}
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"id":"cs_test","url":"https://checkout.stripe.com/c/pay/test","currency":"usd","amount_total":500,"metadata":{"order_id":"order-test","tenant_id":"tenant-test","amount_cents":"500","currency":"usd"}}`))}, nil
	})
	for i := 0; i < 2; i++ {
		session, e := s.CreateCheckout(context.Background(), CheckoutRequest{OrderID: "order-test", TenantID: "tenant-test", AmountCents: 500})
		if e != nil || session.ID != "cs_test" {
			t.Fatalf("session %+v %v", session, e)
		}
	}
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader("sk_test_dummy diagnostic credential"))}, nil
	})
	if _, e = s.CreateCheckout(context.Background(), CheckoutRequest{OrderID: "order-test", TenantID: "tenant-test", AmountCents: 500}); e != ErrProvider || strings.Contains(e.Error(), "dummy") {
		t.Fatalf("provider error leaks secret: %v", e)
	}
}
func TestConfigDisabledPartialAndMode(t *testing.T) {
	for _, key := range []string{"STRIPE_SECRET_KEY", "STRIPE_WEBHOOK_SECRET", "STRIPE_SUCCESS_URL", "STRIPE_CANCEL_URL"} {
		t.Setenv(key, "")
	}
	if _, on, e := ConfigFromEnv(); e != nil || on {
		t.Fatal("enabled without config")
	}
	t.Setenv("STRIPE_SECRET_KEY", "sk_test_dummy")
	if _, on, e := ConfigFromEnv(); e == nil || on {
		t.Fatal("partial config enabled")
	}
	for _, key := range []string{"sk_test_dummy", "sk_live_dummy"} {
		s, e := NewStripe(Config{key, "whsec_dummy", "https://example.com/return", "https://example.com/cancel"})
		if e != nil || s.LiveMode() != strings.HasPrefix(key, "sk_live_") {
			t.Fatalf("mode %v", e)
		}
	}
	encoded, e := json.Marshal(Config{"sk_test_dummy", "whsec_dummy", "https://example.com/return", "https://example.com/cancel"})
	if e != nil || strings.Contains(string(encoded), "dummy") {
		t.Fatalf("serialized config leaks credentials: %s %v", encoded, e)
	}
}

func TestSignatureRawBodyTimestampAndRotation(t *testing.T) {
	now := time.Unix(1700000000, 0)
	body := []byte(`{"id":"evt_test","type":"checkout.session.completed","data":{"object":{}}}`)
	for _, tc := range []struct {
		name           string
		body           []byte
		header, secret string
		valid          bool
	}{
		{"valid", body, signed(body, now.Unix(), "test-secret"), "test-secret", true},
		{"rotation", body, signed(body, now.Unix(), "test-secret") + ",v1=0000", "test-secret", true},
		{"rotation valid second", body, strings.Replace(signed(body, now.Unix(), "test-secret"), ",v1=", ",v1=0000,v1=", 1), "test-secret", true},
		{"v0 rejected", body, strings.Replace(signed(body, now.Unix(), "test-secret"), ",v1=", ",v0=", 1), "test-secret", false},
		{"changed bytes", append(append([]byte{}, body...), '\n'), signed(body, now.Unix(), "test-secret"), "test-secret", false},
		{"stale", body, signed(body, now.Unix()-301, "test-secret"), "test-secret", false},
		{"future", body, signed(body, now.Unix()+301, "test-secret"), "test-secret", false},
		{"wrong secret", body, signed(body, now.Unix(), "other"), "test-secret", false},
		{"disabled", body, signed(body, now.Unix(), ""), "", false},
		{"missing", body, "", "test-secret", false},
		{"duplicate timestamp", body, signed(body, now.Unix(), "test-secret") + ",t=1700000000", "test-secret", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := VerifyEvent(tc.body, tc.header, tc.secret, now)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
		})
	}
}
