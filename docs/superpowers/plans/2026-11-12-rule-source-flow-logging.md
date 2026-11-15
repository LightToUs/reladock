# Rule Source Flow Logging Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add platform-admin-controlled, per-rule source IP/port flow logging with truthful applied state, durable Agent buffering, bounded storage, explicit gaps, and no packet-payload collection.

**Architecture:** Migration v34 adds requested/applied rule state, Agent capability/health, immutable applied manifests, append-only source events, and gap intervals. A Linux apply Agent subscribes to conntrack `NEW` netlink events, attributes the original pre-SNAT tuple through its durable last-good schema-3 manifest, persists bounded events/gaps in a source-flow WAL, and uploads idempotent batches through mTLS. The platform exposes bounded admin-only queries and a per-rule toggle while tenants see status only.

**Tech Stack:** Go 1.24, PostgreSQL/pgx, Linux conntrack netlink through `github.com/florianl/go-conntrack` v0.7.0, nftables integration tests, static HTML/CSS/JavaScript, Node test runner.

**Spec:** `docs/superpowers/specs/2026-11-12-rule-source-flow-logging-design.md`

## Global Constraints

- Existing rules and new rules default source logging to disabled.
- Only platform administrators can toggle or query raw source tuples; tenant users receive status only.
- Never record payload, URL, DNS/SNI, or full PCAP.
- Source-flow logging is optional and fail-open for forwarding; every known loss is an explicit persisted gap.
- Billing/metering WAL, reports, quotas, and ledger behavior remain unchanged and isolated.
- Enabling requires an authenticated apply-mode Agent capability and explicit `SOURCE_FLOW_RETENTION_DAYS` in the inclusive range 30–365.
- Do not deploy, enable production rules, modify production networks, or use customer traffic during this implementation.
- Preserve the existing dirty worktree and do not include unrelated files or changes in checkpoints.

## Review Focus

- Rapid enable/disable/enable before ACK must preserve the actual applied boolean/generation and never infer it from the latest request.
- Delayed WAL events after edit, disable, or retirement must validate against the immutable historical applied manifest, not current rule state.
- Netlink loss, unclean restart, queue/WAL full, and upload backlog must open visible gaps without blocking forwarding or billing WAL.
- Five-tuple reuse must remain distinct while duplicate upload/retry of the same Agent event ID stays idempotent.
- Retention cleanup and restored backups must not expose expired source events or delete billing/audit facts.

---

### Task 1: Migration v34 and source-flow domain model

**Files:**
- Create: `internal/platform/migration34.sql`
- Create: `internal/platform/source_flows.go`
- Modify: `internal/platform/store.go`
- Modify: `internal/platform/schema.sql`
- Modify: `internal/platform/rules.go`
- Modify: `internal/platform/admin_rules.go`
- Modify: `internal/platform/migration_regression_test.go`
- Test: `internal/platform/source_flows_test.go`

**Interfaces:**
- Produces: `SourceFlowEvent`, `SourceFlowGap`, `AppliedSourceFlowRule`, `SourceFlowStatus`, and rule fields `SourceLoggingEnabled`, `SourceLoggingAppliedEnabled`, `SourceLoggingAppliedVersion`, `SourceLoggingAppliedGeneration`.
- Produces: v34 tables/constraints/indexes named in the spec and `nodes.agent_source_flow_supported`.

- [ ] **Step 1: Write failing migration/domain tests**

Add tests proving defaults are false/zero, event and gap constraints reject invalid tuples/reasons, duplicate `(node_id,event_id)` is unique, source indexes exist, certificate identity replacement clears capability, v33 data survives v34, and rerunning migrations preserves rows.

- [ ] **Step 2: Run tests to verify RED**

Run: `go test -race ./internal/platform -run 'TestMigration34|TestSourceFlowSchema' -count=1`

Expected: FAIL because migration 34, fields, and tables do not exist.

- [ ] **Step 3: Implement the v34 schema and strict Go value types**

