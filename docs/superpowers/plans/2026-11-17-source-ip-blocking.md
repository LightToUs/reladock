# Source IP Blocking Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add audited, per-rule IPv4 CIDR blocking at entrance nodes and make the existing source-flow retention schedule observable.

**Architecture:** PostgreSQL stores platform-admin block intents and versions them with the owning relay rule. Configuration schema 5 and a certificate-bound Agent capability carry the active CIDRs into a dedicated raw-prerouting nftables drop chain. The existing bounded source-flow prune operation remains the sole deletion path and is run by a testable recurring controller worker.

**Tech Stack:** Go 1.24+, PostgreSQL 17, static JavaScript, nftables, Node.js tests

**Spec:** `docs/superpowers/specs/2026-11-17-source-ip-blocking-design.md`

## Global Constraints

- Preserve tenant isolation, node/rule lock order, transactional Outbox, applied ACK, and same-transaction platform audit.
- Modify only RelaDock's owned nftables table; do not touch fake-ip, customer VPN configuration, target machines, or production servers.
- Reject IPv6 and `/0` in this IPv4-only increment; do not claim blocked until the corresponding desired version is applied.
- Source-flow cleanup must not delete billing, wallet, ledger, rule audit, or platform audit facts.
- Every production behavior follows RED → GREEN TDD and the final status document records verified and unverified boundaries.

## Review Focus

- A stale desired version or cross-tenant rule ID must not add/remove a block or enqueue configuration.
- An older Agent or a new certificate identity without the capability must never lease schema 5.
- CIDR normalization must not broaden an operator's input silently, and `/0` must be rejected.
- A block for one rule/port/protocol must not drop traffic for another rule sharing the node.
- Retention must keep open gaps and current manifests and must remain bounded/idempotent after partial runs.

---

### Task 1: Persistence and transactional block management

**Files:**
- Create: `internal/platform/migration43.sql`
- Create: `internal/platform/source_ip_blocks.go`
- Create: `internal/platform/source_ip_blocks_test.go`
- Modify: `internal/platform/store.go`
- Modify: migration/version regression tests that assert schema 42

**Interfaces:**
- Produces: `SourceIPBlock`, `SourceIPBlockInput`, `AdminSourceIPBlocks`, `AdminAddSourceIPBlock`, and `AdminRemoveSourceIPBlock`.
- Consumes: existing `adminTx`, node lock, relay-rule desired version, `enqueueConfig`, and `platformLog`.

- [ ] Write PostgreSQL integration tests for canonical `/32` and CIDR creation, exact duplicate replay, stale/cross-tenant rejection, `/0`/special-address/capacity rejection, removal, Outbox/version changes, and audit rollback.
- [ ] Run the focused tests and verify they fail because migration 43 and block APIs do not exist.
- [ ] Add migration 43 and minimal platform methods with a partial unique active-CIDR constraint and 256-active-row limit.
- [ ] Run focused platform tests and migration regressions; expect PASS.

### Task 2: Schema-5 delivery and Agent capability

**Files:**
- Modify: `internal/platform/delivery.go`
- Modify: `internal/platform/heartbeat.go`
- Modify: `internal/agent/heartbeat.go`
- Modify: `internal/web/agent.go`
- Modify: `cmd/reladock-agent/main.go`
- Test: delivery, heartbeat, Agent heartbeat, and HTTP Agent tests

**Interfaces:**
- Produces: `ConfigRule.BlockedSourceCIDRs`, schema 5 snapshots, and `AgentHeartbeat.SourceIPBlockSupported` negotiated through `X-RelaDock-Source-IP-Block`.
- Consumes: active block rows from Task 1.

- [ ] Write failing tests proving active blocks force schema 5, deterministic CIDR ordering, certificate capability reset, header transport, and lease refusal without current capability.
- [ ] Run focused tests and confirm the expected missing-field/schema failures.
- [ ] Implement the minimal snapshot, heartbeat header, persistence, identity reset, and ClaimConfig gate.
- [ ] Update all bounded schema consumers from 4 to 5 where appropriate and run focused tests; expect PASS.

### Task 3: nftables compilation and strict readback

**Files:**
- Modify: `internal/dataplane/compiler.go`
- Modify: `internal/dataplane/verify.go`
- Modify: `internal/dataplane/compiler_test.go`
- Modify: `internal/dataplane/verify_test.go`
- Modify: schema-aware dataplane/Agent tests as required

**Interfaces:**
- Consumes: schema-5 `BlockedSourceCIDRs` from Task 2.
- Produces: a raw-prerouting `source_block` chain and strict verifier support.

- [ ] Write failing compiler tests for rule-scoped IPv4 CIDR drops across TCP, UDP, both, and ranges, plus paused rules and malformed/duplicate CIDRs.
- [ ] Write failing readback tests for exact chain priority, comments, expressions and near-miss rejection.
- [ ] Implement minimal compilation and verification without touching foreign tables.
- [ ] Run `go test -race ./internal/dataplane ./internal/agent`; expect PASS.

### Task 4: Admin API and source-flow UI

**Files:**
- Modify: `internal/web/admin.go`
- Create: `internal/web/source_ip_blocks_http_test.go`
- Modify: `public/admin.html`
- Modify: `public/admin.js`
- Create: `public/source-ip-blocks.js`
- Create: `public/source-ip-blocks.test.js`
- Modify: `public/assets.go`, `public/assets_test.go`, `internal/web/static_assets_test.go`

**Interfaces:**
- Consumes: Task 1 block APIs and source-flow event rule/source fields.
- Produces: strict platform-admin JSON routes, manual CIDR add/list/remove, and one-click event `/32` blocking.

- [ ] Write failing HTTP tests for authentication, strict input, tenant scope, stale version, CRUD responses, and status codes.
- [ ] Write failing Node/static tests for the manual form, one-click action, requested/applied copy, and safe refresh-after-commit behavior.
- [ ] Implement routes, embedded module, dialog/table controls, and audit labels.
- [ ] Run focused Go/Node/static tests; expect PASS.

### Task 5: Retention worker observability and documentation

**Files:**
- Modify: `cmd/reladock/main.go`
- Create: `cmd/reladock/source_flow_retention_test.go`
- Modify: `internal/platform/source_flows.go`
- Modify: `internal/platform/source_flows_ingest_test.go`
- Modify: `internal/observability/*` only as needed for a worker result
- Modify: `README.md`, `docs/REQUIREMENTS.md`, `docs/ARCHITECTURE.md`, `docs/ROADMAP.md`, `docs/IMPLEMENTATION_STATUS.md`

**Interfaces:**
- Produces: immediate plus 30-second bounded retention scheduling and visible `retentionDays` in the admin response.
- Consumes: existing `PruneSourceFlows(ctx, 1000)` and worker observation mechanism.

- [ ] Write failing tests for immediate execution, recurring execution, cancellation, retention-days response, bounded deletion, open-gap/current-manifest preservation, and non-source-flow fact preservation.
- [ ] Extract the recurring worker and connect logs/metrics without changing the prune transaction's safety boundaries.
- [ ] Run focused tests, `npm run vet`, `git diff --check`, and the complete `npm test`; record exact results and skipped/unverified environment work.
- [ ] Update status documents and state that production deployment, live firewall changes, IPv6, and real traffic blocking were not performed.
