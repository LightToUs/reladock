# Unassigned Dedicated Node Claim Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Allow `dedicated-tenant` nodes to remain unassigned until the first successful plan purchase atomically binds the node to that tenant, with audited safe manual release.

**Architecture:** Schema v43 relaxes only the dedicated-tenant ownership constraint. Catalog and checkout validation treat an unassigned dedicated-tenant node as saleable, while the existing node advisory lock serializes the final purchase; `finishPlanPurchase` performs the claim immediately before debit/subscription writes so any later error rolls everything back. Platform administration exposes honest “待分配” state and a guarded release action; ordinary node editing cannot bypass release checks.

**Tech Stack:** Go 1.24+, PostgreSQL migrations/transactions/row and advisory locks, static HTML/CSS/JavaScript, Node test runner, real PostgreSQL integration tests.

**Spec:** `docs/superpowers/specs/2026-11-16-unassigned-dedicated-node-claim-design.md`

## Global Constraints

- Only `dedicated-tenant` may have an empty `dedicated_tenant_id`; `shared` must remain empty and `dedicated-rule` must remain bound.
- Quotes, checkout orders, failed payments and failed purchases never claim a node.
- Claim, node revision, platform audit, wallet debit, ledger and subscription commit in one transaction.
- Competing tenants serialize on the node lock; only the committed winner may spend funds.
- Refund, dispute, expiry and rule deletion never release ownership automatically.
- Manual release is platform-admin-only, revision-guarded and blocked by active subscriptions, undeleted rules, active allocations/retirements, or unfinished delivery.
- No Agent, nftables/tc, payment-provider configuration or real cloud resource changes belong to this feature.

## Review Focus

- Stripe order creation against an unassigned node must validate the sale without mutating `nodes`.
- A credited Stripe order whose purchase is rejected must retain wallet credit but leave the node unassigned.
- Two tenant transactions with different idempotency keys must not both pass an initially empty owner.
- A bound node must not be made unassigned or reassigned through the generic edit endpoint.
- Release must treat validated/failed/leased/applying configuration and unreleased retirement state as unfinished work.

---

### Task 1: Schema v43 and node input semantics

**Files:**
- Create: `internal/platform/migration43.sql`
- Modify: `internal/platform/store.go`
- Modify: `internal/platform/store_test.go`
- Modify: `internal/platform/resource_mode_test.go`
- Modify: `internal/platform/admin_nodes.go`

**Interfaces:**
- Produces: schema v43 constraint and `normalizeNode(NodeInput) (NodeInput, error)` semantics where only an empty dedicated-tenant owner is accepted.
- Produces: generic node edit protection that rejects changing or clearing a bound dedicated-tenant owner.

- [ ] **Step 1: Write failing migration and normalization tests**
  Add `TestMigration43AllowsOnlyUnassignedDedicatedTenantNodes` and extend `TestAdminNodeResourceModeValidationAndMutation` to assert blank dedicated-tenant succeeds, blank dedicated-rule fails, shared-with-owner fails, v42 assigned rows survive, and generic edit cannot clear/reassign a bound owner.
- [ ] **Step 2: Run RED tests**
  Run: `go test -race ./internal/platform -run 'TestMigration43AllowsOnlyUnassignedDedicatedTenantNodes|TestAdminNodeResourceModeValidationAndMutation'`
  Expected: FAIL because schema stays v42 and normalization rejects the blank owner.
- [ ] **Step 3: Implement migration and normalization**
  Add migration43 embedding/execution, set `SchemaVersion=43`, replace only `nodes_resource_mode_owner_check`, and make `normalizeNode` accept blank owner only for `ResourceModeDedicatedTenant`.
- [ ] **Step 4: Protect bound ownership in generic edit**
  In `mutateNode`, reject any mode/owner change from a bound dedicated-tenant node; the dedicated release method added later is the only clearing path.
- [ ] **Step 5: Run GREEN tests**
  Run the Step 2 command and `go test -race ./internal/platform -run 'TestMigration|TestAdminNodeResourceMode'`.
- [ ] **Step 6: Commit**
  Commit migration, store integration, normalization and tests.

