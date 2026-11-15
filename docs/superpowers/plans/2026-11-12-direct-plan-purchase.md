# Direct Plan Purchase Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a customer buy a plan in one flow by using wallet balance first, charging only the server-derived Stripe shortfall, and automatically purchasing after a verified callback.

**Architecture:** Extend payment orders with a constrained plan-purchase intent and tenant-readable result state. Refactor the existing atomic subscription purchase into transaction helpers so Stripe credit and the plan debit can commit together without reversing the established node-before-wallet lock order. Keep arbitrary wallet top-up and wallet-only purchase as compatible paths.

**Tech Stack:** Go 1.24, PostgreSQL migrations/transactions, static ES modules, Node test runner, real isolated PostgreSQL integration tests.

**Spec:** `docs/superpowers/specs/2026-11-12-direct-plan-purchase-design.md`

## Global Constraints

- USD amounts remain integer cents and wallet ledger entries remain immutable.
- The browser never supplies the external plan-payment amount.
- Stripe return URLs never prove payment, wallet credit, subscription purchase, Agent application, or end-to-end health.
- Existing arbitrary wallet top-up, refund/dispute review, and spending-freeze behavior remain compatible.
- Do not modify production services, payment credentials, Stripe state, Agent, or network rules.
- Preserve all pre-existing dirty-worktree changes. Do not create commits in this shared dirty checkout because several required documentation/UI files already contain unrelated user changes.

## Review Focus

- Two different browser keys for the same pending tenant/rule/price selection must recover one order and one Stripe Session.
- Manual purchase or auto-renewal racing a paid callback must not deadlock, overdraw the wallet, or open two entitlements.
- Business failure after verified payment must preserve the wallet credit, while audit/storage failure must roll the entire event back for Stripe retry.
- A 1–49 cent shortfall must charge Stripe's 50-cent minimum and preserve the exact excess as wallet balance.
- Lost browser storage, tenant changes, and stale responses must never reveal or apply another tenant's order.

---

### Task 1: Persist and quote plan-purchase orders

**Files:**
- Create: `internal/platform/migration35.sql`
- Modify: `internal/platform/store.go`
- Modify: `internal/platform/payments.go`
- Test: `internal/platform/payments_test.go`

**Interfaces:**
- Produces: extended `PaymentOrder` fields `Purpose`, `RuleID`, `PriceVersionID`, `PurchaseState`, `PurchaseError`, `SubscriptionID`.
- Produces: `CheckoutPlanPayment(ctx, who, key, PurchasePlanInput, provider) (PaymentOrder, error)`.
- Produces: tenant-scoped `PaymentOrder(ctx, who, id)` and `PlanPaymentOrders(ctx, who)` reads.
- Consumes: immutable `PlanPriceVersion`, wallet balance/freeze, line/node sale gates, existing provider session contract.

- [ ] **Step 1: Write failing migration/model tests**

Add tests that existing top-up orders backfill to `wallet-topup/none`, invalid purpose/field combinations fail, and the same pending tenant/rule-or-unbound/price selection cannot be inserted twice.

- [ ] **Step 2: Run the focused tests and verify RED**

Run: `node scripts/start.mjs test -race ./internal/platform -run 'TestPaymentMigration35|TestPlanPaymentCheckout' -count=1`

Expected: FAIL because schema v35, plan-order fields, and `CheckoutPlanPayment` do not exist.

- [ ] **Step 3: Add migration 35 and the plan-order model**

Register `migration35.sql`, set `SchemaVersion = 35`, extend scanning/JSON fields, and add constrained plan-order persistence plus bounded tenant reads. Use a partial expression unique index for one pending selection, including the unbound `NULL rule_id` case.

- [ ] **Step 4: Implement server-derived checkout quoting**

Validate current authorization, rule/plan ownership and sales gates, wallet freeze, and insufficient balance. Calculate `max(price-balance, payments.MinTopUpCents)`, persist the plan selection before the provider call, recover an existing pending selection under a different browser key, and keep provider idempotency tied to the local order ID.

