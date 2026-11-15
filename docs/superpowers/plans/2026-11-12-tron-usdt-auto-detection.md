# TRON USDT Automatic Detection and QR Payment Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Automatically discover solidified official-USDT transfers for open TRON orders and add a locally generated address QR plus server-time countdown to the invoice.

**Architecture:** A SolidityNode client walks solidified TransactionInfo blocks, while a leased PostgreSQL v38 cursor guarantees sequential retryable progress. Discovery only attaches a candidate TxID to a uniquely matched order; the existing verifier and atomic credit transaction remain the final settlement path. An authenticated QR endpoint renders the raw Base58Check collection address, and the browser presents an authoritative countdown with manual TxID recovery collapsed by default.

**Tech Stack:** Go 1.24+, PostgreSQL/pgx, standard HTTP client, `github.com/skip2/go-qrcode`, vanilla ES modules, Node test runner.

**Spec:** `docs/superpowers/specs/2026-11-12-tron-usdt-auto-detection-design.md`

## Global Constraints

- Preserve all unrelated dirty-worktree changes and do not send funds or request a wallet private key.
- Scan only `/walletsolidity` solidified state; never credit from an unconfirmed event or balance delta.
- Existing `ApplyTronPaymentReceipt` remains the sole wallet/ledger/receipt settlement transaction.
- QR payload is the raw validated collection address, generated locally and returned only through an authenticated tenant-scoped order route.
- Countdown uses server time, reaches zero without going negative, and cannot extend the database transfer window.
- Manual TxID claim remains available as a bounded recovery path.

## Review Focus

- A malformed/failed block response or ambiguous transaction must hold the cursor for retry and never skip the height; Task 1 and Task 2 tests pin this.
- Restarted or concurrent scanners must not claim two heights or advance a stale lease; Task 2 PostgreSQL race tests pin this.
- A discovered transfer with the same amount but outside the original order window must not attach or credit; Task 2 tests pin this.
- QR access must remain authenticated and cross-tenant order IDs must return 404; Task 4 HTTP tests pin this.
- Browser clock skew, tab delay and countdown expiry must not display extra payable time or leave the QR active; Task 4 Node tests pin this.

---

### Task 1: Solidified Block Receipt Scanner

**Files:**
- Modify: `internal/tronpayments/client.go`
- Modify: `internal/tronpayments/client_test.go`

**Interfaces:**
- Produces: `LatestSolidBlock(ctx context.Context) (int64, error)` and `ScanSolidBlock(ctx context.Context, height int64) ([]Receipt, error)`.
- Reuses: `Receipt`, official contract/address parsing, provider error classification and the hardened no-proxy/no-redirect HTTP client.

- [ ] **Step 1: Write failing scanner tests** for latest height, a single exact official-USDT candidate, unrelated logs, ambiguous same-transaction transfers, failed receipts, malformed JSON, wrong returned height, response limit, API-key header and provider failures.
- [ ] **Step 2: Run `go test ./internal/tronpayments -run 'TestClient(Latest|Scan)' -count=1`** and observe missing methods.
- [ ] **Step 3: Implement the two methods** using `/walletsolidity/getnowblock` and `/walletsolidity/gettransactioninfobyblocknum`, with an 8 MiB bounded block response and strict candidate parsing.
- [ ] **Step 4: Run `go test ./internal/tronpayments -count=1`** and confirm all verifier/scanner tests pass.

### Task 2: Persistent Scan Cursor and Automatic Order Match

**Files:**
- Create: `internal/platform/migration38.sql`
- Modify: `internal/platform/store.go`
- Modify: `internal/platform/store_test.go`
- Modify: `internal/platform/migration_regression_test.go`
- Modify: `internal/platform/tron_payments.go`
- Modify: `internal/platform/tron_payments_test.go`

**Interfaces:**
- Produces: `ClaimNextTronScanBlock(ctx, latestHeight, initialLookback, leaseDuration) (*TronScanLease, error)`, `CompleteTronScanBlock(ctx, lease TronScanLease) error`, `RetryTronScanBlock(ctx, lease TronScanLease, reason string, delay time.Duration) error`, and `MatchDiscoveredTronPayment(ctx context.Context, receipt tronpayments.Receipt) (*TronPaymentOrder, error)`.
- `TronScanLease` contains `Height int64` and `Token string`.