Embed/apply migration 34 after migration 33, update the base schema, add exact check constraints and indexes, extend every rule scan consistently, and keep event/gap models free of payload fields.

- [ ] **Step 4: Run focused tests to verify GREEN**

Run: `go test -race ./internal/platform -run 'TestMigration34|TestSourceFlowSchema' -count=1`

Expected: PASS.

- [ ] **Step 5: Checkpoint only task-owned paths**

Run: `git diff --check -- internal/platform`

### Task 2: Admin toggle, Agent capability, and schema-3 snapshot delivery

**Files:**
- Modify: `internal/platform/heartbeat.go`
- Modify: `internal/platform/certificate_rotation.go`
- Modify: `internal/platform/admin_rules.go`
- Modify: `internal/platform/delivery.go`
- Modify: `internal/web/admin.go`
- Modify: `internal/web/agent.go`
- Test: `internal/platform/source_logging_toggle_test.go`
- Test: `internal/web/source_logging_http_test.go`
- Modify: `internal/platform/heartbeat_test.go`

**Interfaces:**
- Produces: `Store.AdminSetRuleSourceLogging(ctx, who, tenantID, ruleID string, desiredVersion int64, enabled bool) (Rule, error)`.
- Produces: `PATCH /api/admin/rules/:id/source-logging` with exact body `{tenantId,desiredVersion,enabled}`.
- Produces: heartbeat capability header `X-RelaDock-Source-Flow: 1` persisted only for the authenticated certificate.
- Produces: schema-3 `ConfigRule.SourceLoggingEnabled` snapshots.

- [ ] **Step 1: Write failing authorization/concurrency tests**

Cover platform-admin-only access, strict JSON, invalid/stale versions, no-op replay, audit rollback, node lock concurrency, missing retention, validation/offline/old Agent capability rejection, certificate replacement clearing capability, and enabled snapshots using schema 3 while disabled-only snapshots remain compatible.

- [ ] **Step 2: Run tests to verify RED**

Run: `go test -race ./internal/platform ./internal/web -run 'TestAdmin.*SourceLogging|TestSourceLoggingHTTP|TestSourceFlowCapability|TestSourceLoggingSnapshot' -count=1`

Expected: FAIL because the mutation, capability, endpoint, and config field do not exist.

- [ ] **Step 3: Implement the transaction and strict HTTP boundary**

Use platform authorization, tenant/node/rule locks, desired-version compare, rule-version increment, complete snapshot enqueue, and same-transaction `rule.source-logging.enable|disable` audit. Read retention availability from Store configuration; do not infer capability from version strings.

- [ ] **Step 4: Implement heartbeat capability and config schema 3**

Accept the header only on authenticated Agent heartbeat, clear it on certificate identity change, include `sourceLoggingEnabled` in rules, and emit schema 3 only when at least one rule requests logging.

- [ ] **Step 5: Run focused tests to verify GREEN**

Run: `go test -race ./internal/platform ./internal/web -run 'TestAdmin.*SourceLogging|TestSourceLoggingHTTP|TestSourceFlowCapability|TestSourceLoggingSnapshot' -count=1`

Expected: PASS.

- [ ] **Step 6: Checkpoint only task-owned paths**

Run: `git diff --check -- internal/platform internal/web`

### Task 3: Exact applied source-flow manifest and applied-state ACK

**Files:**
- Create: `internal/sourceflow/manifest.go`
- Test: `internal/sourceflow/manifest_test.go`
- Modify: `internal/dataplane/compiler.go`
- Modify: `internal/dataplane/verify.go`
- Modify: `internal/platform/delivery.go`
- Modify: `internal/agent/client.go`
- Modify: `internal/agent/runtime_state.go`
- Test: `internal/dataplane/source_flow_manifest_test.go`
- Test: `internal/platform/source_flow_application_test.go`
- Test: `internal/agent/client_apply_test.go`