- [ ] **Step 5: Run focused tests and verify GREEN**

Run: `node scripts/start.mjs test -race ./internal/platform -run 'TestPaymentMigration35|TestPlanPaymentCheckout|TestPaymentsCheckoutValidationTenantAndIdempotency' -count=1`

Expected: PASS with ordinary top-up regressions still green.

### Task 2: Commit Stripe credit and plan purchase atomically

**Files:**
- Modify: `internal/platform/subscriptions.go`
- Modify: `internal/platform/payments.go`
- Test: `internal/platform/payments_test.go`
- Test: `internal/platform/subscriptions_test.go`

**Interfaces:**
- Produces: transaction-scoped plan preparation/finalization helpers used by both `PurchasePlan` and paid plan orders.
- Consumes: the Task 1 `PaymentOrder` plan intent and deterministic order-derived subscription key.

- [ ] **Step 1: Write failing paid-callback tests**

Cover exact credit/debit ledger balances, one subscription under duplicate/concurrent paid events, 50-cent minimum excess, plan becoming unavailable, revoked authorization, concurrent wallet spending, frozen wallet, and forced audit/storage rollback.

- [ ] **Step 2: Run focused tests and verify RED**

Run: `node scripts/start.mjs test -race ./internal/platform -run 'TestPlanPaymentPaid|TestPlanPaymentBusinessFailure|TestPlanPaymentRollback|TestPurchasePlan' -count=1`

Expected: FAIL because paid callbacks only credit wallets and do not finalize stored plan intents.

- [ ] **Step 3: Refactor `PurchasePlan` without changing its public behavior**

Extract transaction preparation and finalization around the existing lock order. Manual wallet purchase continues to authorize the current member, enforce the same sales/active-period gates, debit once, enqueue configuration when bound, and audit in the same transaction.

- [ ] **Step 4: Finalize plan orders inside `ApplyStripeEvent`**

For plan orders, prepare node/rule/line/subscription locks before the wallet lock, then credit and purchase in one transaction. Convert only known business errors to bounded `purchase_state='failed'` results with retained credit; propagate unknown database/audit errors so the provider event retries.

- [ ] **Step 5: Handle provider terminal failures**

Accept exact `checkout.session.expired` and `checkout.session.async_payment_failed` events for matched pending orders, write terminal order/purchase states without credit, and release the pending-selection uniqueness guard.

- [ ] **Step 6: Run focused tests and verify GREEN**

Run: `node scripts/start.mjs test -race ./internal/platform -run 'TestPlanPayment|TestPayments|TestPurchasePlan' -count=1`

Expected: PASS with one credit/subscription under replay and no changes to ordinary refund/dispute semantics.

### Task 3: Expose strict plan checkout and order status APIs

**Files:**
- Modify: `internal/web/payments.go`
- Modify: `internal/web/server.go`
- Test: `internal/web/payments_http_test.go`

**Interfaces:**
- Produces: `POST /api/payments/plan-checkout` with `{ruleId,priceVersionId}`.
- Produces: `GET /api/payments/orders/:id` and bounded `GET /api/payments/orders?purpose=plan-purchase`.
- Consumes: Task 1 checkout/read methods and the existing authenticated same-origin payment handler.

- [ ] **Step 1: Write failing HTTP tests**

Cover strict JSON/query/method checks, server-derived amount, sufficient-wallet refusal, tenant isolation for exact/recent reads, duplicate-selection recovery, disabled connector behavior, and top-up endpoint compatibility.

- [ ] **Step 2: Run HTTP tests and verify RED**

Run: `node scripts/start.mjs test -race ./internal/web -run 'TestPaymentsHTTP|TestPlanPaymentHTTP' -count=1`

Expected: FAIL with missing routes/handlers.

- [ ] **Step 3: Implement handlers and routing**

Reuse the existing Host/origin/CSRF/authentication and provider-error mapping. Exact order and recent-order reads return only sanitized `PaymentOrder` fields and use current tenant authorization.

