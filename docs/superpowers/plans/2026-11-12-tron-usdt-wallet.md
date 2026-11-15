# TRON USDT Wallet Funding Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a default-disabled, receive-only TRON mainnet USDT funding channel that verifies solidified transfers to the configured collection address and credits the existing USD wallet exactly once.

**Architecture:** A focused `internal/tronpayments` package owns configuration and strict SolidityNode receipt verification. PostgreSQL v36 owns tenant orders, immutable chain receipts and idempotent wallet credits; authenticated web routes create orders and submit transaction hashes, while a bounded control-service worker retries confirmation and optionally invokes the existing plan purchase path. The browser only renders server states.

**Tech Stack:** Go 1.24+, PostgreSQL, existing Go HTTP/static JavaScript console, TRON SolidityNode HTTP API, real isolated PostgreSQL tests.

**Spec:** `docs/superpowers/specs/2026-11-12-tron-usdt-wallet-design.md`

## Global Constraints

- Preserve every existing dirty-worktree change and do not deploy, send funds, query balances, request keys, or modify production configuration.
- Support only TRON mainnet official TRC-20 USD₮ and collection address `TU4x4apSvfu1NLx6ee11rM5PjtVCBbWssw` when explicitly configured.
- Wallet and ledger amounts remain checked integer USD cents; USDT uses checked integer base units and the explicit v1 conversion `1 USDT = 1 USD`. Each active order reserves `cents * 10,000 + suffix`, where a globally unique `0..9,999` suffix below one cent distinguishes shared-address transfers and is not credited.
- No wallet credit before a successful solidified receipt and exact matching Transfer log.
- Chain receipt, wallet update, immutable ledger entry, order state and audit commit in one PostgreSQL transaction.
- Private keys, seed phrases, signing, sweep and refunds are out of scope.
- New behavior follows strict TDD: each production behavior is preceded by a test observed failing for the intended reason.

## Review Focus

- A valid tx hash containing a fake token contract or multiple matching logs must never credit; Task 1 verifier tests pin both cases.
- Concurrent workers and two orders claiming one tx hash must still create one receipt and one credit. An unverified claim must not reserve a public hash or block the rightful distinct-amount order; Task 3 PostgreSQL tests pin both races.
- A plan price supplied with a forged browser amount must use the immutable server price; Task 2 order tests pin the derivation.
- A receipt outside the order window or with a mismatched recipient/amount must enter review without changing the wallet; Tasks 1 and 3 pin the verifier and transaction boundaries.
- A plan purchase failure after credit must retain the credit and report truthful fallback state; Task 4 worker tests pin the two-transaction orchestration.

---

### Task 1: TRON Configuration, Address Validation, and Receipt Verifier

**Files:**
- Create: `internal/tronpayments/config.go`
- Create: `internal/tronpayments/address.go`
- Create: `internal/tronpayments/client.go`
- Create: `internal/tronpayments/client_test.go`
- Modify: `cmd/reladock/main.go`

**Interfaces:**
- Produces: `ConfigFromEnv() (Config, bool, error)`, `ValidateMainnetAddress(string) error`, `NewClient(Config) (*Client, error)`, and `(*Client).Verify(context.Context, txHash string, expectedBaseUnits int64) (Receipt, error)` with typed retryable/not-found/invalid receipt results.
- Consumes: standard `net/http`, `crypto/sha256`, checked `math/big` parsing; no new dependency.

- [ ] **Step 1: Write failing configuration and address tests** covering all-absent disabled, partial fail-closed, HTTPS-only RPC, optional API key secrecy, the configured valid address, bad checksum and non-mainnet prefix.
- [ ] **Step 2: Run `go test ./internal/tronpayments -run 'TestConfig|TestAddress' -count=1`** and confirm failure because the package/interfaces do not exist.
- [ ] **Step 3: Implement the minimal configuration and Base58Check address validation interfaces** without logging or returning secrets.
- [ ] **Step 4: Run the focused tests** and confirm they pass.
- [ ] **Step 5: Write failing verifier tests** with a real HTTP test server for solidified success, empty not-yet-solidified receipt, failed execution, fake contract, wrong recipient, wrong amount, ambiguous logs, malformed/oversized JSON, redirect and timeout.
- [ ] **Step 6: Run `go test ./internal/tronpayments -run TestClient -count=1`** and confirm verifier behavior is missing.
- [ ] **Step 7: Implement `Client.Verify`** using the fixed SolidityNode path, disabled redirects/proxies, bounded body, strict hashes/topics/amounts, and typed results.
- [ ] **Step 8: Run `go test ./internal/tronpayments -count=1`** and confirm all package tests pass.
- [ ] **Step 9: Commit only Task 1 files** with message `feat: add strict TRON USDT receipt verifier` if repository permissions and the user's dirty worktree allow an isolated commit; otherwise record a ledger ruling and leave the changes uncommitted.