**Interfaces:**
- Produces: canonical `sourceflow.Manifest` containing node/generation and sorted `sourceflow.AppliedRule` allocation/rule/version/logging/resolved-target tuples plus SHA256 digest.
- Extends: `platform.ConfigReport` with an optional schema-3 manifest and digest accepted only for `applied`.

- [ ] **Step 1: Write failing canonicalization/application tests**

Prove resolved targets and allocation IDs are canonical and sorted, malformed/mismatched manifests reject the ACK transaction, enable/disable/enable ACK order preserves the exact applied state, unrelated later rule edits do not rewrite historical manifests, and delayed events retain a validation target after retirement.

- [ ] **Step 2: Run tests to verify RED**

Run: `go test -race ./internal/sourceflow ./internal/dataplane ./internal/platform ./internal/agent -run 'Test.*SourceFlowManifest|Test.*SourceLoggingApplied' -count=1`

Expected: FAIL because the manifest package/report fields do not exist.

- [ ] **Step 3: Implement canonical manifest production**

Build the manifest from the compiled plan after DNS resolution and exact allocation validation. Persist it in last-good Agent state and attach it only to a successful schema-3 applied report.

- [ ] **Step 4: Implement transactional platform validation/storage**

Recompute expected rule/allocation identities from the immutable snapshot, validate manifest/digest, insert `applied_source_flow_rules`, and update requested/applied rule fields in the same applied-ACK transaction.

- [ ] **Step 5: Run focused tests to verify GREEN**

Run: `go test -race ./internal/sourceflow ./internal/dataplane ./internal/platform ./internal/agent -run 'Test.*SourceFlowManifest|Test.*SourceLoggingApplied' -count=1`

Expected: PASS.

- [ ] **Step 6: Run existing application/recovery regressions**

Run: `go test -race ./internal/dataplane ./internal/platform ./internal/agent -run 'Test.*(Apply|Application|Recover|RuntimeState)' -count=1`

Expected: PASS.

### Task 4: Private bounded source-flow WAL and gap state

**Files:**
- Create: `internal/sourceflow/wal.go`
- Create: `internal/sourceflow/disk.go`
- Test: `internal/sourceflow/wal_test.go`
- Test: `internal/sourceflow/recovery_test.go`

**Interfaces:**
- Produces: `sourceflow.Open(dir string, Options) (*WAL, error)`; `WAL.AppendEvent`, `WAL.OpenGap`, `WAL.CloseGap`, `WAL.NextBatch`, `WAL.Ack`, `WAL.Status`, and `WAL.Close`.
- Produces: persisted limits, source event UUID, collector epoch/sequence, reserved gap capacity, checksummed atomic snapshot, and nonblocking exclusive directory ownership.

- [ ] **Step 1: Write failing WAL tests**

Cover private absolute paths, corrupt/trailing/tampered state, changed node/options, event capacity, reserved gap admission when full, restart, unclean restart gap, clean disabled stop, gap reason validation, ACK mismatch, duplicate ACK, batch bounds, and a separate metering WAL that remains usable when source WAL is full/corrupt.

- [ ] **Step 2: Run tests to verify RED**

Run: `go test -race ./internal/sourceflow -run 'Test.*WAL|Test.*Gap|Test.*Recovery' -count=1`

Expected: FAIL because the WAL does not exist.

- [ ] **Step 3: Implement the minimal WAL**

Follow the existing metering WAL's private-path, lock, checksum, atomic-replace, strict JSON, and uncertain-write patterns without sharing its files or failure behavior. Bound events/bytes/gaps and never evict unacknowledged facts.

- [ ] **Step 4: Run focused tests to verify GREEN**

Run: `go test -race ./internal/sourceflow -run 'Test.*WAL|Test.*Gap|Test.*Recovery' -count=1`

Expected: PASS.

- [ ] **Step 5: Checkpoint only task-owned paths**

Run: `git diff --check -- internal/sourceflow`

