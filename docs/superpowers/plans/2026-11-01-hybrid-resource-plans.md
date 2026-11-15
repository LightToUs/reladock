# Hybrid Resource Plans Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add shared, tenant-dedicated, and rule-dedicated node/package modes while preserving one independent quota and bandwidth package per relay rule.

**Architecture:** Extend PostgreSQL node and immutable plan-version facts with one common resource-mode vocabulary, enforce node admission under the existing node lock, and filter/revalidate package compatibility at both catalog and purchase boundaries. Reuse the existing per-rule subscription, wallet, usage, nft quota, and renewal-after-exhaustion paths; cloud provisioning remains manual and outside this increment.

**Tech Stack:** Go 1.24+, PostgreSQL migrations/transactions, static ES modules, Node.js 20+ tests.

**Spec:** `docs/superpowers/specs/2026-11-01-hybrid-resource-plans-design.md`

## Global Constraints

- RelaDock remains a B2B network relay and does not add VPN users, protocol credentials, subscriptions, or protocol termination.
- Traffic quota and bandwidth remain independent per relay rule; no tenant or node byte pool is introduced.
- Existing nodes and plan versions migrate to `shared` without changing current rules or accounting history.
- Node purchase/release remains manual; no cloud credentials or provider API calls are added.
- Applied configuration, Agent contact, target reachability, and end-to-end health remain separate facts.
- PostgreSQL remains the source of truth; wallet debits and usage history stay immutable and idempotent.
- Preserve the current dirty workspace and unrelated changes. Because required files already contain user changes, do not create commits or stage whole files; use scoped diffs and verification checkpoints instead.
- Update `docs/IMPLEMENTATION_STATUS.md` whenever implementation or verification status changes.

## Review Focus

- Two concurrent creates on one `dedicated-rule` node must yield one rule and one conflict, never two rules (Task 2 test).
- A stale or forged purchase for a plan whose mode differs from the rule's node must not debit the wallet or create a subscription (Task 3 test).
- Existing rows must migrate to `shared`, and migration reruns must not duplicate or rewrite immutable billing facts (Task 1 test).
- Changing node ownership/mode with undeleted rules must conflict without changing node revision or emitting audit success (Task 2 test).
- Buying a new package after quota exhaustion must start at zero for that rule while late reports remain attributed to the old period and other rules remain unchanged (Task 3 regression test).

---

### Task 1: Persist a common resource-mode model

**Files:**
- Create: `internal/platform/resource_mode.go`
- Create: `internal/platform/migration26.sql`
- Modify: `internal/platform/store.go`
- Modify: `internal/platform/store_test.go`
- Test: `internal/platform/resource_mode_test.go`

**Interfaces:**
- Produces: `type ResourceMode string` with `ResourceModeShared`, `ResourceModeDedicatedTenant`, and `ResourceModeDedicatedRule`.
- Produces: `normalizeResourceMode(value string) (ResourceMode, error)` for strict API/domain validation.
- Persists: `nodes.resource_mode`, `nodes.dedicated_tenant_id`, and immutable `plan_price_versions.resource_mode`.

- [ ] **Step 1: Write migration and value-object failing tests**

  Add tests asserting strict acceptance of the three wire values, rejection of empty/unknown values, migration version 26, `shared` defaults for all pre-v26 nodes/plans, and database rejection of shared-with-owner or dedicated-without-owner combinations.

- [ ] **Step 2: Run the focused tests and verify failure**

  Run: `go test ./internal/platform -run 'Test(ResourceMode|Migration26)'`

  Expected: FAIL because the type, columns, and migration do not exist.

- [ ] **Step 3: Add migration 26 and embed/apply it transactionally**

  Add the two node columns with a cross-column CHECK and tenant foreign key; add a non-null `resource_mode` to immutable plan versions. Default existing rows to `shared`. Embed `migration26.sql` and append version 26 after version 25 in `Store.Migrate` without touching v25's uncommitted work.

- [ ] **Step 4: Implement the strict resource-mode value object**

  Implement only the three specified values and use `ErrInvalid` for external invalid input.

