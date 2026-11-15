# Node-bound Plan Catalog Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add node-bound line offerings so new rule-plan purchases for the existing Hong Kong node cannot be confused with packages for another location.

**Architecture:** Add a mutable `line_offerings` catalog record bound to one node, then attach every new immutable plan price version to an offering. Filter the customer catalog by the selected rule's exact node and repeat offering state, node binding, and mode checks inside the existing atomic purchase transaction.

**Tech Stack:** Go 1.24+, PostgreSQL migrations and transactions, net/http, static HTML/CSS/JavaScript, Node.js tests.

**Spec:** `docs/superpowers/specs/2026-11-01-node-bound-plan-catalog-design.md`

## Global Constraints

- Preserve existing uncommitted work and do not modify production resources.
- Existing unbound price versions and all subscriptions, usage, and ledger facts remain intact and immutable.
- Every new price version must bind one offering; no Hong Kong price or quota is seeded or guessed.
- One rule owns one independent quota, bandwidth limit, duration, and period usage total.
- A purchase response remains a configuration request, not Agent application or end-to-end health evidence.
- Update `docs/IMPLEMENTATION_STATUS.md` whenever implementation or verification status changes.

## Review Focus

- A plan for a different shared node must not appear or purchase for the Hong Kong rule; Task 3 adds catalog and rollback tests.
- Pausing an offering concurrently with purchase must serialize before wallet debit; Task 3 adds a transaction race test.
- Migration must preserve immutable legacy plans and active periods without assigning a guessed node; Task 1 adds migration assertions.
- A stale administrator revision must not overwrite a newer sale state; Task 2 adds optimistic-conflict tests.
- Customer and administrator pages must render legacy/unavailable states without enabling purchase; Task 5 adds DOM/static tests.

---

### Task 1: Persist node-bound line offerings

**Files:**
- Create: `internal/platform/migration27.sql`
- Modify: `internal/platform/store.go`
- Modify: `internal/platform/store_test.go`

**Interfaces:**
- Produces: schema v27 with `line_offerings` and nullable `plan_price_versions.offering_id`.
- Produces: sale-state constraint for `draft`, `published`, `paused`, `sold-out`, and `retired`.

- [ ] **Step 1: Write failing migration tests**

Add tests proving v26-to-v27 migration preserves an existing immutable plan and subscription, leaves its `offering_id` null, enforces unique offering codes, validates state values, and rejects dangling node/offering references.

- [ ] **Step 2: Run the migration tests and verify failure**

Run: `go test ./internal/platform -run 'TestMigration27' -count=1`

Expected: FAIL because schema version 27 and its tables/columns do not exist.

- [ ] **Step 3: Implement migration v27**

Create `line_offerings` with UUID identity, unique validated code, fixed node FK, bounded name/source/destination text, state, revision, and timestamps. Add nullable `offering_id` to `plan_price_versions`; embed and execute the migration from `Store.Migrate`, and set `SchemaVersion = 27`.

- [ ] **Step 4: Run the migration tests**

Run: `go test ./internal/platform -run 'TestMigration27' -count=1`

Expected: PASS.

### Task 2: Add the administrator offering domain API

**Files:**
- Create: `internal/platform/offerings.go`
- Create: `internal/platform/offerings_test.go`

**Interfaces:**
- Produces: `OfferingState`, `LineOffering`, `LineOfferingInput`, and `OfferingStateInput` JSON types.
- Produces: `AdminCreateOffering(context.Context, Identity, LineOfferingInput) (LineOffering, error)`.
- Produces: `AdminSetOfferingState(context.Context, Identity, string, OfferingStateInput) (LineOffering, error)`.
- Produces: `AdminOfferings(context.Context, Identity, int) (Page[LineOffering], error)`.

- [ ] **Step 1: Write failing domain tests**

Cover platform-admin authorization, normalization and bounds, exact node binding, default draft state, idempotent replay versus conflicting code reuse, active-node publishing, paused/sold-out republishing, terminal retirement, revision conflicts, and same-transaction audit rollback.

- [ ] **Step 2: Run the offering tests and verify failure**

Run: `go test ./internal/platform -run 'TestAdmin.*Offering' -count=1`

Expected: FAIL because the offering API is undefined.

- [ ] **Step 3: Implement the offering domain**

Use platform admin transactions and row locks. Derive `ResourceMode` from the bound node for every response. Only the state is mutable; increment `revision` and write `offering.create` or `offering.state` audit records in the same transaction.

- [ ] **Step 4: Run offering tests**

Run: `go test ./internal/platform -run 'TestAdmin.*Offering' -count=1`

Expected: PASS.

### Task 3: Bind price versions, catalogs, and purchases to offerings

**Files:**
- Modify: `internal/platform/subscriptions.go`
- Modify: `internal/platform/subscriptions_test.go`
- Modify: `internal/platform/entitlement_test.go`
- Modify: `internal/platform/costs_test.go`
- Modify: `internal/platform/probes_test.go`
- Modify: `internal/platform/rules_test.go`
- Modify: `internal/platform/wallet_hold_test.go`

**Interfaces:**
- Consumes: Task 2 `LineOffering` and administrator methods.
- Changes: `PlanInput` requires `OfferingID`; `PlanPriceVersion` returns optional offering metadata for legacy rows.
- Preserves: `PurchasePlan(context.Context, Identity, string, PurchasePlanInput) (RuleSubscription, error)` signature.

- [ ] **Step 1: Write failing plan and purchase tests**