- [ ] **Step 1: Write failing v38 migration tests** for the cursor table, tuple uniqueness, lease invariants, bounded errors and repeatable migration.
- [ ] **Step 2: Run focused migration tests** and confirm schema v38 is absent.
- [ ] **Step 3: Register migration v38** after v37 without modifying earlier migration files.
- [ ] **Step 4: Write failing real-PostgreSQL tests** for initial 1,200-block lookback, sequential completion, retry of the same block, stale lease fencing, two-scanner race, automatic exact-order match, out-of-window rejection, duplicate discovery, manual/automatic claim race and audit rollback.
- [ ] **Step 5: Implement cursor and matching methods** with database time, row/advisory locks, bounded errors and no wallet mutation.
- [ ] **Step 6: Run `go test -race ./internal/platform -run 'TestTronScan|TestTronAuto' -count=1`** and confirm pass.

### Task 3: Combined Discovery and Confirmation Worker

**Files:**
- Modify: `cmd/reladock/tron_payments.go`
- Modify: `cmd/reladock/tron_payments_test.go`
- Modify: `internal/observability/metrics.go`

**Interfaces:**
- Consumes: Task 1 scanner and Task 2 store interfaces.
- Produces: `processTronScan(ctx, store, scanner, maxBlocks) error`; the existing loop runs confirmation first and then at most 20 sequential scan blocks per cycle.

- [ ] **Step 1: Write failing worker tests** for discovery scheduling, empty blocks, candidate matching, provider retry without cursor advance, bounded batch catch-up, confirmation priority and context cancellation.
- [ ] **Step 2: Run `go test ./cmd/reladock -run 'TestTron.*(Scan|Worker)' -count=1`** and observe missing scan behavior.
- [ ] **Step 3: Implement the combined worker** with 1,200-block initial lookback, 20-block batch cap, existing error metrics and no secret/error-body logging.
- [ ] **Step 4: Run the focused worker tests** and confirm pass.

### Task 4: Authenticated QR and Server-Time Countdown UI

**Files:**
- Modify: `go.mod`
- Modify: `go.sum`
- Modify: `internal/web/tron_payments.go`
- Modify: `internal/web/tron_payments_http_test.go`
- Modify: `internal/platform/tron_payments.go`
- Modify: `public/index.html`
- Modify: `public/app.css`
- Modify: `public/business-tools.js`
- Modify: `public/business-tools.test.js`
- Modify: `internal/web/static_assets_test.go`

**Interfaces:**
- Produces: authenticated `GET /api/payments/tron/orders/{id}/qr` returning `image/png`, order JSON `serverTime`, `tronExpiryPresentation(...)`, and a locally managed one-second invoice countdown.

- [ ] **Step 1: Add `github.com/skip2/go-qrcode`** and write failing unit/HTTP tests for deterministic PNG generation, no-store headers, tenant isolation, invalid/expired orders and raw-address-only payload.
- [ ] **Step 2: Write failing Node tests** for automatic-detection wording, exact amount/address copy values, server-time clock-skew handling, `MM:SS`, zero clamping, QR disabling and manual recovery visibility.
- [ ] **Step 3: Implement the QR endpoint and `serverTime` presentation** without exposing provider configuration or API keys.
- [ ] **Step 4: Implement invoice markup/styles/controller** with address QR, exact amount copy, per-second countdown, expiry refresh and collapsed manual TxID recovery.
- [ ] **Step 5: Run `go test ./internal/web -run 'TestTronPaymentsHTTP|TestStatic' -count=1` and `node --test public/business-tools.test.js`** and confirm pass.

### Task 5: Documentation, Verification and Controlled Production Release

**Files:**
- Modify: `README.md`
- Modify: `docs/ARCHITECTURE.md`
- Modify: `docs/PAYMENTS.md`
- Modify: `docs/REQUIREMENTS.md`
- Modify: `docs/ROADMAP.md`
- Modify: `docs/IMPLEMENTATION_STATUS.md`

**Interfaces:**
- Documents: automatic scan/cursor behavior, raw-address QR boundary, countdown semantics, manual recovery and incomplete real-funds acceptance.

- [ ] **Step 1: Update documentation** and preserve the distinction between deployed code and a completed real-USDT acceptance.
- [ ] **Step 2: Run focused race tests, full Node tests, `npm run vet`, `git diff --check`, and build/check the Linux release artifact.**
- [ ] **Step 3: Fresh Critical/Important review** of cursor loss/duplication, event parsing, tenant isolation, countdown behavior and QR payload.
- [ ] **Step 4: Back up production, deploy v38, verify schema/service/external UI/SolidityNode scan methods, retain rollback artifacts, and do not send funds.**
- [ ] **Step 5: Record exact production evidence and remaining small-value real-transfer acceptance boundary in `IMPLEMENTATION_STATUS.md`.**
