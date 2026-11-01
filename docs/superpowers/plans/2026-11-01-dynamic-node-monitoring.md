# Dynamic Node Monitoring Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add tenant-safe dynamic node traffic/resource monitoring to both control consoles and unify both consoles around a blue visual theme.

**Architecture:** Extend the existing tenant/admin operations-node query with scoped per-node traffic aggregates and Agent start time; no migration or Agent change is required. A focused browser module owns truthful monitoring presentation and a non-overlapping adaptive poller, while each console renders the same data in its existing information architecture.

**Tech Stack:** Go 1.24, PostgreSQL 17, static HTML/CSS/ES modules, Node.js 20 test runner, systemd Linux release workflow.

**Spec:** `docs/superpowers/specs/2026-11-01-dynamic-node-monitoring-design.md`

## Global Constraints

- Enterprise node traffic must include only the current tenant's undeleted rules; platform administrators may see all-tenant node aggregates.
- Unknown, insufficient and stale data must never be formatted as zero or health.
- Recent rates use receive-time deltas from the same epoch with a 1–60 second interval and a latest report age no greater than 30 seconds.
- Resource samples older than 90 seconds are stale; Agent contact, data-plane consistency and end-to-end health remain separate facts.
- Use recursive timeouts with 5-second visible and 30-second hidden intervals; requests must not overlap.
- Do not add WebSockets, a frontend framework, resource history storage, a database migration or Agent/data-plane changes.
- Apply the blue theme only to `/` and `/admin`; do not modify the public marketing site.

## Review Focus

- A platform node shared by two tenants must expose only the caller tenant's bytes/rates on `/api/operations/nodes`, while `/api/admin/operations/nodes` may aggregate both.
- Mixed fresh/stale rule reports must sum only valid fresh rates and retain truthful aggregate state rather than treating stale traffic as zero.
- Hidden/visible transitions and slow requests must never create overlapping poll requests or update after polling is stopped.
- A failed refresh must preserve the last successful node values and visibly mark the refresh failure or stale timestamp.
- Auto-refresh must not rebuild administrator management controls, close dialogs or lose in-progress form state.

---

### Task 1: Tenant-safe per-node traffic aggregation

**Files:**
- Modify: `internal/platform/operations.go`
- Modify: `internal/platform/operations_test.go`
- Modify: `internal/web/operations_http_test.go`

**Interfaces:**
- Produces: `OperationNode.AgentStartedAt *time.Time` and embedded `NodeTraffic` JSON fields for totals, report time, rate state and optional upload/download/total bytes per second.
- Produces: `Store.ListOperationsNodes(ctx, who, admin, staleAfter, page)` with traffic scoped by the existing `admin` and `who.TenantID` authorization boundary.

- [ ] **Step 1: Write the failing platform tests**

Add `TestListOperationsNodesAggregatesTrafficByAuthorizationScope` with two tenants sharing one node. Assert tenant A sees only A totals/rates, tenant B sees only B totals/rates, and a platform administrator sees their sum. Include fresh, stale, single-sample and no-report nodes so every aggregate state is pinned.

- [ ] **Step 2: Run the focused platform test and verify it fails**

Run: `npm run db:start` then `go test ./internal/platform -run TestListOperationsNodesAggregatesTrafficByAuthorizationScope -count=1`

Expected: FAIL because `OperationNode` does not expose node traffic fields.

- [ ] **Step 3: Implement the scoped aggregate**

Add a `NodeTraffic` value type in `internal/platform/operations.go`. Query undeleted rules through the current operations transaction, reuse `applyTelemetryRate` for each rule, aggregate totals by `node_id`, sum only fresh rule rates, and derive `fresh`, `stale`, `insufficient`, or no-report state without inventing values. Add `n.agent_started_at` to the existing node query and merge traffic by node before returning.

- [ ] **Step 4: Run the focused platform test and verify it passes**

