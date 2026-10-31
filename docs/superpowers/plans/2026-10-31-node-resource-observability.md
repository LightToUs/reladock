# Node Resource Observability Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add bounded Agent resource telemetry, sustained resource/drift incidents, admin visibility and aggregate metrics without claiming end-to-end health.

**Architecture:** Extend the authenticated heartbeat with a fixed observation collected from Linux kernel files and the existing read-only dataplane verifier. Store one latest row per node, maintain condition start timestamps transactionally, reuse incident reconciliation, and expose only bounded admin facts and aggregate metrics.

**Tech Stack:** Go 1.24+, PostgreSQL, existing vanilla JavaScript admin UI, Prometheus text exposition.

**Spec:** `docs/superpowers/specs/2026-10-31-node-resource-observability-design.md`

## Global Constraints

- Resource observations never imply end-to-end forwarding health.
- No shell execution, raw diagnostics or high-cardinality metric labels.
- Unknown collection values must not be converted into healthy values.
- Re-enrollment, revocation and retirement remove previous-identity observations.
- Payment behavior is unchanged.

## Review Focus

- Malformed or future/stale samples must roll back both heartbeat and observation writes.
- A single unhealthy sample must not open an incident.
- Unknown or stale telemetry must not remain presented as current pressure or sync.
- Identity replacement must not retain a previous Agent's resource facts.
- Dataplane verification errors must never be rendered as `in-sync`.

---

### Task 1: Bounded Agent collector

**Files:**
- Create: `internal/agent/resources.go`
- Test: `internal/agent/resources_test.go`
- Modify: `internal/agent/engine.go`

**Interfaces:**
- Produces: `ResourceCollector.Collect(context.Context, DataplaneInspector) platform.ResourceObservation`.
- Produces: `NodeEngine.DataplaneState(context.Context) string`.

- [ ] Write failing tests for CPU deltas, memory, disk, conntrack, unavailable values and verifier classification.
- [ ] Run focused tests and confirm missing implementation failures.
- [ ] Implement direct bounded collection and read-only drift classification.
- [ ] Run focused tests and commit.

### Task 2: Persist authenticated latest observations

**Files:**
- Create: `internal/platform/migration24.sql`
- Modify: `internal/platform/store.go`
- Modify: `internal/platform/heartbeat.go`
- Modify: `internal/platform/admin_nodes.go`
- Modify: `internal/platform/node_retirement.go`
- Test: `internal/platform/heartbeat_test.go`
- Test: `internal/platform/admin_test.go`
- Test: `internal/platform/node_retirement_test.go`

**Interfaces:**
- Consumes: `platform.ResourceObservation` from Task 1.
- Produces: latest observation fields on `ManagedNode` and server-maintained condition timestamps.

- [ ] Write failing PostgreSQL tests for validation, latest-row replacement, condition continuity and identity reset.
- [ ] Run focused tests and confirm schema/API failures.
- [ ] Add migration 24 and transactional heartbeat persistence.
- [ ] Add bounded admin projection and identity cleanup.
- [ ] Run focused tests and commit.

### Task 3: Wire the Agent heartbeat and HTTP contract

**Files:**
- Modify: `cmd/reladock-agent/main.go`
- Modify: `internal/agent/heartbeat_test.go`
- Modify: `internal/web/agent_test.go`

**Interfaces:**
- Consumes: `ResourceCollector` and persisted heartbeat contract.
- Produces: one observation on each normal heartbeat.

- [ ] Write failing client and authenticated HTTP tests for the observation payload.
- [ ] Run focused tests and confirm failures.
- [ ] Construct one collector for the process and attach each sample to heartbeat.
- [ ] Run focused tests and commit.

### Task 4: Sustained resource and drift incidents

**Files:**
- Modify: `internal/platform/migration24.sql`
- Modify: `internal/platform/operations.go`
- Test: `internal/platform/operations_test.go`
- Modify: `public/operations-tools.js`

**Interfaces:**
- Consumes: condition-since timestamps from Task 2.
- Produces: six fixed incident kinds through the existing audit/notification path.

- [ ] Write failing tests for duration gates, stale samples, maintenance nodes, recovery and idempotency.
- [ ] Run focused tests and confirm failures.
- [ ] Extend the incident constraint and reconciliation conditions.
- [ ] Add human-readable fixed labels in the UI.
- [ ] Run focused tests and commit.

### Task 5: Admin and aggregate observability

**Files:**
- Modify: `internal/platform/metrics.go`
- Test: `internal/platform/metrics_test.go`
- Modify: `internal/observability/metrics.go`
- Test: `internal/observability/metrics_test.go`
- Modify: `public/admin.js`
- Modify: `public/admin.html`
- Modify: `public/node-contact-status.js`
- Test: `test/node-contact-status.test.js`

**Interfaces:**
- Consumes: latest resource rows and incident kinds.
- Produces: fixed node-resource status counts and truthful admin presentation.

- [ ] Write failing Go and JavaScript tests for aggregate labels and presentation states.
- [ ] Run focused tests and confirm failures.
- [ ] Add fixed aggregate metrics and resource presentation.
- [ ] Run focused tests and commit.

### Task 6: Documentation and full verification

**Files:**
- Modify: `README.md`
- Modify: `docs/ARCHITECTURE.md`
- Modify: `docs/REQUIREMENTS.md`
- Modify: `docs/ROADMAP.md`
- Modify: `docs/OPERATIONS.md`
- Modify: `docs/acceptance/LINK_AND_CAPACITY.md`

**Interfaces:**
- Consumes: completed behavior from Tasks 1-5.
- Produces: truthful operator and acceptance documentation.

- [ ] Document thresholds, unknown/stale semantics, non-goals and real-host acceptance gaps.
- [ ] Run `npm run vet`, `npm test`, `npm run build:release` and `git diff --check`.
- [ ] Perform a whole-branch review and fix important findings with a failing test first.
- [ ] Commit documentation and verification fixes.