- [ ] **Step 5: Run focused tests and inspect the scoped diff**

  Run: `go test ./internal/platform -run 'Test(ResourceMode|Migration26)'`

  Expected: PASS with a real test schema; direct runs without `TEST_DATABASE_URL` are not full verification.

  Run: `git diff --check -- internal/platform/resource_mode.go internal/platform/migration26.sql internal/platform/store.go internal/platform/store_test.go internal/platform/resource_mode_test.go`

### Task 2: Enforce mixed node tenancy and expose tenant-safe inventory

**Files:**
- Modify: `internal/platform/admin_nodes.go`
- Modify: `internal/platform/nodes.go`
- Modify: `internal/platform/rules.go`
- Modify: `internal/platform/admin_test.go`
- Modify: `internal/platform/rules_test.go`
- Modify: `internal/web/server.go`
- Modify: `internal/web/server_test.go`
- Test: `internal/platform/resource_mode_test.go`

**Interfaces:**
- Extends: `NodeInput` with `ResourceMode ResourceMode` and `DedicatedTenantID string`.
- Extends: `ManagedNode` with resource mode and dedicated tenant ID; tenant-facing `Node` exposes resource mode but not another tenant's identity.
- Produces: `Store.ListTenantNodes(ctx context.Context, who Identity) ([]Node, error)` returning shared nodes plus dedicated nodes owned by `who.TenantID`.
- Produces: `checkNodeAdmission(ctx context.Context, tx pgx.Tx, nodeID, tenantID string) error`, called only while the node lock is held.

- [ ] **Step 1: Write failing node administration and admission tests**

  Assert valid shared/dedicated registration, invalid mode/owner pairs, nonexistent tenant rejection, mode/owner changes only on empty nodes, cross-tenant dedicated-node rejection, multiple same-tenant rules on `dedicated-tenant`, and one-rule maximum on `dedicated-rule`.

- [ ] **Step 2: Add the dedicated-rule concurrency test**

  Start two rule creations for the same empty `dedicated-rule` node and assert exactly one success, one `ErrConflict`, one undeleted rule, and no orphan endpoint allocation or idempotency record for the loser.

- [ ] **Step 3: Run focused platform tests and verify failure**

  Run: `go test -race ./internal/platform -run 'Test(NodeResourceMode|DedicatedRule|DedicatedTenant)'`

  Expected: FAIL because node mode is not read or enforced.

- [ ] **Step 4: Extend node CRUD without weakening existing lifecycle checks**

  Normalize resource mode and dedicated tenant ID, treating an omitted mode as
  `shared` only in the legacy local `AddNode` wrapper so existing operator CLI
  calls remain compatible. Browser/admin inputs are explicit. Verify the tenant
  foreign key through normal SQL constraints/query handling, return the fields
  from admin listing, and block mode/owner changes whenever an undeleted rule
  exists. Keep address, capacity, revision, audit, withdrawal, and retirement
  behavior intact.

- [ ] **Step 5: Enforce node admission under the existing lock**

  Call `checkNodeAdmission` in `CreateRule` after locking and reading the node. Shared accepts any tenant; tenant-dedicated requires owner equality; rule-dedicated additionally requires zero undeleted rules. Return `ErrConflict` for a valid but unavailable/different-owner node without leaking the owner identity.

- [ ] **Step 6: Scope tenant node inventory**

  Use `ListTenantNodes` for `/api/overview` and `/api/nodes`; keep `ListNodes` for internal/Agent-compatible inventory. Add HTTP tests proving another tenant cannot discover a dedicated node while its owner can and all authenticated tenants still see shared nodes.

- [ ] **Step 7: Run focused tests and inspect the scoped diff**

  Run: `go test -race ./internal/platform ./internal/web -run 'Test(NodeResourceMode|DedicatedRule|DedicatedTenant|TenantNodeInventory)'`

  Expected: PASS.

  Run: `git diff --check -- internal/platform/admin_nodes.go internal/platform/nodes.go internal/platform/rules.go internal/platform/admin_test.go internal/platform/rules_test.go internal/platform/resource_mode_test.go internal/web/server.go internal/web/server_test.go`

