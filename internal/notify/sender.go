package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

const MaxResponseBytes = 4096

var ErrDisabled = errors.New("external incident notifications are disabled")
var ErrConfig = errors.New("notification webhook requires an exact HTTPS URL and a secret of at least 32 bytes")
var ErrInvalidEvent = errors.New("invalid incident notification metadata")
var ErrDelivery = errors.New("notification webhook delivery was not acknowledged")

type Config struct {
	URL    string `json:"url"`
	Secret string `json:"-"`
}
type Sender struct {
	endpoint string
	secret   []byte
	client   *http.Client
}

// New is disabled with empty config. Partial config fails closed. The complete
// operator URL is pinned; Event has no destination, path or credential fields.
// Production uses system TLS roots and exposes no alternate transport option.
func New(c Config) (*Sender, error) {
	if c.URL == "" && c.Secret == "" {
		return &Sender{}, nil
	}
	u, e := url.Parse(c.URL)
	if e != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Opaque != "" || strings.ContainsAny(c.URL, "?#") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Path == "" || u.Path[0] != '/' || path.Clean(u.Path) != u.Path || u.RawPath != "" || u.EscapedPath() != u.Path || len(c.Secret) < 32 {
		return nil, ErrConfig
	}
	transport := &http.Transport{
		Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, TLSHandshakeTimeout: 5 * time.Second,
		ResponseHeaderTimeout: 8 * time.Second, MaxResponseHeaderBytes: 16 << 10,
		MaxIdleConns: 4, MaxIdleConnsPerHost: 4, IdleConnTimeout: 30 * time.Second,
	}
	return &Sender{endpoint: u.String(), secret: []byte(c.Secret), client: &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (s *Sender) Enabled() bool {
	return s != nil && s.client != nil && s.endpoint != "" && len(s.secret) >= 32
}

// Send performs one attempt only. Store's durable lease/backoff owns retries.
// A 2xx is a receiver ACK, not proof an operator read or acted on the incident.
// Retries preserve event ID/content but use fresh signed delivery timestamps.
func (s *Sender) Send(ctx context.Context, event Event) error {
	if !s.Enabled() {
		return ErrDisabled
	}
	if !event.Valid() {
		return ErrInvalidEvent
	}
	event.OccurredAt = event.OccurredAt.UTC()
	raw, e := json.Marshal(event)
	if e != nil || len(raw) > MaxEventBytes {
		return ErrInvalidEvent
	}
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(stamp + "."))
	mac.Write(raw)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(raw))
	if e != nil {
		return ErrDelivery
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-RelaDock-Event-ID", event.EventID)
	req.Header.Set("X-RelaDock-Timestamp", stamp)
	req.Header.Set("X-RelaDock-Signature", "v1="+hex.EncodeToString(mac.Sum(nil)))
	response, e := s.client.Do(req)
	if e != nil {
		return ErrDelivery
	}
	defer response.Body.Close()
	body, e := io.ReadAll(io.LimitReader(response.Body, MaxResponseBytes+1))
	if e != nil || len(body) > MaxResponseBytes || response.StatusCode < 200 || response.StatusCode >= 300 {
		return ErrDelivery
	}
	return nil
}
func (s *Sender) Close() {
	if s != nil && s.client != nil {
		s.client.CloseIdleConnections()
	}
}
