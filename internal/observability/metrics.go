package observability

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"reladock/internal/platform"
)

type Worker string

const (
	WorkerSubscriptionReconcile Worker = "subscription-reconcile"
	WorkerIncidentReconcile     Worker = "incident-reconcile"
	WorkerNotificationDelivery  Worker = "notification-delivery"
)

var workers = []Worker{WorkerSubscriptionReconcile, WorkerIncidentReconcile, WorkerNotificationDelivery}

type SnapshotFunc func(context.Context) (platform.OperationalMetrics, error)
type workerCounts struct {
	success atomic.Uint64
	failure atomic.Uint64
}
type Registry struct {
	version      string
	snapshot     SnapshotFunc
	worker       [3]workerCounts
	scrapeErrors atomic.Uint64
	scrapeGate   chan struct{}
}

func New(version string, snapshot SnapshotFunc) *Registry {
	return &Registry{version: version, snapshot: snapshot, scrapeGate: make(chan struct{}, 1)}
}
func workerIndex(worker Worker) (int, bool) {
	for i, v := range workers {
		if worker == v {
			return i, true
		}
	}
	return 0, false
}
func (r *Registry) ObserveWorker(worker Worker, runErr error) error {
	i, ok := workerIndex(worker)
	if !ok {
		return errors.New("unknown metrics worker")
	}
	if runErr == nil {
		r.worker[i].success.Add(1)
	} else {
		r.worker[i].failure.Add(1)
	}
	return nil
}

func label(value string) string { return strconv.Quote(value) }
func family(b *strings.Builder, name, help, labelName string, values map[string]int64) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(b, "%s{%s=%s} %d\n", name, labelName, label(key), values[key])
	}
}
func (r *Registry) processMetrics(b *strings.Builder) {
	fmt.Fprintln(b, "# HELP reladock_build_info RelaDock build identity.")
	fmt.Fprintln(b, "# TYPE reladock_build_info gauge")
	fmt.Fprintf(b, "reladock_build_info{version=%s} 1\n", label(r.version))
	fmt.Fprintln(b, "# HELP reladock_worker_runs_total Completed control worker runs.")
	fmt.Fprintln(b, "# TYPE reladock_worker_runs_total counter")
	for i, w := range workers {
		fmt.Fprintf(b, "reladock_worker_runs_total{worker=%s,result=\"error\"} %d\n", label(string(w)), r.worker[i].failure.Load())
		fmt.Fprintf(b, "reladock_worker_runs_total{worker=%s,result=\"success\"} %d\n", label(string(w)), r.worker[i].success.Load())
	}
	fmt.Fprintln(b, "# HELP reladock_metrics_scrape_errors_total Metric snapshot failures.")
	fmt.Fprintln(b, "# TYPE reladock_metrics_scrape_errors_total counter")
	fmt.Fprintf(b, "reladock_metrics_scrape_errors_total %d\n", r.scrapeErrors.Load())
}
func (r *Registry) encode(ctx context.Context) string {
	var b strings.Builder
	snapshot, err := r.snapshot(ctx)
	if err != nil {
		r.scrapeErrors.Add(1)
	}
	r.processMetrics(&b)
	fmt.Fprintln(&b, "# HELP reladock_database_up Whether the current metric snapshot reached PostgreSQL.")
	fmt.Fprintln(&b, "# TYPE reladock_database_up gauge")
	if err != nil {
		fmt.Fprintln(&b, "reladock_database_up 0")
		return b.String()
	}
	fmt.Fprintln(&b, "reladock_database_up 1")
	family(&b, "reladock_nodes", "Nodes by lifecycle state.", "status", snapshot.Nodes)
	family(&b, "reladock_agent_contacts", "Nodes by authenticated control contact recency.", "state", snapshot.AgentContacts)
	family(&b, "reladock_rules", "Relay rules by persisted state.", "status", snapshot.Rules)
	family(&b, "reladock_current_config_deliveries", "Current desired-generation deliveries by state.", "state", snapshot.CurrentDeliveries)
	family(&b, "reladock_open_incidents", "Open control incidents by kind.", "kind", snapshot.OpenIncidents)
	family(&b, "reladock_notification_outbox", "Durable notification events by state.", "state", snapshot.NotificationOutbox)
	family(&b, "reladock_agent_upgrades", "Agent upgrade tasks by state.", "state", snapshot.AgentUpgrades)
	family(&b, "reladock_endpoint_retirements", "Endpoint retirement jobs by state.", "state", snapshot.EndpointRetirements)
	fmt.Fprintln(&b, "# HELP reladock_frozen_wallets Wallets with spending disabled.")
	fmt.Fprintln(&b, "# TYPE reladock_frozen_wallets gauge")
	fmt.Fprintf(&b, "reladock_frozen_wallets %d\n", snapshot.FrozenWallets)
	return b.String()
}

func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		if request.URL.Path != "/metrics" {
			http.NotFound(w, request)
			return
		}
		if request.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		if request.Method != http.MethodGet {
			w.Header().Set("Allow", "GET, HEAD")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		select {
		case r.scrapeGate <- struct{}{}:
			defer func() { <-r.scrapeGate }()
		default:
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(request.Context(), 3*time.Second)
		defer cancel()
		_, _ = w.Write([]byte(r.encode(ctx)))
	})
}