Run: `go test ./internal/platform -run 'TestListOperationsNodes(AggregatesTrafficByAuthorizationScope|ScopesTenantResources)' -count=1`

Expected: PASS.

- [ ] **Step 5: Write and run the HTTP authorization assertions**

Extend `TestOperationsHTTPEnforcesScopeAndSupportsTickets` (or add a focused peer test) to assert the tenant and admin endpoints serialize the new fields with the same scope and reject unauthenticated/cross-scope access.

Run: `go test ./internal/web -run 'TestOperationsHTTP.*Node' -count=1`

Expected: PASS.

- [ ] **Step 6: Commit the backend slice**

```bash
git add internal/platform/operations.go internal/platform/operations_test.go internal/web/operations_http_test.go
git commit -m "feat: aggregate scoped node traffic telemetry"
```

### Task 2: Monitoring presentation and adaptive polling module

**Files:**
- Create: `public/node-monitoring.js`
- Create: `test/node-monitoring.test.mjs`
- Modify: `internal/web/server.go`
- Modify: `internal/web/static_assets_test.go`

**Interfaces:**
- Consumes: operation-node JSON from Task 1.
- Produces: `nodeMonitoringPresentation(node, now)` for display-safe traffic, resource, contact and Agent-runtime values.
- Produces: `createAdaptivePoller({ run, documentTarget, activeMs, hiddenMs, setTimer, clearTimer })` returning `{ start, stop, refresh }`.

- [ ] **Step 1: Write failing presentation tests**

Assert fresh traffic formats upload/download rate and cumulative bytes, insufficient/stale/no-report states remain explicit, resource nulls remain unknown, samples beyond 90 seconds are stale, and Agent runtime is labeled independently from VPS uptime.

- [ ] **Step 2: Write failing poller tests**

Using injected fake timers and a fake document, assert 5-second visible cadence, 30-second hidden cadence, immediate refresh on visibility restoration, no overlapping `run()` promises, stop cancellation, and recovery after a rejected refresh while preserving caller-owned data.

- [ ] **Step 3: Run the Node test and verify it fails**

Run: `node --test test/node-monitoring.test.mjs`

Expected: FAIL because the module does not exist.

- [ ] **Step 4: Implement the minimal module**

Reuse `formatBytes`/`formatRate` from `public/telemetry.js`. Use recursive `setTimeout` scheduled only after `run()` settles; visibility changes cancel the pending timer and choose the new interval. Do not mutate page DOM in this module.

- [ ] **Step 5: Register and verify the new static asset**

Add `/node-monitoring.js` to the static allowlist/content type path in `internal/web/server.go` and assertions in `internal/web/static_assets_test.go`.

Run: `node --test test/node-monitoring.test.mjs && go test ./internal/web -run TestStatic -count=1`

Expected: PASS.

- [ ] **Step 6: Commit the shared frontend slice**

```bash
git add public/node-monitoring.js test/node-monitoring.test.mjs internal/web/server.go internal/web/static_assets_test.go
git commit -m "feat: add adaptive node monitoring client"
```

### Task 3: Enterprise node-status menu and live panel

**Files:**
- Modify: `public/index.html`
- Modify: `public/app.js`
- Modify: `public/app.css`
- Modify: `internal/web/static_assets_test.go`
- Modify: `test/node-monitoring.test.mjs`

**Interfaces:**
- Consumes: `/api/operations/nodes?staleAfterSeconds=90`, `nodeMonitoringPresentation`, and `createAdaptivePoller`.
- Produces: `renderEnterpriseNodeMonitoring(items, options)` behavior bound to `#node-status-rows` and `#node-status-message`.

- [ ] **Step 1: Add failing static and DOM-model tests**

Assert the navigation contains “节点状态”, the section IDs exist, only operation-node items are rendered, failure keeps prior rows, and every row has contact, upload/download rate, cumulative traffic, Agent runtime, CPU/RAM/disk/conntrack and observed-time targets.