### Task 2: v36 Orders and Server-derived Quotes

**Files:**
- Create: `internal/platform/migration36.sql`
- Create: `internal/platform/tron_payments.go`
- Create: `internal/platform/tron_payments_test.go`
- Modify: `internal/platform/store.go`
- Modify: `internal/platform/store_test.go`
- Modify: `internal/platform/migration_regression_test.go`

**Interfaces:**
- Consumes: `platform.Identity`, immutable plan price rows, existing tenant write authorization and audit helpers.
- Produces: `CreateTronPaymentOrder`, `ClaimTronPaymentOrder`, `GetTronPaymentOrder`, `ListTronPaymentOrders`, `ClaimPendingTronPaymentConfirmation`, and v36 tables/types.

- [ ] **Step 1: Write failing migration tests** asserting schema v36, repeatability, immutable receipt triggers, uniqueness and preservation of v34 source-flow data and the reserved in-progress v35 Stripe migration.
- [ ] **Step 2: Run the focused store/migration tests** and confirm the missing v36 schema fails.
- [ ] **Step 3: Add and register migration v36** without altering the user's v34 migration or taking over the reserved v35 payment work.
- [ ] **Step 4: Run the focused migration tests** and confirm they pass.
- [ ] **Step 5: Write failing order tests** for wallet top-up idempotency, changed-request conflict, globally unique active exact quotes within the requested cent bucket, server-derived plan price, optional owned rule, cross-tenant rejection, 30-minute transfer expiry plus bounded late-claim grace, and strict tx hash syntax without pre-verification hash reservation.
- [ ] **Step 6: Run `go test ./internal/platform -run 'TestTronPaymentOrder' -count=1`** and confirm the new store methods are missing.
- [ ] **Step 7: Implement the minimal order and worker-claim store methods** with tenant authorization, locks, bounded queries and no wallet mutation.
- [ ] **Step 8: Run the focused order tests** and confirm they pass.
- [ ] **Step 9: Commit Task 2 files** with message `feat: persist idempotent TRON funding orders`, subject to the same isolated-commit ruling.

### Task 3: Idempotent Solidified Credit Transaction

**Files:**
- Modify: `internal/platform/tron_payments.go`
- Modify: `internal/platform/tron_payments_test.go`
- Modify: `internal/platform/migration36.sql` only if a test proves a missing database invariant before Task 2 is committed.

**Interfaces:**
- Consumes: `tronpayments.Receipt`, claimed order lease/token, existing `billing_wallets`, `billing_ledger`, immutable audit helpers.
- Produces: `ApplyTronPaymentReceipt(ctx, orderID, leaseToken string, receipt tronpayments.Receipt) (TronPaymentOrder, error)` and `RetryTronPaymentConfirmation`.

- [ ] **Step 1: Write failing PostgreSQL tests** for exact credit, duplicate replay, concurrent workers, cross-tenant claims sharing a public tx hash without pre-verification denial-of-service, verified receipt uniqueness, wrong amount/recipient/contract/time to review, wallet overflow, frozen-wallet credit behavior and audit failure rollback.
- [ ] **Step 2: Run `go test -race ./internal/platform -run 'TestTronPaymentCredit' -count=1`** and confirm missing application behavior.
- [ ] **Step 3: Implement the wallet-locked transaction** that inserts the immutable receipt, appends `payment-credit`, advances the order and audit atomically.
- [ ] **Step 4: Implement retry/review reporting** with lease fencing and bounded non-secret errors.
- [ ] **Step 5: Run the focused race tests** and confirm they pass.
- [ ] **Step 6: Commit Task 3 files** with message `feat: credit confirmed TRON payments exactly once`, subject to the isolated-commit ruling.

### Task 4: Authenticated API and Confirmation Worker

**Files:**
- Create: `internal/web/tron_payments.go`
- Create: `internal/web/tron_payments_http_test.go`
- Create: `cmd/reladock/tron_payments.go`
- Create: `cmd/reladock/tron_payments_test.go`
- Modify: `internal/web/server.go`
- Modify: `cmd/reladock/main.go`
- Modify: `internal/observability/metrics.go`
- Modify: `internal/observability/metrics_test.go`