### Task 5: Linux conntrack collector and last-good attribution

**Files:**
- Modify: `go.mod`
- Modify: `go.sum`
- Create: `internal/sourceflow/collector.go`
- Create: `internal/sourceflow/collector_linux.go`
- Create: `internal/sourceflow/collector_other.go`
- Test: `internal/sourceflow/collector_test.go`
- Modify: `internal/agent/runtime.go`
- Modify: `internal/agent/engine.go`
- Modify: `internal/agent/supervisor.go`
- Modify: `cmd/reladock-agent/main.go`
- Test: `internal/agent/source_flow_runtime_test.go`

**Interfaces:**
- Produces: `sourceflow.Collector.Run(ctx)` subscribing only to IPv4 conntrack `NEW` and errors/overrun.
- Consumes: durable last-good `sourceflow.Manifest` and `sourceflow.WAL`.
- Extends: optional paired runtime hooks `SourceFlowCapture` and `SourceFlowUpload` without changing required validation-only activities.

- [ ] **Step 1: Write failing collector/runtime tests**

Use a real collector interface with injected event source to prove original pre-SNAT attribution, TCP/UDP validation, disabled/unrelated/retired flow filtering, five-tuple reuse, context cancellation, netlink overrun/error gaps, queue pressure, manifest replacement only after durable applied promotion, and restart reload of last-good mapping.

- [ ] **Step 2: Run tests to verify RED**

Run: `go test -race ./internal/sourceflow ./internal/agent -run 'Test.*Collector|Test.*SourceFlowRuntime' -count=1`

Expected: FAIL because the collector and runtime hooks do not exist.

- [ ] **Step 3: Add the pinned netlink dependency and Linux adapter**

Add `github.com/florianl/go-conntrack` v0.7.0, isolate it behind the event-source interface, require CAP_NET_ADMIN/apply mode, subscribe to `Conntrack`/IPv4/`NetlinkCtNew`, and map only complete original TCP/UDP tuples. Non-Linux builds return an explicit unsupported error without importing Linux-only symbols.

- [ ] **Step 4: Integrate last-good lookup and independent supervisor activity**

Start capture/upload only in apply mode with a private source WAL directory. Capture failure must back off and report gaps without freezing dataplane or changing metering behavior.

- [ ] **Step 5: Run focused tests to verify GREEN**

Run: `go test -race ./internal/sourceflow ./internal/agent -run 'Test.*Collector|Test.*SourceFlowRuntime' -count=1`

Expected: PASS.

- [ ] **Step 6: Verify all host builds still compile**

Run: `go test ./cmd/reladock-agent ./internal/sourceflow ./internal/agent -count=1`

Expected: PASS on the development host.

### Task 6: mTLS batch ingestion, idempotent ACK, and delayed validation

**Files:**
- Create: `internal/agent/source_flows.go`
- Modify: `internal/agent/client.go`
- Modify: `internal/web/agent.go`
- Modify: `internal/platform/source_flows.go`
- Test: `internal/agent/source_flows_test.go`
- Test: `internal/web/source_flows_agent_http_test.go`
- Test: `internal/platform/source_flows_ingest_test.go`

**Interfaces:**
- Produces: `/agent/source-flows` strict mTLS POST accepting bounded event/gap batches and returning exact durable event/gap ACK cursors.
- Produces: `Store.IngestSourceFlows(ctx, authenticatedNodeID string, batch SourceFlowBatch) (SourceFlowReceipt, error)`.

- [ ] **Step 1: Write failing ingestion tests**

Cover certificate/node mismatch, foreign tenant/rule/allocation, unknown generation, Agent-claimed target mismatch, invalid IP/port/protocol/time, future/stale observation bounds, oversized body/batch, duplicate retry, mixed valid/invalid atomic rollback, delayed event after disable/retirement, exact receipt, lost response replay, and gap open/close idempotence.

- [ ] **Step 2: Run tests to verify RED**