- [ ] **Step 2: Run the focused tests and verify they fail**

Run: `node --test test/node-monitoring.test.mjs && go test ./internal/web -run TestEnterpriseWorkspaceExposesTrafficAndNodeResourceTelemetry -count=1`

Expected: FAIL because the menu and live panel do not exist.

- [ ] **Step 3: Add the enterprise section and renderer**

Insert a “节点状态” navigation item and independent `#node-status-section` after rule management. Render only the server-returned tenant-scoped nodes. Use compact progress bars for CPU/RAM/disk/conntrack, separate upload/download values, explicit state copy, and an `aria-live` refresh message.

- [ ] **Step 4: Bind adaptive refresh without full workspace reload**

Start one poller after authenticated workspace entry. Each run requests only `/api/operations/nodes?staleAfterSeconds=90`, updates monitoring cells in place, and on failure preserves existing rows plus an error/time marker. Stop or return to existing authentication handling on HTTP 401.

- [ ] **Step 5: Verify enterprise behavior**

Run: `node --test test/node-monitoring.test.mjs && go test ./internal/web -run 'TestEnterpriseWorkspaceExposesTrafficAndNodeResourceTelemetry|TestStatic' -count=1`

Expected: PASS.

- [ ] **Step 6: Commit the enterprise slice**

```bash
git add public/index.html public/app.js public/app.css internal/web/static_assets_test.go test/node-monitoring.test.mjs
git commit -m "feat: add live enterprise node status"
```

### Task 4: Administrator node-list monitoring

**Files:**
- Modify: `public/admin.html`
- Modify: `public/admin.js`
- Modify: `public/admin.css`
- Modify: `internal/web/static_assets_test.go`
- Modify: `test/node-monitoring.test.mjs`

**Interfaces:**
- Consumes: `/api/admin/operations/nodes?staleAfterSeconds=90`, `nodeMonitoringPresentation`, and `createAdaptivePoller`.
- Produces: monitoring cells keyed by safe node ID without reconstructing existing management/control cells.

- [ ] **Step 1: Add failing admin markup and update tests**

Assert “全部入口节点” has appended live traffic and resource columns, update targets are keyed by node ID, and applying a monitoring refresh changes only those targets while an open dialog and its form values remain untouched.

- [ ] **Step 2: Run the focused tests and verify they fail**

Run: `node --test test/node-monitoring.test.mjs && go test ./internal/web -run TestAdmin -count=1`

Expected: FAIL because administrator monitoring targets are absent.

- [ ] **Step 3: Append monitoring cells to each administrator node row**

Keep all existing lifecycle, certificate, delivery and management cells. Add compact upload/download rate, cumulative traffic, CPU/RAM/disk/conntrack, Agent runtime and sample-time targets. Initial reload fetches monitoring once; later refreshes patch only the monitoring cell contents.

- [ ] **Step 4: Bind administrator adaptive refresh**

Start after platform authorization succeeds, poll only the admin operations-node endpoint, preserve the last successful values on failure, and never invoke the full `reload()` from the timer.

- [ ] **Step 5: Verify administrator behavior**

Run: `node --test test/node-monitoring.test.mjs && go test ./internal/web -run 'TestAdmin|TestStatic' -count=1`

Expected: PASS.

- [ ] **Step 6: Commit the administrator slice**

```bash
git add public/admin.html public/admin.js public/admin.css internal/web/static_assets_test.go test/node-monitoring.test.mjs
git commit -m "feat: add live admin node telemetry"
```

### Task 5: Unified blue console theme and responsive visual verification

**Files:**
- Modify: `public/app.css`
- Modify: `public/admin.css`
- Modify: `public/index.html`
- Modify: `public/admin.html`
- Modify: `internal/web/static_assets_test.go`

**Interfaces:**
- Produces: blue console design tokens and responsive monitoring layout; no API contract changes.

- [ ] **Step 1: Add failing palette/static assertions**