### Task 2: Catalog visibility and atomic first purchase claim

**Files:**
- Modify: `internal/platform/subscriptions.go`
- Modify: `internal/platform/subscriptions_test.go`
- Modify: `internal/platform/payments.go`
- Modify: `internal/platform/payments_test.go`

**Interfaces:**
- Produces: `checkNodeSaleAdmission(ctx context.Context, tx pgx.Tx, nodeID, tenantID string, claim bool) (bool, error)`; return bool reports whether `finishPlanPurchase` must claim.
- Extends: `preparedPlanPurchase` with `claimDedicatedNode bool`.
- Consumes: schema v43 nullable dedicated-tenant ownership.

- [ ] **Step 1: Write failing catalog and wallet purchase tests**
  Add tests proving unassigned dedicated plans appear to two tenants, first successful purchase sets the owner/revision/audit, idempotent replay does not repeat debit/audit, losing tenant receives the dedicated-claim conflict, and insufficient balance/audit failure preserve null ownership and all monetary counts.
- [ ] **Step 2: Run RED wallet tests**
  Run the new focused tests with `go test -race ./internal/platform -run 'UnassignedDedicated|DedicatedClaim'`.
  Expected: FAIL because the catalog excludes null ownership and purchase admission rejects it.
- [ ] **Step 3: Implement read-only sale admission and catalog filtering**
  Permit unassigned dedicated-tenant nodes for sale validation, preserve existing shared/dedicated-rule rules, and include unassigned dedicated-tenant offerings in the unbound catalog without exposing nodes bound to another tenant.
- [ ] **Step 4: Implement final purchase claim**
  Record the claim requirement during preparation; in `finishPlanPurchase`, update only the still-unassigned dedicated-tenant row, increment revision, and write `node.dedicated-claim` before any debit. Return a dedicated conflict if another tenant already owns it. All writes remain in the caller transaction.
- [ ] **Step 5: Write and run failing concurrency test**
  Add a two-tenant simultaneous purchase test using distinct wallets/keys. Before the claim implementation it must allow neither or violate the desired one-winner result; after implementation assert exactly one success, one claim conflict, one debit/subscription, and untouched losing wallet.
- [ ] **Step 6: Add Stripe quote/callback tests**
  Assert `CheckoutPlanPayment` creates/reuses an order without claiming; a successful callback claims with the purchase, while a callback whose purchase becomes unavailable credits the wallet and leaves ownership null.
- [ ] **Step 7: Run GREEN subscription/payment tests**
  Run: `go test -race ./internal/platform -run 'UnassignedDedicated|DedicatedClaim|PlanPayment.*Unassigned'`.
- [ ] **Step 8: Commit**
  Commit catalog, transaction, payment and concurrency changes.

### Task 3: Audited manual release API

**Files:**
- Modify: `internal/platform/admin_nodes.go`
- Modify: `internal/platform/resource_mode_test.go`
- Modify: `internal/web/admin.go`
- Modify: `internal/web/server_test.go`

**Interfaces:**
- Produces: `AdminReleaseDedicatedTenantNode(ctx context.Context, who Identity, id string, revision int64) error`.
- Extends: `ManagedNode` with `DedicatedTenantReleaseEligible bool` and `DedicatedTenantReleaseReasons []string`.
- Produces: `POST /api/admin/nodes/:id/release-dedicated-tenant` with `{revision}`.

- [ ] **Step 1: Write failing store tests**
  Assert platform permission and revision checks, successful release, audit/revision update, and rollback on audit failure. Table-drive blockers for an active subscription, undeleted rule, endpoint allocation, non-released retirement, and config state `queued`, `leased`, `validated`, `applying`, or `failed`.
- [ ] **Step 2: Run RED store tests**
  Expected: compile failure because `AdminReleaseDedicatedTenantNode` and eligibility fields do not exist.
- [ ] **Step 3: Implement eligibility and release transaction**
  Under platform and node locks, load the revision/owner with `FOR UPDATE`, compute exact blockers using fresh queries, clear only a bound dedicated-tenant owner, increment revision, and write `node.dedicated-release` in the same transaction.