Run: `go test -race ./internal/platform ./internal/web ./internal/agent -run 'Test.*SourceFlow.*(Ingest|Agent|Upload)' -count=1`

Expected: FAIL because ingestion/transport is missing.

- [ ] **Step 3: Implement platform transaction and mTLS route**

Validate only against immutable applied manifests, insert events/gaps atomically, update latest collector cursor/health, and return an ACK only after commit. Keep the existing Agent request budget and add a route-specific bounded body/batch limit.

- [ ] **Step 4: Implement Agent batch upload and ACK pruning**

Use the existing mTLS client, strict JSON, no redirects, timeout/error handling, exact receipt matching, and WAL prune only after durable ACK.

- [ ] **Step 5: Run focused tests to verify GREEN**

Run: `go test -race ./internal/platform ./internal/web ./internal/agent -run 'Test.*SourceFlow.*(Ingest|Agent|Upload)' -count=1`

Expected: PASS.

### Task 7: Collector health, incidents, retention, and privacy-audited admin query

**Files:**
- Modify: `internal/platform/heartbeat.go`
- Modify: `internal/platform/operations.go`
- Modify: `internal/platform/notifications.go`
- Modify: `internal/platform/metrics.go`
- Modify: `internal/platform/source_flows.go`
- Modify: `internal/web/admin.go`
- Modify: `cmd/reladock/main.go`
- Test: `internal/platform/source_flow_operations_test.go`
- Test: `internal/web/source_flow_admin_http_test.go`

**Interfaces:**
- Produces: heartbeat `SourceFlowStatus` and persisted `node_source_flow_latest`.
- Produces: two-minute sustained `source-flow-collector-degraded` incident with two-minute normal resolution.
- Produces: `GET /api/admin/source-flows` with required bounded time range, filters, page size 50, and privacy-access audit.
- Produces: bounded retention worker governed by explicit `SOURCE_FLOW_RETENTION_DAYS`.

- [ ] **Step 1: Write failing health/query/retention tests**

Cover quiet healthy collector, contact stale separation, open gap/new drops, backlog age, WAL 80 percent threshold, sustained incident open/resolve/recur, no raw source labels in Prometheus, platform-only pagination/filter isolation, required/maximum time range, privacy audit rollback, received-time retention, small delete batches, manifest/gap horizon, and restore cleanup gate.

- [ ] **Step 2: Run tests to verify RED**

Run: `go test -race ./internal/platform ./internal/web -run 'Test.*SourceFlow.*(Health|Incident|Query|Retention|Metrics)' -count=1`

Expected: FAIL because health reconciliation/query/retention do not exist.

- [ ] **Step 3: Implement heartbeat health and incident reconciliation**

Keep node contact distinct; persist bounded status only from authenticated heartbeats, derive normal/degraded/stale exactly as the spec states, and use existing incident/notification transaction patterns.

- [ ] **Step 4: Implement bounded query and retention worker**

Require indexed filters and a bounded time range, audit successful reads without copying IPs into audit details, delete by received time in bounded batches, and retain applied manifests/gaps for the same horizon.

- [ ] **Step 5: Run focused tests to verify GREEN**

Run: `go test -race ./internal/platform ./internal/web -run 'Test.*SourceFlow.*(Health|Incident|Query|Retention|Metrics)' -count=1`

Expected: PASS.

### Task 8: Platform controls, source-flow page, and tenant status

**Files:**
- Create: `public/source-flow-logging.js`
- Test: `public/source-flow-logging.test.js`
- Modify: `public/admin.html`
- Modify: `public/admin.js`
- Modify: `public/admin.css`
- Modify: `public/index.html`
- Modify: `public/app.js`
- Modify: `public/assets.go`
- Modify: `internal/web/server.go`
- Test: `internal/web/static_assets_test.go`

**Interfaces:**
- Produces: source logging confirmation/status controller and paginated platform source-flow view.
- Consumes: admin toggle/query APIs and tenant rule requested/applied/collector status.