Assert both console roots declare blue theme colors, old green/lime primary tokens are absent from primary controls, alert colors remain distinct, and public `website/` files are untouched.

- [ ] **Step 2: Run the focused static test and verify it fails**

Run: `go test ./internal/web -run 'Test.*(Theme|Static)' -count=1`

Expected: FAIL against the current green themes.

- [ ] **Step 3: Apply the blue palettes**

Use a navy/bright-blue dark palette for the enterprise console and a pale-blue/deep-navy palette for the administrator console. Replace hard-coded green primary states, update browser `theme-color`, preserve red/orange alert semantics and green online semantics, and keep contrast/focus indicators accessible.

- [ ] **Step 4: Add responsive monitoring styles**

Desktop uses compact table/progress presentation. At 860/620px breakpoints, keep the page body free of horizontal overflow, allow local table scrolling, and maintain readable labels and tap targets. Respect `prefers-reduced-motion`.

- [ ] **Step 5: Run local visual checks**

Run the local service, inspect `/` and `/admin` at desktop and 500px widths, confirm no page-level horizontal overflow, no clipped monitoring values, dialogs remain usable, and dynamic refresh does not cause layout jumps. Save screenshots only as temporary verification artifacts.

- [ ] **Step 6: Run static tests and commit the visual slice**

Run: `go test ./internal/web -run 'Test.*(Theme|Static)' -count=1`

Expected: PASS.

```bash
git add public/app.css public/admin.css public/index.html public/admin.html internal/web/static_assets_test.go
git commit -m "style: unify consoles with blue monitoring theme"
```

### Task 6: Full verification, documentation, release and production deployment

**Files:**
- Modify: `package.json`
- Modify: `README.md`
- Modify: `docs/ROADMAP.md`
- Modify: `docs/IMPLEMENTATION_STATUS.md`

**Interfaces:**
- Consumes: all prior tasks.
- Produces: a traceable control-only release and production verification record.

- [ ] **Step 1: Update product documentation and release version**

Document node-scoped dynamic monitoring, tenant aggregation boundaries, polling semantics and blue theme. Bump the control release from `0.9.7` to `0.9.8`; do not change production Agent release metadata or claim new Agent functionality.

- [ ] **Step 2: Run full verification**

Run: `npm test`

Expected: all Go race/integration and Node tests pass, with only documented environment skips.

Run: `npm run vet`

Expected: exit 0.

Run: `npm run build:release`

Expected: a `v0.9.8` Linux amd64 release with verified inner/outer SHA256.

- [ ] **Step 3: Review the complete branch**

Inspect `git diff`, confirm unrelated untracked website/deployment files remain untouched, verify no tenant-scoping regression, and confirm no UI copy equates online/in-sync with end-to-end health.

- [ ] **Step 4: Commit the release/documentation slice**

```bash
git add package.json README.md docs/ROADMAP.md docs/IMPLEMENTATION_STATUS.md
git commit -m "docs: prepare v0.9.8 node monitoring release"
```

- [ ] **Step 5: Back up and deploy only the production control component**

On `103.30.78.5`, create and verify a new root-only backup containing the current control binary, service unit, environment file and custom-format PostgreSQL dump. Upload the exact built archive, verify its outer and inner checksums, run staged `reladock --check-config`, install the `control` component, restart `reladock`, and retain automatic rollback to the verified prior binary/unit if startup fails. Do not connect to or modify the entry node.

- [ ] **Step 6: Verify production behavior and record evidence**

Verify service active/version, database schema, fresh `node-01` heartbeat/resource sample, public `/`, `/admin`, `/node-monitoring.js`, authenticated/unauthenticated API boundaries, blue theme assets, and a browser session showing live value changes without full-page refresh. Update `docs/IMPLEMENTATION_STATUS.md` with the exact deployed commit, backup path, test counts and remaining unverified end-to-end/production-capacity boundaries, then commit that record.