- [ ] **Step 4: Run HTTP tests and verify GREEN**

Run: `node scripts/start.mjs test -race ./internal/web -run 'TestPaymentsHTTP|TestPlanPaymentHTTP' -count=1`

Expected: PASS.

### Task 4: Make the enterprise checkout a direct-purchase flow

**Files:**
- Modify: `public/business-tools.js`
- Modify: `public/business-tools.test.js`
- Modify: `public/index.html`
- Modify: `internal/web/static_assets_test.go`

**Interfaces:**
- Consumes: Task 3 plan checkout and exact/recent order APIs.
- Produces: wallet contribution, server-compatible external amount/minimum excess, direct-purchase actions, and truthful post-return order reconciliation.

- [ ] **Step 1: Write failing frontend behavior tests**

Change expectations so insufficient balance submits plan identity rather than `amountCents`, shows wallet contribution plus Stripe shortfall/minimum excess, handles purchased/pending/failed/review states, and preserves tenant/logout/stale-response isolation.

- [ ] **Step 2: Run Node/static contract tests and verify RED**

Run: `node --test public/business-tools.test.js && node scripts/start.mjs test ./internal/web -run 'TestEnterpriseStaticAssets' -count=1`

Expected: FAIL on the old recharge-then-wallet-purchase copy and request body.

- [ ] **Step 3: Implement presentation and request changes**

Keep wallet-only purchase when sufficient. For insufficient balance call plan checkout, persist the local order ID, validate Stripe URLs, reconcile exact/recent local order state after return, clear successful entries, and never infer success from query parameters.

- [ ] **Step 4: Update markup and truthful copy**

Describe one purchase, the wallet contribution, external difference, minimum-charge remainder, retained-wallet fallback, and webhook truth boundary. Preserve unrelated current entry-address edits in `public/index.html`.

- [ ] **Step 5: Run frontend/static tests and verify GREEN**

Run: `node --test public/business-tools.test.js && node scripts/start.mjs test ./internal/web -run 'TestEnterpriseStaticAssets' -count=1`

Expected: PASS.

### Task 5: Document status and verify the workspace

**Files:**
- Modify: `README.md`
- Modify: `docs/REQUIREMENTS.md`
- Modify: `docs/ARCHITECTURE.md`
- Modify: `docs/ROADMAP.md`
- Modify: `docs/PAYMENTS.md`
- Modify: `docs/IMPLEMENTATION_STATUS.md`

**Interfaces:**
- Consumes: final code behavior and fresh verification results from Tasks 1–4.
- Produces: an accurate handoff that distinguishes local implementation from real Stripe/production acceptance.

- [ ] **Step 1: Update documentation without overwriting existing edits**

Record direct purchase, server-derived shortfall, automatic callback purchase, failure-to-wallet fallback, schema v35, retained top-up, and unchanged production/merchant boundaries. Note that this latest user direction supersedes the explicit second wallet-purchase step in older payment documents/designs.

- [ ] **Step 2: Run focused payment suites**

Run: `node scripts/start.mjs test -race ./internal/payments ./internal/platform ./internal/web -run 'TestPayment|TestPayments|TestPlanPayment|TestPurchasePlan|TestSignature|TestStripe|TestConfig' -count=1`

Expected: PASS.

- [ ] **Step 3: Run the complete suite**

Run: `npm test`

Expected: all Go/PostgreSQL and Node tests pass, aside from documented opt-in environment skips. If an existing flaky test fails, report it by exact name and reproduce it independently; do not present the full suite as passing.

- [ ] **Step 4: Run static verification**

Run: `npm run vet`

Expected: PASS.

Run: `git diff --check`

Expected: no whitespace errors.

- [ ] **Step 5: Re-read the spec and update implementation status with evidence**

Check every goal/non-goal, record commands/results/skips and the production boundary in `docs/IMPLEMENTATION_STATUS.md`, and list any unverified real Stripe, deployment, refund, or merchant behavior.
