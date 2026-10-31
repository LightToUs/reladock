# Operational Metrics Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a loopback-only Prometheus endpoint for truthful, low-cardinality RelaDock operational facts.

**Architecture:** `platform.Store` produces one bounded database snapshot, `internal/observability` owns process counters and deterministic text encoding, and `cmd/reladock` runs a separate optional listener. The endpoint never shares browser routes or labels customer resources.

**Tech Stack:** Go 1.24 standard library, pgx/PostgreSQL, Prometheus text exposition 0.0.4, existing runtime configuration and HTTP lifecycle.

**Spec:** `docs/superpowers/specs/2026-10-31-operational-metrics-design.md`

## Global Constraints

- `METRICS_LISTEN_ADDRESS` is optional, loopback-literal only, and distinct from control/Agent addresses.
- No new dependency and no high-cardinality or secret labels.
- Contact, configuration, target reachability and end-to-end health remain separate facts.
- Payment behavior is unchanged.

## Review Focus

- Unknown database states must fail closed rather than create arbitrary labels; Task 1 tests each family.
- Database outage must still yield parseable metrics with `reladock_database_up 0`; Task 2 tests it.
- Concurrent worker updates and scrapes must be race-free; Task 2 runs concurrent counter tests.
- Public/conflicting listener configuration must fail before startup; Task 3 tests runtime configuration.
- HEAD, wrong methods and wrong paths must not trigger unexpected database work; Task 2 tests HTTP behavior.

---

### Task 1: Database operational snapshot

**Files:** Create `internal/platform/metrics.go`; create `internal/platform/metrics_test.go`.

**Interfaces:** Produces `Store.OperationalMetrics(context.Context, time.Duration) (OperationalMetrics, error)` with fixed count maps.

- [ ] Write real-PostgreSQL tests for all count families, current desired delivery only, five-minute contact buckets and unknown-state rejection.
- [ ] Run the focused test and confirm it fails because the interface is absent.
- [ ] Implement one read-only transaction with fixed allowlists and bounded aggregate queries.
- [ ] Run the focused tests and commit `feat: expose operational metric snapshot`.

### Task 2: Prometheus encoder and worker counters

**Files:** Create `internal/observability/metrics.go`; create `internal/observability/metrics_test.go`.

**Interfaces:** Consumes Task 1 snapshot callback; produces `Registry.ObserveWorker`, `Registry.Handler`, and deterministic text output.

- [ ] Write failing tests for HTTP contract, database failure, deterministic fixed labels, absence of customer identifiers, and concurrent counters.
- [ ] Implement fixed-family encoding, a three-second scrape context, database-down behavior and atomic process counters.
- [ ] Run package tests with `-race` and commit `feat: serve truthful operational metrics`.

### Task 3: Runtime listener integration

**Files:** Modify `internal/runtimeconfig/config.go`; modify `internal/runtimeconfig/config_test.go`; modify `cmd/reladock/main.go`; modify `cmd/reladock/notifications.go`; add/update command tests as needed; modify `deploy/control.env.example`.

**Interfaces:** Consumes Task 2 handler and records fixed worker outcomes.

- [ ] Write failing configuration tests for disabled, public, malformed and conflicting metrics addresses.
- [ ] Add validated `MetricsAddress`, start the listener only when configured, include it in failure/shutdown handling, and instrument existing workers without changing their business behavior.
- [ ] Run runtime/command tests and commit `feat: integrate loopback metrics listener`.

### Task 4: Operator documentation and full verification

**Files:** Modify `docs/OPERATIONS.md`, `docs/PROBES.md`, `docs/DEPLOYMENT.md`, `docs/ROADMAP.md`, `README.md` and release truthfulness if needed.

- [ ] Document metric meanings, scrape setup, conservative alerts and non-health boundaries; correct stale probe integration claims.
- [ ] Run `npm run vet`, `npm test`, `npm run build:release` and `git diff --check`.
- [ ] Review label cardinality, query failure, worker races and shutdown; add regressions for accepted findings.
- [ ] Commit `docs: document operational metrics`.
