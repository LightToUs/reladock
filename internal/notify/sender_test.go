package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func incidentEvent() Event {
	return Event{EventID: "incident:19834c51-0b88-4d85-8c17-c2805c9ef532:1:open", IncidentID: "19834c51-0b88-4d85-8c17-c2805c9ef532", NodeID: "hk-01", Kind: "contact-stale", State: "open", Occurrences: 1, OccurredAt: time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)}
}
func TestSenderDisabledAndStrictOperatorEndpoint(t *testing.T) {
	disabled, e := New(Config{})
	if e != nil || disabled.Enabled() {
		t.Fatal("default enabled")
	}
	if e = disabled.Send(context.Background(), incidentEvent()); !errors.Is(e, ErrDisabled) {
		t.Fatalf("disabled send %v", e)
	}
	secret := strings.Repeat("s", 32)
	for _, endpoint := range []string{"", "http://example.com/hook", "https://example.com", "https://user:pass@example.com/hook", "https://example.com/hook?q=1", "https://example.com/hook?", "https://example.com/hook#", "https://example.com/a/../hook", "https://example.com/%2fhook"} {
		if _, e := New(Config{URL: endpoint, Secret: secret}); e == nil {
			t.Fatalf("accepted endpoint %q", endpoint)
		}
	}
	if _, e = New(Config{URL: "https://example.com/hook", Secret: strings.Repeat("s", 31)}); e == nil {
		t.Fatal("short secret accepted")
	}
	sender, e := New(Config{URL: "https://example.com/hooks/incidents", Secret: secret})
	if e != nil || !sender.Enabled() {
		t.Fatal(e)
	}
	if sender.client.Timeout != 10*time.Second || sender.client.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("unsafe timeout/proxy")
	}
	raw, e := json.Marshal(Config{URL: "https://example.com/hook", Secret: secret})
	if e != nil || strings.Contains(string(raw), secret) {
		t.Fatal("secret serializes")
	}
}
func TestSenderTLSExactEndpointSignatureAndStableEventID(t *testing.T) {
	secret := strings.Repeat("signing-key-", 4)
	event := incidentEvent()
	calls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "POST" || r.URL.RequestURI() != "/hooks/incidents" || r.URL.RawQuery != "" {
			t.Errorf("destination %s %s", r.Method, r.URL)
		}
		body, e := io.ReadAll(r.Body)
		if e != nil {
			t.Error(e)
		}
		h := hmac.New(sha256.New, []byte(secret))
		h.Write([]byte(r.Header.Get("X-RelaDock-Timestamp") + "."))
		h.Write(body)
		sig, e := hex.DecodeString(strings.TrimPrefix(r.Header.Get("X-RelaDock-Signature"), "v1="))
		if e != nil || !hmac.Equal(sig, h.Sum(nil)) {
			t.Error("signature does not bind timestamp/raw bytes")
		}
		if r.Header.Get("X-RelaDock-Event-ID") != event.EventID || r.Header.Get("Content-Type") != "application/json" {
			t.Error("missing event identity")
		}
		var got Event
		if json.Unmarshal(body, &got) != nil || got.EventID != event.EventID || got.Occurrences != 1 {
			t.Errorf("event %s", body)
		}
		if strings.Contains(string(body), secret) || r.Header.Get("Authorization") != "" {
			t.Error("credential leaked")
		}
		w.WriteHeader(202)
	}))
	defer server.Close()
	sender, e := New(Config{URL: server.URL + "/hooks/incidents", Secret: secret})
	if e != nil {
		t.Fatal(e)
	}
	sender.client.Transport = server.Client().Transport
	for i := 0; i < 2; i++ {
		if e = sender.Send(context.Background(), event); e != nil {
			t.Fatal(e)
		}
	}
	if calls != 2 {
		t.Fatalf("calls=%d", calls)
	}
}
func TestSenderOnly2xxAndBoundedResponseNoRedirect(t *testing.T) {
	for _, status := range []int{200, 204, 299, 300, 302, 400, 429, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Location", "/must-not-follow")
				w.WriteHeader(status)
			}))
			defer server.Close()
			s, e := New(Config{URL: server.URL + "/hook", Secret: strings.Repeat("s", 32)})
			if e != nil {
				t.Fatal(e)
			}
			s.client.Transport = server.Client().Transport
			e = s.Send(context.Background(), incidentEvent())
			if (e == nil) != (status >= 200 && status < 300) || calls != 1 {
				t.Fatalf("status=%d calls=%d error=%v", status, calls, e)
			}
		})
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strings.Repeat("x", MaxResponseBytes+1))
	}))
	defer server.Close()
	s, e := New(Config{URL: server.URL + "/hook", Secret: strings.Repeat("s", 32)})
	if e != nil {
		t.Fatal(e)
	}
	s.client.Transport = server.Client().Transport
	if e = s.Send(context.Background(), incidentEvent()); e == nil {
		t.Fatal("oversized ACK accepted")
	}
}
func TestSenderInvalidEventNeverMakesRequest(t *testing.T) {
	calls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer server.Close()
	s, e := New(Config{URL: server.URL + "/hook", Secret: strings.Repeat("s", 32)})
	if e != nil {
		t.Fatal(e)
	}
	s.client.Transport = server.Client().Transport
	event := incidentEvent()
	event.EventID = "wrong-event-id"
	if e = s.Send(context.Background(), event); !errors.Is(e, ErrInvalidEvent) || calls != 0 {
		t.Fatalf("invalid event calls=%d error=%v", calls, e)
	}
}
