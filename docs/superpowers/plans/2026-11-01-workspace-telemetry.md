# Workspace Telemetry Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add truthful per-rule traffic, recent bandwidth utilization, traffic history and associated-node resource telemetry to the enterprise relay workspace.

**Architecture:** Reuse immutable usage reports and latest node resource observations without changing the Agent or data plane. PostgreSQL store methods enforce tenant scope and derive bounded display telemetry; authenticated read-only HTTP endpoints expose it; a small ES module formats and renders the workspace data.

**Tech Stack:** Go 1.24, PostgreSQL, static HTML/CSS/ES modules, Node.js test runner.

**Spec:** `docs/superpowers/specs/2026-11-01-workspace-telemetry-design.md`

## Global Constraints

- Traffic and bandwidth remain independent per relay rule; no tenant-wide quota or bandwidth pool.
- Missing or stale telemetry remains unknown and must not be rendered as zero or healthy.
- Agent contact, configuration applied, resource normal, target reachable and end-to-end health remain separate facts.
- Existing usage-report idempotency, immutable history and billing-period attribution cannot change.
- Enterprise reads remain tenant-scoped and may expose resources only for nodes referenced by that tenant's undeleted rules.

## Review Focus

- Two latest reports from different epochs must not produce a rate.
- A latest report older than 30 seconds must return stale rate state without numeric utilization.
- Cross-tenant rule history and unassociated node resources must remain hidden.
- Counter deltas and rate arithmetic near integer limits must not overflow or become negative.
- Sparse history buckets must remain sparse; missing reports must not be filled as zero traffic.

---

### Task 1: Per-rule telemetry store

**Files:**
- Modify: `internal/platform/billing.go`
- Modify: `internal/platform/billing_test.go`

**Interfaces:**
- Produces: `RuleTelemetry`, `UsagePoint`, `RuleTelemetrySeries`, `Store.RuleTelemetry(ctx, who)`, and `Store.RuleTelemetryHistory(ctx, who, ruleID, window)`.

- [ ] Write integration tests covering tenant scope, cumulative totals, fresh same-epoch rate, stale/insufficient/different-epoch rate states, overflow-safe utilization, and sparse 1h/24h/7d buckets.
- [ ] Run the focused tests and confirm they fail because the telemetry interfaces do not exist.
- [ ] Implement the bounded summary and history queries without schema changes.
- [ ] Run the focused tests and the complete `internal/platform` package tests.
- [ ] Commit the store change.

### Task 2: Authenticated telemetry HTTP API

**Files:**
- Modify: `internal/web/billing.go`
- Modify: `internal/web/billing_http_test.go`

**Interfaces:**
- Consumes: Task 1 telemetry store methods.
- Produces: `GET /api/billing/telemetry` and the strict `ruleId` + `window` form.

- [ ] Write HTTP tests for summary/history responses, strict query pairing, method rejection, anonymous denial and cross-tenant 404.
- [ ] Run the focused tests and confirm the new route fails.
- [ ] Implement the read-only route while preserving `/api/billing/usage` compatibility.
- [ ] Run focused HTTP tests.
- [ ] Commit the API change.

### Task 3: Tenant-scoped node resources

**Files:**
- Modify: `internal/platform/operations.go`
- Modify: `internal/platform/operations_test.go`
- Modify: `internal/web/operations_http_test.go`

**Interfaces:**
- Produces: resource status, observation time, CPU, memory, disk, conntrack, clock skew and dataplane state on `OperationNode`.

- [ ] Write tests proving fresh/stale resource presentation and hiding nodes not referenced by the tenant.
- [ ] Run focused tests and confirm missing resource fields fail.
- [ ] Extend the existing tenant-scoped operation-node query using `node_resource_latest`.
- [ ] Run focused platform and HTTP tests.
- [ ] Commit the resource API change.

### Task 4: Workspace telemetry presentation

**Files:**
- Create: `public/telemetry.js`
- Create: `test/telemetry.test.mjs`
- Modify: `public/index.html`
- Modify: `public/app.js`
- Modify: `public/app.css`
- Modify: `public/assets.go`
- Modify: `public/assets_test.go`
- Modify: `internal/web/server.go`
- Modify: `internal/web/static_assets_test.go`

**Interfaces:**
- Consumes: Tasks 2 and 3 JSON fields.
- Produces: automatic rule traffic/rate cells, selectable traffic trend, utilization cards, and associated-node resource cards.

- [ ] Write Node tests for byte/rate formatting, stale/insufficient presentation, utilization labels and sparse trend scaling; add static asset integration expectations.
- [ ] Run focused Node and Go tests and confirm failures from the absent module/UI.
- [ ] Implement the ES module and wire authenticated refresh/history selection.
- [ ] Add responsive dashboard styles with explicit freshness/unknown copy.
- [ ] Run focused Node/static asset tests and inspect the page at desktop and mobile widths.
- [ ] Commit the presentation change.

### Task 5: Documentation and full verification

**Files:**
- Modify: `README.md`
- Modify: `docs/REQUIREMENTS.md`
- Modify: `docs/ARCHITECTURE.md`
- Modify: `docs/ROADMAP.md`
- Modify: `docs/IMPLEMENTATION_STATUS.md`

**Interfaces:**
- Consumes: completed Tasks 1–4.
- Produces: accurate implementation status and limits.

- [ ] Document the new display, receive-time rate/history limitation and absence of long-term node-resource storage.
- [ ] Run `npm test` against the isolated PostgreSQL test schemas.
- [ ] Run `npm run vet`.
- [ ] Review the branch against the spec, fix Critical/Important findings with failing tests first, and record unverified environment work.
- [ ] Commit documentation and verification updates.