- [ ] **Step 4: Write failing HTTP tests**
  Cover 401/403, strict JSON, mismatched/stale revisions, conflict blockers and successful 200 response.
- [ ] **Step 5: Implement and verify the route**
  Add the exact POST route before generic node actions; run focused platform and web tests until green.
- [ ] **Step 6: Commit**
  Commit release store/API and tests.

### Task 4: Platform UI for optional owner, pending allocation and release

**Files:**
- Modify: `public/admin.html`
- Modify: `public/admin.js`
- Modify: `public/node-editor.js`
- Modify: `public/node-editor.test.js`
- Modify: `public/node-action-dialog.js`
- Modify: `public/node-action-dialog.test.js`
- Modify: `internal/web/static_assets_test.go`

**Interfaces:**
- Consumes: `ManagedNode.dedicatedTenantReleaseEligible`, `.dedicatedTenantReleaseReasons`, and the release endpoint.
- Produces: form behavior where dedicated-tenant owner is optional and dedicated-rule owner remains required.

- [ ] **Step 1: Write failing UI behavior tests**
  Test shared owner disabled/cleared, dedicated-tenant enabled/optional, dedicated-rule enabled/required, and existing values preserved when editing. Test list presentation uses “待分配 · 首次成功购买后绑定” for a blank dedicated-tenant node.
- [ ] **Step 2: Run RED Node/static tests**
  Run: `node --test public/node-editor.test.js public/node-action-dialog.test.js` and the focused static-assets Go test.
- [ ] **Step 3: Implement form and presentation**
  Update labels/help text and `syncDedicatedTenant`; show the dedicated release control only for a bound dedicated-tenant node and disable it with exact server-provided blockers when ineligible.
- [ ] **Step 4: Wire confirmation action**
  Add a release dialog descriptor and call `/api/admin/nodes/:id/release-dedicated-tenant` with current revision; keep errors in the dialog and refresh on success.
- [ ] **Step 5: Run GREEN UI/static tests**
  Run the Step 2 commands plus all public Node tests.
- [ ] **Step 6: Commit**
  Commit UI and static contract tests.

### Task 5: Documentation, full verification and production deployment

**Files:**
- Modify: `README.md`
- Modify: `docs/REQUIREMENTS.md`
- Modify: `docs/ARCHITECTURE.md`
- Modify: `docs/ROADMAP.md`
- Modify: `docs/IMPLEMENTATION_STATUS.md`

**Interfaces:**
- Consumes: completed schema v43 behavior and exact verification/deployment evidence.
- Produces: durable handoff and production rollback record.

- [ ] **Step 1: Update product and architecture docs**
  Record the exact claim/release semantics and keep Agent/data-plane/health boundaries explicit.
- [ ] **Step 2: Run full local verification**
  Run `GOFLAGS=-timeout=20m npm test`, `npm run vet`, `git diff --check`, and `npm run build:release`; record exact pass counts, skips and SHA256 values.
- [ ] **Step 3: Rehearse migration on a production backup**
  Create and verify a custom-format production dump, restore it into a temporary database, run the candidate v42→v43 migration, and prove account/session/TRON replay guards and zero business-row facts remain unchanged.
- [ ] **Step 4: Deploy with rollback boundary**
  Run candidate `--check-config`, stop the service, create an immediate verified dump and binary/config backup, install exact-hash controller/admin/recovery binaries, start and wait for schema v43/health. Rollback requires restoring both old binaries and the immediate v42 dump because the v42 controller rejects schema v43.
- [ ] **Step 5: Verify production behavior**
  Confirm active/enabled service, schema 43, public resources, unauthenticated 401 boundaries, zero business counts, wallet/ledger invariants and no new warning logs. Do not create a real node, plan, payment or traffic merely to test deployment.
- [ ] **Step 6: Update implementation status with production facts**
  Record backup path, hashes, schema, counts, warnings, untested logged-in interaction and unchanged Agent/data-plane boundaries.
- [ ] **Step 7: Commit and push**
  Commit documentation/evidence and push the feature branch after production verification.