### Task 3: Bind immutable package types to node resource modes

**Files:**
- Modify: `internal/platform/subscriptions.go`
- Modify: `internal/platform/subscriptions_test.go`
- Modify: `internal/web/subscriptions.go`
- Modify: `internal/web/subscriptions_http_test.go`

**Interfaces:**
- Extends: `PlanInput` and `PlanPriceVersion` JSON with `resourceMode`.
- Changes: `ListPlans(ctx context.Context, who Identity, ruleID string, page int) (Page[PlanPriceVersion], error)`; empty `ruleID` lists the full catalog, otherwise ownership is verified and only matching node-mode plans are returned.
- Extends: `GET /api/plans` with optional, single `ruleId`; `/api/admin/plans` remains platform-only and unfiltered.
- Preserves: `PurchasePlan(ctx, who, key, PurchasePlanInput)` signature and current no-early-renewal semantics.

- [ ] **Step 1: Write failing immutable plan-mode tests**

  Assert plan creation accepts only the three modes, same code/version replay conflicts when mode differs, existing immutability triggers reject mode mutation, and admin/full catalog responses include the mode.

- [ ] **Step 2: Write failing catalog and purchase-boundary tests**

  Assert rule-filtered catalogs return only matching modes with tenant ownership checks. Attempt a forged incompatible purchase and assert `ErrConflict`, unchanged wallet balance, zero ledger rows, zero new subscriptions, unchanged rule version/status, and no Outbox generation.

- [ ] **Step 3: Pin the existing exhaustion-reset behavior**

  Extend the two-rule quota test: exhaust rule A, buy a compatible replacement, assert its new period starts at zero; report late old-period usage and assert it stays on the closed period; assert rule B's usage and entitlement never change.

- [ ] **Step 4: Run focused subscription tests and verify failure**

  Run: `go test -race ./internal/platform ./internal/web -run 'Test(PlanResourceMode|PlanCatalogForRule|PurchasePlanResourceMode|IndependentRuleQuota)'`

  Expected: FAIL because plan versions have no mode and purchases do not compare node mode.

- [ ] **Step 5: Extend plan scans, creation, and catalog filtering**

  Add `resource_mode` to the exact immutable plan column list and insert path. For filtered tenant catalogs, authorize the rule, read its node mode, and constrain the catalog query without exposing dedicated owner IDs.

- [ ] **Step 6: Revalidate compatibility inside `PurchasePlan`**

  While holding the existing node lock and rule row lock, read the node mode with capacity and reject a plan-mode mismatch before wallet mutation. Do not alter period attribution, wallet lock order, quota enforcement, or Agent payload semantics.

- [ ] **Step 7: Extend strict HTTP query handling**

  Permit `ruleId` only on tenant `GET /api/plans`, reject duplicates/unknown parameters, preserve page validation, and verify tenant isolation and forged purchase responses.

- [ ] **Step 8: Run focused tests and inspect the scoped diff**

  Run: `go test -race ./internal/platform ./internal/web -run 'Test(PlanResourceMode|PlanCatalogForRule|PurchasePlanResourceMode|IndependentRuleQuota)'`

  Expected: PASS.

  Run: `git diff --check -- internal/platform/subscriptions.go internal/platform/subscriptions_test.go internal/web/subscriptions.go internal/web/subscriptions_http_test.go`

### Task 4: Present resource-aware nodes and packages in both consoles

**Files:**
- Create: `public/resource-mode.js`
- Test: `public/resource-mode.test.js`
- Modify: `public/assets.go`
- Modify: `public/assets_test.go`
- Modify: `public/admin.html`
- Modify: `public/admin.js`
- Modify: `public/node-editor.js`
- Modify: `public/node-editor.test.js`
- Modify: `public/index.html`
- Modify: `public/business-tools.js`
- Modify: `internal/web/static_assets_test.go`

**Interfaces:**
- Produces: `resourceModeLabel(value)` and `planMatchesNode(plan, node)` in `public/resource-mode.js`.
- Admin node form submits `resourceMode` and `dedicatedTenantId`; the dedicated tenant field is required only for dedicated modes and cleared/disabled for shared mode.
- Tenant purchase UI reloads `/api/plans?ruleId=<id>` when the selected rule changes and labels each plan's resource mode.

