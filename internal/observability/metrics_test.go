package observability

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"reladock/internal/platform"
)

func fixture() platform.OperationalMetrics {
	return platform.OperationalMetrics{Nodes: map[string]int64{"ready": 2, "maintenance": 1}, AgentContacts: map[string]int64{"recent": 2, "stale": 1}, Rules: map[string]int64{"pending-agent": 3}, CurrentDeliveries: map[string]int64{"applied": 2, "failed": 1}, OpenIncidents: map[string]int64{"contact-stale": 1}, NotificationOutbox: map[string]int64{"pending": 1}, AgentUpgrades: map[string]int64{"succeeded": 2}, EndpointRetirements: map[string]int64{"released": 4}, FrozenWallets: 1}
}

func TestMetricsHandlerEmitsDeterministicLowCardinalityFacts(t *testing.T) {
	r := New("v0.9.0", func(context.Context) (platform.OperationalMetrics, error) { return fixture(), nil })
	if err := r.ObserveWorker(WorkerIncidentReconcile, nil); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "http://metrics.local/metrics", nil)
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("status/header: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	body := rec.Body.String()
	for _, want := range []string{`reladock_database_up 1`, `reladock_nodes{status="ready"} 2`, `reladock_agent_contacts{state="stale"} 1`, `reladock_worker_runs_total{worker="incident-reconcile",result="success"} 1`} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in %s", want, body)
		}
	}
	for _, forbidden := range []string{"tenant", "node_id", "rule_id", "target", "certificate", "secret@example.com"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("high-cardinality/secret field %q leaked", forbidden)
		}
	}
}

func TestMetricsDatabaseFailureIsParseableAndNotCached(t *testing.T) {
	var calls atomic.Int64
	r := New("v0.9.0", func(context.Context) (platform.OperationalMetrics, error) {
		if calls.Add(1) == 1 {
			return platform.OperationalMetrics{}, errors.New("database secret")
		}
		return fixture(), nil
	})
	for i, want := range []string{"reladock_database_up 0", "reladock_database_up 1"} {
		rec := httptest.NewRecorder()
		r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("scrape %d: %d %s", i, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "database secret") {
			t.Fatal("database error leaked")
		}
		if i == 0 && !strings.Contains(rec.Body.String(), "reladock_metrics_scrape_errors_total 1") {
			t.Fatalf("current scrape failure not counted: %s", rec.Body.String())
		}
	}
}

func TestMetricsHTTPContractAvoidsDatabaseWork(t *testing.T) {
	var calls atomic.Int64
	r := New("dev", func(context.Context) (platform.OperationalMetrics, error) { calls.Add(1); return fixture(), nil })
	h := r.Handler()
	for _, tc := range []struct {
		method, path string
		status       int
	}{{http.MethodHead, "/metrics", 200}, {http.MethodPost, "/metrics", 405}, {http.MethodGet, "/other", 404}} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != tc.status {
			t.Fatalf("%s %s=%d", tc.method, tc.path, rec.Code)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("unexpected snapshot calls: %d", calls.Load())
	}
}

func TestWorkerCountersAreConcurrentAndRejectUnknown(t *testing.T) {
	r := New("dev", func(context.Context) (platform.OperationalMetrics, error) { return fixture(), nil })
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var err error
			if i%2 == 0 {
				err = errors.New("failed")
			}
			if observeErr := r.ObserveWorker(WorkerNotificationDelivery, err); observeErr != nil {
				t.Error(observeErr)
			}
		}(i)
	}
	wg.Wait()
	if err := r.ObserveWorker(Worker("invented"), nil); err == nil {
		t.Fatal("accepted unknown worker label")
	}
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `worker="notification-delivery",result="success"} 50`) || !strings.Contains(body, `worker="notification-delivery",result="error"} 50`) {
		t.Fatalf("counters: %s", body)
	}
}

func TestMetricsAllowsOnlyOneDatabaseScrapeAtATime(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int64
	r := New("dev", func(context.Context) (platform.OperationalMetrics, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return fixture(), nil
	})
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		r.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/metrics", nil))
	}()
	<-entered
	second := httptest.NewRecorder()
	r.Handler().ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if second.Code != http.StatusServiceUnavailable {
		t.Fatalf("concurrent scrape status=%d", second.Code)
	}
	if calls.Load() != 1 {
		t.Fatalf("concurrent database calls=%d", calls.Load())
	}
	close(release)
	<-firstDone
}