Cover required offering binding, mode derivation, immutable replay, exact-node rule catalogs with two shared nodes, hidden legacy rows, non-published offerings, forged other-node purchases with unchanged wallet/ledger/outbox, and serialized pause-versus-purchase behavior.

- [ ] **Step 2: Run focused tests and verify failure**

Run: `go test ./internal/platform -run 'Test(Plan|Purchase|Offering).*' -count=1`

Expected: FAIL because plans do not yet carry an offering.

- [ ] **Step 3: Implement offering-aware plan reads and creation**

Join optional offering metadata when scanning plans. Require a valid offering for every new version, derive and store its node resource mode, list legacy rows only to administrators, and return customer plans only for the selected owned rule's exact node and published offerings.

- [ ] **Step 4: Implement atomic purchase checks**

After the existing node and rule locks, lock the offering row before wallet access. Reject null bindings, non-published state, node mismatch, mode mismatch, or inactive nodes without any billing or configuration mutation.

- [ ] **Step 5: Update existing fixtures to create a draft offering, publish it, and bind plans**

Keep fixture data explicit; do not add a globally seeded offering or plan.

- [ ] **Step 6: Run platform tests**

Run: `go test ./internal/platform -count=1`

Expected: PASS.

### Task 4: Expose strict HTTP endpoints

**Files:**
- Modify: `internal/web/subscriptions.go`
- Modify: `internal/web/subscriptions_http_test.go`
- Modify: `internal/web/server_test.go`

**Interfaces:**
- Produces: `GET, POST /api/admin/offerings`.
- Produces: `POST /api/admin/offerings/state` with offering ID, expected revision, and target state.
- Changes: `POST /api/admin/plans` accepts `offeringId`; `GET /api/plans` requires `ruleId`.

- [ ] **Step 1: Write failing HTTP tests**

Cover admin-only create/list/state operations, strict unknown-field and query rejection, invalid methods, stale revisions, tenant catalog exact-node filtering, and atomic forged purchase rejection.

- [ ] **Step 2: Run HTTP tests and verify failure**

Run: `go test ./internal/web -run 'TestSubscriptionsHTTP' -count=1`

Expected: FAIL because offering routes are unhandled.

- [ ] **Step 3: Implement endpoint routing and validation**

Extend `serveSubscriptions` without weakening session, same-origin, rate-limit, body-size, or platform-admin checks. Keep `Cache-Control: no-store` and existing error mappings.

- [ ] **Step 4: Run HTTP tests**

Run: `go test ./internal/web -run 'TestSubscriptionsHTTP' -count=1`

Expected: PASS.

### Task 5: Add administrator and customer catalog presentation

**Files:**
- Modify: `public/admin.html`
- Modify: `public/admin.js`
- Modify: `public/business-tools.js`
- Modify: `public/business-tools.test.js`
- Modify: `public/assets_test.go`
- Modify: `internal/web/static_assets_test.go`

**Interfaces:**
- Consumes: Task 4 offering and plan JSON endpoints.
- Produces: admin offering form/list/state controls and offering-bound plan form.
- Produces: customer plan labels containing line name and source-to-destination direction.

- [ ] **Step 1: Write failing frontend/static tests**

Assert that plan creation sends `offeringId`, no longer accepts an independent resource-mode choice, unavailable offerings cannot be selected for new versions, legacy plans are labeled unbound in the admin table, and customer options include route identity while preserving independent-quota copy.

- [ ] **Step 2: Run frontend tests and verify failure**

Run: `node --test public/*.test.js`

Expected: FAIL on missing offering behavior.

- [ ] **Step 3: Implement the admin catalog UI**

Add a line-offering section above immutable prices, populate the existing-node selector, expose explicit state controls with revisions, and derive the plan resource-mode label from the chosen offering.

- [ ] **Step 4: Implement customer catalog labels and empty states**

Show line name, source-to-destination direction, resource mode, quota, bandwidth, duration, and price. Keep purchase disabled when no published exact-node version is returned.

- [ ] **Step 5: Run frontend and static tests**

Run: `node --test public/*.test.js && go test ./internal/web -run 'Test.*Static|Test.*Asset' -count=1`

Expected: PASS.

### Task 6: Document and verify the completed boundary

**Files:**
- Modify: `README.md`
- Modify: `docs/REQUIREMENTS.md`
- Modify: `docs/ARCHITECTURE.md`
- Modify: `docs/ROADMAP.md`
- Modify: `docs/IMPLEMENTATION_STATUS.md`

**Interfaces:**
- Consumes: all prior tasks.
- Produces: factual implementation and verification record without a production deployment claim.

- [ ] **Step 1: Update product, architecture, roadmap, and handoff documentation**

Document node-bound offerings, legacy unbound history, exact-node purchase validation, manual Hong Kong catalog setup, and the absence of automatic provisioning, seeded prices, or production deployment.

- [ ] **Step 2: Run formatting and focused checks**

Run: `gofmt -w internal/platform/offerings.go internal/platform/offerings_test.go internal/platform/subscriptions.go internal/platform/subscriptions_test.go internal/web/subscriptions.go internal/web/subscriptions_http_test.go`

Run: `git diff --check`

Expected: both commands succeed without diagnostics.

- [ ] **Step 3: Run the complete required verification**

Run: `npm test`

Run: `npm run vet`

Expected: all available Go race, Node, and static-analysis checks pass; any environment skip is recorded exactly.

- [ ] **Step 4: Record verification evidence**

Append commands, pass/fail counts, skipped tests, and unverified production boundaries to `docs/IMPLEMENTATION_STATUS.md`.