- [ ] **Step 1: Write failing UI/controller tests**

Cover six tenant statuses, unsupported/degraded reasons, admin-only toggle, performance/privacy/retention confirmation, submit lock, stale async response isolation, desired-version conflicts, paginated filters, explicit gap rendering when zero events exist, and absence of payload/export controls.

- [ ] **Step 2: Run tests to verify RED**

Run: `node --test public/source-flow-logging.test.js && go test ./internal/web -run 'Test.*SourceFlow.*Static|TestStaticAssets' -count=1`

Expected: FAIL because the controller/assets/markup do not exist.

- [ ] **Step 3: Implement platform and tenant UI**

Use page-local dialogs and existing navigation patterns. Update only the latest request, keep controls disabled while submitting, show requested versus applied state, and keep raw tuples out of tenant responses/DOM.

- [ ] **Step 4: Run focused tests to verify GREEN**

Run: `node --test public/source-flow-logging.test.js && go test ./internal/web -run 'Test.*SourceFlow.*Static|TestStaticAssets' -count=1`

Expected: PASS.

- [ ] **Step 5: Run JavaScript syntax checks through existing tests**

Run: `node --test public/*.test.js`

Expected: all applicable tests pass; the existing opt-in Linux installer test may remain skipped by design.

### Task 9: Isolated Linux proof, documentation, and full verification

**Files:**
- Modify: `internal/dataplane/integration_test.go`
- Modify: `docs/REQUIREMENTS.md`
- Modify: `docs/ARCHITECTURE.md`
- Modify: `docs/ROADMAP.md`
- Modify: `docs/IMPLEMENTATION_STATUS.md`
- Modify: `docs/DEPLOYMENT.md`
- Modify: `docs/OPERATIONS.md`
- Modify: `docs/acceptance/LINK_AND_CAPACITY.md`

**Interfaces:**
- Proves: original source tuple before SNAT is recorded for TCP and UDP pseudo-flow, and optional logging failure does not stop forwarding or billing metering.
- Documents: implementation versus production acceptance boundaries and operator retention/capability prerequisites.

- [ ] **Step 1: Write/extend the opt-in isolated Linux test before integration code changes**

Add a disposable namespace/container case that applies a schema-3 rule, creates traffic from a distinct client address/port, observes a source event before SNAT, verifies landing still sees the relay SNAT address, injects collector/WAL failure, and proves forwarding plus metering continue while a gap is reported.

- [ ] **Step 2: Run isolated test to verify RED**

Run the repository's opt-in dataplane integration command from `docs/DEPLOYMENT.md` with the new source-flow case selected.

Expected: FAIL before collector integration is complete or SKIP with an explicit missing Linux/Docker prerequisite that must be reported, never silently counted as proof.

- [ ] **Step 3: Make only the integration wiring needed for GREEN**

Wire the real Linux adapter and test harness without altering host network state or enabling production services.

- [ ] **Step 4: Run the isolated test to verify GREEN**

Run the same opt-in command.

Expected: PASS, or report the exact environmental blocker without claiming real-kernel validation.

- [ ] **Step 5: Update required documentation and implementation status**

Record implemented controls, measured test results, performance caveats, retention/privacy boundary, current production version incompatibility, and that no production rule was enabled or deployed.

- [ ] **Step 6: Run complete verification**

Run: `npm test`

Expected: Go real-PostgreSQL race suite and Node suite complete with zero failures; report every skipped test.

Run: `npm run vet`

Expected: exit 0.

Run: `git diff --check`

Expected: exit 0.

- [ ] **Step 7: Inspect the final scoped diff**

Run: `git status --short --branch && git diff --stat && git diff -- docs/IMPLEMENTATION_STATUS.md docs/REQUIREMENTS.md docs/ARCHITECTURE.md docs/ROADMAP.md`

Expected: only intended source-flow implementation/docs plus the user's pre-existing unrelated changes; do not reset or include unrelated files.
