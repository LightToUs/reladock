// Package probe observes target TCP reachability and explicitly approved
// cooperating HTTP(S) endpoints. It never establishes end-to-end relay health.
package probe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Type string

const (
	TCP  Type = "tcp"
	HTTP Type = "http"
	UDP  Type = "udp"
)

type Status string

const (
	Unknown              Status = "unknown"
	TargetReachable      Status = "targetReachable"
	TargetUnreachable    Status = "targetUnreachable"
	EndpointResponsive   Status = "endpointResponsive"
	EndpointUnresponsive Status = "endpointUnresponsive"
)
const (
	DefaultTimeout               = 3 * time.Second
	MaxTimeout                   = 10 * time.Second
	MaxResponseBytes       int64 = 4096
	MaxResponseHeaderBytes int64 = 8192
	MaxEndpoints                 = 64
)

var ErrInvalid = errors.New("invalid probe configuration or request")
var nodeID = regexp.MustCompile(`^[a-z][a-z0-9-]{1,63}$`)
var ruleID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Config must come from an authorized operator, not unchecked tenant input.
// AllowedEndpoints is an exact URL allowlist. No credentials, headers, request
// bodies, proxy configuration or insecure TLS options are accepted.
type Config struct {
	AllowedEndpoints []string
	RootCAs          *x509.CertPool
}

// Request binds an observation to a node/rule/config generation. For TCP/UDP,
// provide TargetHost and TargetPort only. For HTTP, provide Endpoint only.
// The caller must authorize the rule target and the probe's execution location.
type Request struct {
	NodeID     string
	RuleID     string
	Generation int64
	Type       Type
	TargetHost string
	TargetPort int
	Endpoint   string
	Timeout    time.Duration
}

// Result contains no target addresses, URLs, headers, response bodies, or error
// strings. LatencyMillis is elapsed wall time, null when no measurement exists.
// HTTP status reports the cooperating endpoint, never the UDP or relay path.
type Result struct {
	NodeID        string    `json:"nodeId"`
	RuleID        string    `json:"ruleId"`
	Generation    int64     `json:"generation"`
	ObservedAt    time.Time `json:"observedAt"`
	Type          Type      `json:"type"`
	Status        Status    `json:"status"`
	LatencyMillis *float64  `json:"latencyMillis"`
}

// Prober is immutable after construction and safe for concurrent Run calls.
// It owns no scheduler, listener, persistent connection, or background goroutine.
type Prober struct {
	endpoints map[string]struct{}
	roots     *x509.CertPool
}

func New(config Config) (*Prober, error) {
	if len(config.AllowedEndpoints) > MaxEndpoints {
		return nil, ErrInvalid
	}
	p := &Prober{endpoints: map[string]struct{}{}}
	for _, endpoint := range config.AllowedEndpoints {
		if !validEndpoint(endpoint) {
			return nil, ErrInvalid
		}
		p.endpoints[endpoint] = struct{}{}
	}
	if config.RootCAs != nil {
		p.roots = config.RootCAs.Clone()
	}
	return p, nil
}

func validHost(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		addr = addr.Unmap()
		return addr.Zone() == "" && !addr.IsUnspecified() && !addr.IsMulticast() && addr.String() != "255.255.255.255"
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}
func validEndpoint(raw string) bool {
	if len(raw) > 2048 || strings.ContainsAny(raw, "%#\\") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || !validHost(u.Hostname()) {
		return false
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
			return false
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return false
	}
	if len(u.Path) < 1 || len(u.Path) > 256 || u.Path[0] != '/' || strings.Contains(u.Path, "//") {
		return false
	}
	for _, part := range strings.Split(u.Path, "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	for _, c := range u.Path {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("/._~-", c)) {
			return false
		}
	}
	return true
}

// Run performs one bounded observation. Network failures/timeouts become typed
// observation statuses with nil error. Invalid input returns ErrInvalid without
// I/O; caller cancellation returns unknown with ctx.Err(). UDP performs no I/O.
func (p *Prober) Run(ctx context.Context, req Request) (Result, error) {
	result := Result{NodeID: req.NodeID, RuleID: req.RuleID, Generation: req.Generation, Type: req.Type, Status: Unknown, ObservedAt: time.Now().UTC()}
	if p == nil || !nodeID.MatchString(req.NodeID) || !ruleID.MatchString(req.RuleID) || req.Generation < 1 || req.Timeout < 0 || req.Timeout > MaxTimeout {
		return result, ErrInvalid
	}
	switch req.Type {
	case TCP, UDP:
		if !validHost(req.TargetHost) || req.TargetPort < 1 || req.TargetPort > 65535 || req.Endpoint != "" {
			return result, ErrInvalid
		}
	case HTTP:
		if _, ok := p.endpoints[req.Endpoint]; !ok || req.TargetHost != "" || req.TargetPort != 0 {
			return result, ErrInvalid
		}
	default:
		return result, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if req.Type == UDP {
		return result, nil
	}
	timeout := req.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	bounded, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	started := time.Now()
	dialer := &net.Dialer{Timeout: timeout, KeepAlive: -1}
	if req.Type == TCP {
		result.Status = TargetUnreachable
		conn, err := dialer.DialContext(bounded, "tcp", net.JoinHostPort(req.TargetHost, strconv.Itoa(req.TargetPort)))
		if err == nil {
			conn.Close()
			result.Status = TargetReachable
		}
	} else {
		result.Status = EndpointUnresponsive
		if p.httpResponsive(bounded, req.Endpoint, timeout, dialer) {
			result.Status = EndpointResponsive
		}
	}
	result.ObservedAt = time.Now().UTC()
	if err := ctx.Err(); err != nil {
		result.Status = Unknown
		return result, err
	}
	latency := float64(time.Since(started)) / float64(time.Millisecond)
	result.LatencyMillis = &latency
	return result, nil
}

func (p *Prober) httpResponsive(ctx context.Context, endpoint string, timeout time.Duration, dialer *net.Dialer) bool {
	transport := &http.Transport{
		Proxy: nil, DialContext: dialer.DialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: p.roots},
		TLSHandshakeTimeout: timeout, ResponseHeaderTimeout: timeout,
		MaxResponseHeaderBytes: MaxResponseHeaderBytes, DisableCompression: true,
		DisableKeepAlives: true, MaxConnsPerHost: 1,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", "RelaDock-Probe/1")
	response, err := client.Do(req)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 || response.ContentLength > MaxResponseBytes || (response.Header.Get("Content-Encoding") != "" && response.Header.Get("Content-Encoding") != "identity") {
		return false
	}
	// Discard at most the fixed limit plus one sentinel byte; never retain body
	// bytes or include endpoint payloads in logs, errors, or Result.
	count, err := io.Copy(io.Discard, io.LimitReader(response.Body, MaxResponseBytes+1))
	return err == nil && count <= MaxResponseBytes
}
