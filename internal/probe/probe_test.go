package probe

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func requestFor(kind Type) Request {
	return Request{NodeID: "hk-01", RuleID: "a1b2c3d4-0000-4000-8000-000000000001", Generation: 7, Type: kind, Timeout: time.Second}
}
func newProber(t *testing.T, config Config) *Prober {
	t.Helper()
	p, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTCPConnectReportsOnlyTargetReachabilityAndSendsNoPayload(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	received := make(chan int, 1)
	go func() {
		conn, e := listener.Accept()
		if e != nil {
			received <- -1
			return
		}
		defer conn.Close()
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 1)
		n, _ := conn.Read(buf)
		received <- n
	}()
	req := requestFor(TCP)
	host, port, _ := net.SplitHostPort(listener.Addr().String())
	req.TargetHost = host
	req.TargetPort, _ = strconv.Atoi(port)
	before := time.Now()
	got, err := newProber(t, Config{}).Run(context.Background(), req)
	if err != nil || got.Status != TargetReachable || got.Type != TCP || got.NodeID != req.NodeID || got.RuleID != req.RuleID || got.Generation != 7 || got.ObservedAt.Before(before) || got.ObservedAt.After(time.Now()) || got.LatencyMillis == nil || *got.LatencyMillis < 0 {
		t.Fatalf("TCP observation: %+v %v", got, err)
	}
	if n := <-received; n != 0 {
		t.Fatalf("TCP probe sent application data: %d", n)
	}
	listener.Close()
	got, err = newProber(t, Config{}).Run(context.Background(), req)
	if err != nil || got.Status != TargetUnreachable {
		t.Fatalf("closed target: %+v %v", got, err)
	}
}

func TestUDPNeverSendsPacketsOrClaimsHealth(t *testing.T) {
	socket, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	host, port, _ := net.SplitHostPort(socket.LocalAddr().String())
	req := requestFor(UDP)
	req.TargetHost = host
	req.TargetPort, _ = strconv.Atoi(port)
	got, err := newProber(t, Config{}).Run(context.Background(), req)
	if err != nil || got.Status != Unknown || got.LatencyMillis != nil {
		t.Fatalf("UDP observation: %+v %v", got, err)
	}
	socket.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	buf := make([]byte, 16)
	if n, _, err := socket.ReadFrom(buf); err == nil || n != 0 {
		t.Fatalf("unexpected UDP traffic: %d %v", n, err)
	}
}

func TestHTTPAllowlistStatusAndNoCredentialsOrProxy(t *testing.T) {
	var calls atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" || r.URL.Path != "/health" || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Proxy-Authorization") != "" {
			t.Error("unexpected method/path/credentials")
		}
		body, _ := io.ReadAll(r.Body)
		if len(body) != 0 {
			t.Error("unexpected request payload")
		}
		w.WriteHeader(204)
	}))
	defer endpoint.Close()
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("ALL_PROXY", "http://127.0.0.1:1")
	req := requestFor(HTTP)
	req.Endpoint = endpoint.URL + "/health"
	if _, err := newProber(t, Config{}).Run(context.Background(), req); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unapproved endpoint accepted: %v", err)
	}
	got, err := newProber(t, Config{AllowedEndpoints: []string{req.Endpoint}}).Run(context.Background(), req)
	if err != nil || got.Status != EndpointResponsive || got.LatencyMillis == nil || calls.Load() != 1 {
		t.Fatalf("approved endpoint: %+v %v calls=%d", got, err, calls.Load())
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "/health") || strings.Contains(string(raw), "127.0.0.1") || strings.Contains(string(raw), "healthy") {
		t.Fatalf("result leaks target/claims health: %s", raw)
	}
}

func TestHTTPRedirectNeverFollowsUnapprovedEndpoint(t *testing.T) {
	var follow atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { follow.Add(1); w.WriteHeader(204) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL+"/private", 302) }))
	defer redirect.Close()
	req := requestFor(HTTP)
	req.Endpoint = redirect.URL + "/health"
	got, err := newProber(t, Config{AllowedEndpoints: []string{req.Endpoint, target.URL + "/private"}}).Run(context.Background(), req)
	if err != nil || got.Status != EndpointUnresponsive || follow.Load() != 0 {
		t.Fatalf("redirect followed: %+v %v calls=%d", got, err, follow.Load())
	}
}

func TestHTTPBoundsBodyHeadersStatusAndTime(t *testing.T) {
	for _, mode := range []string{"body", "chunked", "headers", "status", "slow-headers", "slow-body", "compressed"} {
		t.Run(mode, func(t *testing.T) {
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "body":
					w.Header().Set("Content-Length", "5000")
					io.WriteString(w, strings.Repeat("x", 5000))
				case "chunked":
					w.(http.Flusher).Flush()
					io.WriteString(w, strings.Repeat("x", 5000))
				case "headers":
					w.Header().Set("X-Oversized", strings.Repeat("x", 12000))
					w.WriteHeader(204)
				case "status":
					w.WriteHeader(503)
				case "slow-headers":
					<-r.Context().Done()
				case "slow-body":
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
					<-r.Context().Done()
				case "compressed":
					w.Header().Set("Content-Encoding", "gzip")
					io.WriteString(w, "not decoded or collected")
				}
			}))
			defer endpoint.Close()
			req := requestFor(HTTP)
			req.Endpoint = endpoint.URL + "/health"
			req.Timeout = 150 * time.Millisecond
			started := time.Now()
			got, err := newProber(t, Config{AllowedEndpoints: []string{req.Endpoint}}).Run(context.Background(), req)
			if err != nil || got.Status != EndpointUnresponsive || time.Since(started) > 2*time.Second {
				t.Fatalf("bound %s: %+v %v elapsed=%v", mode, got, err, time.Since(started))
			}
		})
	}
}