- [ ] **Step 1: Write failing pure presentation tests**

  Assert Chinese labels for all three modes, unknown values remain explicit, plan/node matching is exact, and node-editor create/edit sessions round-trip both new fields without retaining a prior dedicated tenant on a shared create.

- [ ] **Step 2: Run Node tests and verify failure**

  Run: `node --test public/resource-mode.test.js public/node-editor.test.js`

  Expected: FAIL because the module and form fields do not exist.

- [ ] **Step 3: Extend the platform node dialog and list**

  Add the mode select and dedicated tenant UUID input, dynamic required/disabled behavior, node list labels, and tenant IDs in the tenant table. Keep all current dialog session/conflict protections and do not rebuild controls during monitoring refresh.

- [ ] **Step 4: Extend plan administration and customer purchase copy**

  Add resource mode to immutable plan creation/listing. In the customer workspace, fetch compatible plans for the selected rule, preserve selection only when still valid, label the resource mode, and keep the existing truthful message that purchase waits for node delivery. State clearly that sharing applies to node resources, never package traffic.

- [ ] **Step 5: Embed and assert every new module/field**

  Add `resource-mode.js` to the embedded filesystem and update static asset tests for node mode controls, plan mode controls, rule-filtered catalog requests, and independent-quota copy.

- [ ] **Step 6: Run frontend/static tests and inspect the scoped diff**

  Run: `node --test public/*.test.js`

  Run: `go test ./public ./internal/web -run 'Test(Embedded|Static|Admin)'`

  Expected: PASS.

  Run: `git diff --check -- public/resource-mode.js public/resource-mode.test.js public/assets.go public/assets_test.go public/admin.html public/admin.js public/node-editor.js public/node-editor.test.js public/index.html public/business-tools.js internal/web/static_assets_test.go`

### Task 5: Update product truth and run full verification

**Files:**
- Modify: `README.md`
- Modify: `docs/REQUIREMENTS.md`
- Modify: `docs/ARCHITECTURE.md`
- Modify: `docs/ROADMAP.md`
- Modify: `docs/IMPLEMENTATION_STATUS.md`

**Interfaces:**
- Documents: mixed node resource modes are implemented control-plane constraints; per-rule packages remain independent.
- Documents: provider API provisioning, automatic server purchase/release, early renewal queues, production cloud/network validation, and SLA claims remain unimplemented.

- [ ] **Step 1: Update requirements and architecture**

  Record the confirmed shared/dedicated modes, one-line-one-package rule, no quota sharing, hard stop at quota, manual provisioning boundary, and exact enforcement points.

- [ ] **Step 2: Update roadmap and implementation status**

  Mark only code/test-backed behavior implemented. Add migration 26 and focused test evidence, retain `productionReady=false`, and list cloud provider automation and production acceptance as unimplemented.

- [ ] **Step 3: Run formatting and complete test suite**

  Run: `gofmt -w internal/platform/resource_mode.go internal/platform/resource_mode_test.go internal/platform/admin_nodes.go internal/platform/nodes.go internal/platform/rules.go internal/platform/admin_test.go internal/platform/rules_test.go internal/platform/subscriptions.go internal/platform/subscriptions_test.go internal/platform/store.go internal/platform/store_test.go internal/web/server.go internal/web/server_test.go internal/web/subscriptions.go internal/web/subscriptions_http_test.go internal/web/static_assets_test.go`

  Run: `npm test`

  Expected: all PostgreSQL-backed Go race tests and Node tests pass; any skipped environment test is reported explicitly.

- [ ] **Step 4: Run static analysis and final diff checks**

  Run: `npm run vet`

  Run: `git diff --check`

  Expected: PASS with no formatting errors.

- [ ] **Step 5: Review only the feature delta and report boundaries**

  Inspect `git diff` while distinguishing pre-existing workspace edits from this feature. Report implemented behavior, exact verification output, skipped tests, and that no cloud API, production server, payment merchant, or real network rule was touched.
