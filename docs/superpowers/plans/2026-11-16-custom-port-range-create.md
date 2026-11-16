# Custom Port Range Creation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Allow dedicated-tenant customers to create one relay rule that reserves and forwards a chosen single ingress port or continuous ingress range.

**Architecture:** PostgreSQL remains the allocation authority by storing every protocol/port endpoint while the rule stores range bounds. Configuration schema 4 carries the bounds and exact allocations; capable Agents validate the bijection, compile interval forwarding and one rule-level shaping class, while older Agents are not leased range snapshots. The browser enables range inputs only for eligible purchased dedicated-tenant packages.

**Tech Stack:** Go 1.24+, PostgreSQL 17, nftables/tc, Node.js 20+ browser modules, Docker Compose tests.

**Spec:** `docs/superpowers/specs/2026-11-16-custom-port-range-create-design.md`

## Global Constraints

- Custom ranges are creation-only and available only to the owning tenant of `dedicated-tenant` nodes.
- Omitted ports preserve automatic single-port allocation; defaults remain `20000-60000` per TCP/UDP pool.
- A rule contains at most 4096 numeric ports and a node at most 10000 active protocol/port endpoints.
- Exact endpoint uniqueness, idempotency, audit, subscription binding and Outbox mutation stay in one transaction.
- Range delivery requires schema 4 and an authenticated latest-heartbeat capability; no downgrade to the start port.
- One range remains one billing, quota, counter and shaping identity.

## Review Focus

- End-only, zero, reversed, oversized and pool-hole inputs must fail without writes (Task 2 tests).
- `both` must reserve and validate every TCP and UDP endpoint atomically (Tasks 2 and 3 tests).
- An old or rotated Agent identity must not lease a range snapshot (Task 3 tests).
- Range expiry/quota/source-flow output must cover every port without creating multiple billing identities (Task 4 tests).
- Switching package/node/protocol in the dialog must reset stale range values and never submit hidden custom ports (Task 5 tests).

---

### Task 1: Schema 42 range bounds and customer port pools

**Files:**
- Create: `internal/platform/migration42.sql`
- Modify: `internal/platform/store.go`, `internal/platform/schema.sql`
- Test: `internal/platform/port_ranges_test.go`, `internal/platform/migration_regression_test.go`

**Interfaces:**
- Produces: `relay_rules.listen_port_end`, `node_customer_port_pools`, `nodes.agent_port_range_supported`, schema version 42.
- Consumes: existing exact `endpoint_allocations` primary key and node identity reset triggers.

- [ ] Write migration tests for backfill, default TCP/UDP pools, new-node defaults, widened exact-port checks and capability reset; run them and observe missing schema 42.
- [ ] Add migration 42, embed/order it in `Store.Migrate`, and update bootstrap schema with equivalent final state.
- [ ] Run migration and regression tests; commit the schema task.

### Task 2: Transactional custom range reservation

**Files:**
- Modify: `internal/platform/rules.go`, `internal/platform/admin_rules.go`, `internal/platform/probes.go`
- Test: `internal/platform/port_ranges_test.go`, `internal/platform/rules_test.go`, `internal/web/rules_http_test.go`

**Interfaces:**
- Consumes: Task 1 range columns and pools.
- Produces: `RuleInput.ListenPort`, `RuleInput.ListenPortEnd`, `Rule.ListenPortEnd`, exact allocation rows for every requested protocol/port.

- [ ] Add failing tests for automatic compatibility, `40000-41000/udp`, `both`, pool/tenant/mode/size/node-total rejection, overlap rollback and idempotency hash conflicts.
- [ ] Implement validation, pool coverage, endpoint-count checks, atomic rule/allocation insertion and range-aware scans/lists/probes.
- [ ] Run platform and HTTP rule tests; commit the reservation task.

### Task 3: Schema 4 delivery and capability gate

**Files:**
- Modify: `internal/platform/delivery.go`, `internal/platform/heartbeat.go`, `internal/platform/bootstrap.go`, `internal/web/agent.go`, `internal/agent/heartbeat.go`, `internal/agent/validate.go`, `cmd/reladock-agent/main.go`
- Test: `internal/platform/delivery_test.go`, `internal/platform/heartbeat_test.go`, `internal/web/agent_test.go`, `internal/agent/validate_test.go`, `internal/agent/forced_rotation_test.go`

**Interfaces:**
- Consumes: range rule/allocation facts from Task 2.
- Produces: `ConfigRule.ListenPortEnd`, schema 4 snapshots, `X-RelaDock-Port-Range: 1`, persisted capability and lease withholding for unsupported identities.

- [ ] Add failing tests for exact range/allocation bijection, capability heartbeat/reset and unsupported lease withholding.
- [ ] Implement schema 4 creation/validation and authenticated capability propagation without changing old heartbeat JSON.
- [ ] Run delivery, heartbeat, Agent HTTP and validation tests; commit the wire task.

### Task 4: Range-aware nftables, shaping and source attribution

**Files:**
- Modify: `internal/dataplane/compiler.go`, `internal/shaping/compiler.go`, `internal/platform/source_flows.go`, `internal/platform/probes.go`
- Test: `internal/dataplane/compiler_test.go`, `internal/dataplane/integration_test.go`, `internal/shaping/compiler_test.go`, `internal/platform/source_flows_test.go`

**Interfaces:**
- Consumes: schema 4 bounds and exact allocations from Task 3.
- Produces: interval DNAT/SNAT/expiry and tc marking using one rule identity; precise allocation/source-flow tuples.

- [ ] Add failing compiler tests for interval forwarding, one class/counter/quota identity, allocation gaps and exact relay-port attribution.
- [ ] Implement inclusive range validation and interval nft/tc expressions while retaining exact guards/manifests.
- [ ] Run dataplane, shaping and source-flow tests; commit the data-plane task.

### Task 5: Eligible console controls and range presentation

**Files:**
- Modify: `public/index.html`, `public/rule-editor.js`, `public/app.js`, `public/app.css`
- Test: `test/rule-editor.test.mjs`, `test/rule-submission.test.mjs`, `internal/web/static_assets_test.go`

**Interfaces:**
- Consumes: dedicated package `resourceMode` and range API fields.
- Produces: create-only `auto|single|range` mode, safe payload normalization and `start-end` display.

- [ ] Add failing browser tests for eligibility, mode transitions, stale-value clearing, payloads and presentation.
- [ ] Enable and style the controls, submit optional numeric bounds only in create mode, and keep them locked during edit.
- [ ] Run browser/static tests; commit the console task.

### Task 6: Documentation, full verification and production release

**Files:**
- Modify: `README.md`, `docs/REQUIREMENTS.md`, `docs/ARCHITECTURE.md`, `docs/ROADMAP.md`, `docs/IMPLEMENTATION_STATUS.md`, `docs/DEPLOYMENT.md`

**Interfaces:**
- Consumes: Tasks 1-5 and current deployment/runbook controls.
- Produces: explicit implemented/undeployed/Agent-upgrade/Linux/VPS validation boundaries and recoverable production rollout evidence.

- [ ] Update required documents and implementation status without claiming Linux/VPS/HY2 production readiness.
- [ ] Run focused tests, `GOFLAGS=-timeout=20m npm test`, `npm run vet`, `git diff --check`, Linux amd64 release build and configuration check.
- [ ] Request whole-branch review and fix every Critical/Important finding with RED→GREEN evidence.
- [ ] Back up production DB/binary/config, deploy schema-compatible control binary, verify schema 42/service/HTTP/auth/assets/logs, publish the compatible Agent release without silently upgrading nodes, and record hashes/rollback boundary.