func TestHTTPSVerifiesCertificatesAndSupportsExplicitTrustRoots(t *testing.T) {
	endpoint := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer endpoint.Close()
	req := requestFor(HTTP)
	req.Endpoint = endpoint.URL + "/health"
	got, err := newProber(t, Config{AllowedEndpoints: []string{req.Endpoint}}).Run(context.Background(), req)
	if err != nil || got.Status != EndpointUnresponsive {
		t.Fatalf("untrusted TLS accepted: %+v %v", got, err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(endpoint.Certificate())
	got, err = newProber(t, Config{AllowedEndpoints: []string{req.Endpoint}, RootCAs: roots}).Run(context.Background(), req)
	if err != nil || got.Status != EndpointResponsive {
		t.Fatalf("explicit trusted TLS: %+v %v", got, err)
	}
}

func TestInvalidConfigurationHasNoNetworkSideEffects(t *testing.T) {
	for _, url := range []string{"ftp://127.0.0.1/health", "http://user:pass@127.0.0.1/health", "http://127.0.0.1/health?token=secret", "http://127.0.0.1/health#secret", "http://127.0.0.1/health?", "http://127.0.0.1/../admin", "http://127.0.0.1/%2e%2e/admin", "http://127.0.0.1//admin", "http://127.0.0.1/health%2fadmin", "http://127.0.0.1/health\\admin", "http://0.0.0.0/health", "http://[::]/health", "http://127.0.0.1:0/health", "http://127.0.0.1:65536/health", "http://127.0.0.1/" + strings.Repeat("a", 300)} {
		if _, err := New(Config{AllowedEndpoints: []string{url}}); !errors.Is(err, ErrInvalid) {
			t.Errorf("unsafe URL accepted: %q %v", url, err)
		}
	}
	p := newProber(t, Config{})
	for _, mutate := range []func(*Request){func(r *Request) { r.NodeID = "" }, func(r *Request) { r.RuleID = "" }, func(r *Request) { r.Generation = 0 }, func(r *Request) { r.Type = "both" }, func(r *Request) { r.Timeout = -1 }, func(r *Request) { r.Timeout = time.Minute }, func(r *Request) { r.TargetHost = "host/path" }, func(r *Request) { r.TargetHost = "0.0.0.0" }, func(r *Request) { r.TargetPort = 0 }, func(r *Request) { r.TargetPort = 65536 }, func(r *Request) { r.Endpoint = "http://127.0.0.1/health" }} {
		req := requestFor(TCP)
		req.TargetHost = "127.0.0.1"
		req.TargetPort = 443
		mutate(&req)
		if _, err := p.Run(context.Background(), req); !errors.Is(err, ErrInvalid) {
			t.Errorf("invalid request accepted: %+v %v", req, err)
		}
	}
}

func TestCallerCancellationIsUnknownNotHealthFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := requestFor(TCP)
	req.TargetHost = "127.0.0.1"
	req.TargetPort = 443
	got, err := newProber(t, Config{}).Run(ctx, req)
	if !errors.Is(err, context.Canceled) || got.Status != Unknown {
		t.Fatalf("cancellation misreported: %+v %v", got, err)
	}
}

func TestEndpointBracketsRequireAnIPv6Literal(t *testing.T) {
	for _, endpoint := range []string{"http://[localhost]/health", "http://[127.0.0.1]/health"} {
		if _, err := New(Config{AllowedEndpoints: []string{endpoint}}); !errors.Is(err, ErrInvalid) {
			t.Errorf("non-IPv6 bracket authority accepted: %s %v", endpoint, err)
		}
	}
	if _, err := New(Config{AllowedEndpoints: []string{"http://[::1]:8080/health"}}); err != nil {
		t.Fatalf("valid IPv6 authority rejected: %v", err)
	}
}

func TestHTTPCancellationDuringResponseRemainsUnknown(t *testing.T) {
	entered := make(chan struct{})
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done() }))
	defer endpoint.Close()
	req := requestFor(HTTP)
	req.Endpoint = endpoint.URL + "/health"
	p := newProber(t, Config{AllowedEndpoints: []string{req.Endpoint}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type observation struct {
		result Result
		err    error
	}
	done := make(chan observation, 1)
	go func() { result, err := p.Run(ctx, req); done <- observation{result, err} }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("local endpoint not contacted")
	}
	cancel()
	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) || got.result.Status != Unknown || got.result.LatencyMillis != nil {
			t.Fatalf("canceled observation: %+v %v", got.result, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled probe did not stop")
	}
}

func TestHTTPBodyLimitAndConcurrentImmutableProber(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, strings.Repeat("x", 4096)) }))
	defer endpoint.Close()
	req := requestFor(HTTP)
	req.Endpoint = endpoint.URL + "/health"
	allowed := []string{req.Endpoint}
	p := newProber(t, Config{AllowedEndpoints: allowed})
	allowed[0] = "http://127.0.0.1:1/unapproved"
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := p.Run(context.Background(), req)
			if err != nil || got.Status != EndpointResponsive {
				t.Errorf("concurrent allowed/body boundary: %+v %v", got, err)
			}
		}()
	}
	wg.Wait()
}