**Interfaces:**
- Consumes: Task 1 client, Tasks 2–3 store methods, existing session/API-key and CSRF boundaries, `PurchasePlan`.
- Produces: authenticated TRON order routes, aggregate payment status, and bounded retry worker with optional idempotent plan-purchase attempt.

- [ ] **Step 1: Write failing HTTP tests** for disabled status, create/replay, strict body/query/method, read/write API key scope, cross-tenant order isolation, malformed/different tx claim and non-secret responses.
- [ ] **Step 2: Run `go test ./internal/web -run TestTronPaymentsHTTP -count=1`** and confirm routes are absent.
- [ ] **Step 3: Implement the handler and route wiring** while preserving existing Stripe routes and aggregate compatibility.
- [ ] **Step 4: Run the focused HTTP tests** and confirm they pass.
- [ ] **Step 5: Write failing worker tests** for not-yet-solidified retry, RPC failure retry, exact receipt apply, lease loss, plan purchase success and plan purchase failure retaining credited balance.
- [ ] **Step 6: Run `go test ./cmd/reladock -run TestTronPaymentWorker -count=1`** and confirm the worker is absent.
- [ ] **Step 7: Implement the worker, startup configuration and fixed observability label** without logging tx payloads, API keys or customer identifiers.
- [ ] **Step 8: Run the focused worker, web and metrics tests** and confirm they pass.
- [ ] **Step 9: Commit Task 4 files** with message `feat: expose and process TRON funding orders`, subject to the isolated-commit ruling.

### Task 5: Enterprise Checkout UI

**Files:**
- Modify: `public/index.html`
- Modify: `public/business-tools.js`
- Modify: `public/business-tools.test.js`
- Modify: `public/app.css`
- Modify: `internal/web/static_assets_test.go`

**Interfaces:**
- Consumes: Task 4 status/order/claim APIs and existing checkout selection/wallet refresh logic.
- Produces: a truthful TRON payment method, invoice panel and tx-hash submission flow.

- [ ] **Step 1: Write failing JavaScript behavior tests** for method availability, invoice rendering, exact address/amount/network, pending/confirming/review/credited copy, tx submission and stale tenant/session response invalidation.
- [ ] **Step 2: Run the focused Node tests** and confirm the TRON presentation/controller is absent.
- [ ] **Step 3: Implement the minimal UI controller and semantic HTML** reusing existing checkout state boundaries and no browser-derived credit state.
- [ ] **Step 4: Run the focused Node and static-asset tests** and confirm they pass.
- [ ] **Step 5: Inspect desktop and mobile checkout in a real browser** with deterministic fixture data; confirm no horizontal overflow, address wrapping, keyboard reachability and truthful state copy.
- [ ] **Step 6: Commit Task 5 files** with message `feat: add TRON USDT checkout flow`, subject to the isolated-commit ruling.

### Task 6: Documentation and Full Verification

**Files:**
- Modify: `README.md`
- Modify: `docs/PAYMENTS.md`
- Modify: `docs/ARCHITECTURE.md`
- Modify: `docs/REQUIREMENTS.md`
- Modify: `docs/ROADMAP.md`
- Modify: `docs/IMPLEMENTATION_STATUS.md`
- Modify: deployment environment template selected by the current repository layout.

**Interfaces:**
- Consumes: verified implementation and test evidence from Tasks 1–5.
- Produces: truthful configuration, operations, security and unverified-production boundaries for the next maintainer.

- [ ] **Step 1: Document the exact optional environment variables and collection address**, stating no key custody, no sending/sweep/refund, 1:1 quote policy and sandbox-only verification.
- [ ] **Step 2: Update required project status documents** without overwriting unrelated user edits, clearly distinguishing code implementation, local substitute verification and missing TRON mainnet/compliance/production acceptance.
- [ ] **Step 3: Run focused package tests with race detection** for TRON verifier, platform credit, web routes and worker.
- [ ] **Step 4: Run `npm test` against the configured real local PostgreSQL test database** and record counts, failures and skips.
- [ ] **Step 5: Run `npm run vet` and `git diff --check`** and read complete output.
- [ ] **Step 6: Run release/config validation appropriate to the changed startup path** without deploying or contacting mainnet.
- [ ] **Step 7: Re-read the spec and verify every requirement against code/tests/docs**, recording any unmet item as not implemented rather than claiming completion.
- [ ] **Step 8: Commit Task 6 files** with message `docs: document TRON USDT funding boundary`, subject to the isolated-commit ruling.
