# Safe Allocation and Node Retirement Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task in the current feature branch. Every behavioral change starts with a failing test and ends with a focused commit.

**Goal:** Release previously dispatched rule ports only after an authenticated Agent proves an exact, fail-closed conntrack purge under pre-conntrack quarantine, and permanently retire drained node identities without deleting external infrastructure.

**Architecture:** PostgreSQL owns immutable allocation incarnations, retirement jobs, barriers, proof, quarantine-consumption lineage, and node drain eligibility. Schema-v2 node snapshots install allocation-ID-bound raw-priority quarantine plus forward guards. The apply Agent serializes purge and configuration work through the existing external control lock, uses a fixed conntrack executable without a shell, persists every transition, and retains reusable pre-conntrack quarantine until a later allocation atomically takes over the tuple. A final short-lived Agent attestation gates certificate revocation and the irreversible node `retired` state.

**Tech Stack:** Go 1.24+, PostgreSQL, nftables, conntrack-tools, TLS 1.3 mTLS, systemd, POSIX installer scripts, Node.js test/release tooling, vanilla admin UI.

**Spec:** `docs/superpowers/specs/2026-11-01-safe-retirement-design.md`

## Global Constraints

- Do not modify payment behavior or connect a real payment provider.
- Do not contact production VPSs, change production network rules, publish a Release, or delete external infrastructure.
- Never release an allocation because a node is offline, a lease expires, or an operator clicks a force button.
- Never execute `conntrack -F`, an unfiltered delete, a shell command, caller-provided argv, setns, nonzero conntrack zones, or IPv6 retirement in v1.
- Existing `0.0.0.0` historical allocations remain fenced and ineligible for automatic retirement.
- A successful purge is an authenticated node assertion about one allocation incarnation; it is not end-to-end service health.
- All configuration, purge, upgrade, metering, and final-drain barriers fail closed.
- Real VPS/kernel and offline-recovery exercises remain explicit production acceptance gates even after all automated tests pass.

## Review Focus

- A packet arriving during purge cannot create a new conntrack entry because a verified raw-priority quarantine drops it before conntrack.
- A delayed old journal cannot purge or ACK a later tenant's reuse of the same tuple because every allocation, guard, snapshot, proof, and predecessor link carries an immutable UUID.
- `PurgedPendingACK` removes only the obsolete forward guard; pre-conntrack quarantine survives crashes and ACK loss.
- Port reuse removes the old quarantine and activates the new DNAT only in one nft transaction; failed apply restores the old quarantine.
- Applying config, purging retirement, and active upgrade states are mutually excluded in PostgreSQL and by the existing root-owned local control lock.
- Node retirement requires exact reusable-quarantine reconciliation, drained usage WAL, stable dataplane state, a fresh certificate-bound attestation, and transactional identity revocation.

---

### Task 1: Allocation Incarnations and Retirement Schema

**Files:**
- Create: `internal/platform/migration22.sql`
- Create: `internal/platform/retirements_test.go`
- Modify: `internal/platform/store.go`
- Modify: `internal/platform/schema.sql`
- Modify: `internal/platform/heartbeat_test.go`
- Modify: `internal/platform/entitlement_test.go`

**Interfaces:**
- Produces schema version 22, immutable `endpoint_allocations.allocation_id`, retirement rows, predecessor/consumption lineage, drain attestations, expanded node status constraints, and database uniqueness for one claimed/purging retirement per node.

- [ ] **Step 1: Write failing migration tests**

Cover fresh install and v21 upgrade; deterministic preservation of existing tenants/rules/allocations; non-null unique allocation UUIDs; TCP/UDP independence; retirement state/error checks; one active purge per node; immutable released history after endpoint deletion; predecessor referential integrity; `draining/retired` node statuses; and migration rollback on an injected constraint failure.

- [ ] **Step 2: Run the focused tests and verify failure**

Run: `go test ./internal/platform -run 'Migration|RetirementSchema|AllocationIncarnation'`

Expected: FAIL because schema version 22 and retirement objects do not exist.

- [ ] **Step 3: Add migration 22 and fresh-schema parity**

Add UUID allocation identities, `endpoint_retirements`, predecessor and consumption fields, bounded proof/error columns, `node_drain_attestations`, necessary indexes, and status checks. Historical deleted allocations become `waiting-removal`; do not mark them purgeable or synthesize proof. Embed/apply migration 22 and update destructive migration fixtures.

- [ ] **Step 4: Run platform migration tests**

Run: `go test ./internal/platform -run 'Migration|RetirementSchema|AllocationIncarnation|HeartbeatSchema'`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/platform/migration22.sql internal/platform/retirements_test.go internal/platform/store.go internal/platform/schema.sql internal/platform/heartbeat_test.go internal/platform/entitlement_test.go
git commit -m "feat: persist allocation retirement state"
```

### Task 2: Schema-v2 Snapshots and Platform State Machine

**Files:**
- Create: `internal/platform/retirements.go`
- Create: `internal/platform/retirement_delivery_test.go`
- Modify: `internal/platform/delivery.go`
- Modify: `internal/platform/application.go`
- Modify: `internal/platform/rules.go`
- Modify: `internal/platform/rules_test.go`
- Modify: `internal/platform/application_test.go`
- Modify: `internal/platform/node_upgrades.go`
- Modify: `internal/platform/node_upgrades_test.go`

**Interfaces:**
- Produces `ConfigAllocation`, `ConfigRetirement`, schema-v2 `NodeConfig`, `RetirementClaim`, `RetirementProof`, `ReconcileRetirements`, `ClaimRetirement`, `BeginRetirementPurge`, and `ReportRetirement`.

- [ ] **Step 1: Write failing lifecycle and race tests**

Cover allocation UUIDs in snapshots; deletion atomically creating TCP/UDP retirement rows and a quarantine generation; unapplied rules retaining the existing immediate-release path; later applied generation covering removal; strict claim tuple; two-minute hashed lease; five attempts; idempotent begin; no destructive begin after lease loss; no second node purge; apply/purge and upgrade/purge exclusion; exact proof replay; conflicting proof rejection; allocation deletion plus audit in one transaction; and concurrent create remaining blocked until release.

- [ ] **Step 2: Run focused tests and verify failure**

Run: `go test ./internal/platform -run 'Retirement|Allocation|ApplicationBarrier|AgentUpgrade'`

Expected: FAIL with missing schema-v2 snapshot and state-machine methods.

- [ ] **Step 3: Implement transactional deletion and reconciliation**

Freeze allocation IDs/tuples in the deletion transaction, create `waiting-removal`, enqueue schema-v2 full snapshots containing all waiting/queued/claimed two-layer guards, and advance only database-proven applied generations to queued. Reject wildcard and identity mismatches without releasing anything.

- [ ] **Step 4: Implement claim, begin, report, and consumption lineage**

Hash lease tokens at rest. `begin-purge` is the only `claimed -> purging` edge and is idempotent. Final report validates canonical proof and current node identity under the node lock, stores digest/proof, releases the endpoint, and audits atomically. Applied ACK for a new allocation marks only its exact predecessor quarantine consumed.

- [ ] **Step 5: Run platform tests**

Run: `go test ./internal/platform`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/platform/retirements.go internal/platform/retirement_delivery_test.go internal/platform/delivery.go internal/platform/application.go internal/platform/rules.go internal/platform/rules_test.go internal/platform/application_test.go internal/platform/node_upgrades.go internal/platform/node_upgrades_test.go
git commit -m "feat: coordinate allocation retirement"
```

### Task 3: Allocation-Bound Pre-Conntrack Quarantine

**Files:**
- Modify: `internal/dataplane/compiler.go`
- Modify: `internal/dataplane/verify.go`
- Modify: `internal/dataplane/adapter.go`
- Modify: `internal/dataplane/compiler_test.go`
- Modify: `internal/dataplane/verify_test.go`
- Modify: `internal/dataplane/adapter_test.go`
- Modify: `internal/dataplane/integration_test.go`

**Interfaces:**
- Produces allocation-ID-aware plans with raw-priority `quarantine` base chain, ID-bound forward guards, `PurgedPendingACK`, and `ReusableQuarantines` persisted state.

- [ ] **Step 1: Write failing compiler and verifier tests**

Assert raw-priority filter/prerouting chain ownership; exact concrete IPv4/protocol/port drop; stable allocation-ID comments; both guards for waiting/queued/claimed; only quarantine for purged/reusable; no quarantine for active allocation; strict rejection of wildcard/IPv6/duplicate IDs/tuple-ID conflicts; and verifier rejection of wrong priority, hook, expression, comment, ownership, or extra duplicate rule.

- [ ] **Step 2: Run focused tests and verify failure**

Run: `go test ./internal/dataplane -run 'Compile|Verify|Quarantine|AllocationID'`

Expected: FAIL because plans have no allocation identity or pre-conntrack chain.

- [ ] **Step 3: Extend plan/state without weakening recovery**

Compile schema-v2 snapshots into exact two-layer protection. Persist purged/reusable sets separately from failed candidate Tombstones. Every apply and rollback overlays local purged/reusable quarantine, and active reuse is allowed only with the exact predecessor relation.

- [ ] **Step 4: Add namespace integration coverage**

Extend opt-in Linux tests to prove the quarantine is installed before conntrack, blocks new TCP/UDP state, preserves unrelated tuples, and is restored after failed tuple reuse. Do not claim these tests ran on macOS.

- [ ] **Step 5: Run dataplane tests**

Run: `go test ./internal/dataplane`

Expected: PASS for default tests; privileged tests may skip with an explicit reason.

- [ ] **Step 6: Commit**

```bash
git add internal/dataplane
git commit -m "feat: add allocation-bound quarantine"
```

### Task 4: Fixed Conntrack Executor

**Files:**
- Create: `internal/retirement/conntrack.go`
- Create: `internal/retirement/conntrack_test.go`
- Create: `internal/retirement/proof.go`
- Create: `internal/retirement/proof_test.go`

**Interfaces:**
- Consumes a validated structured IPv4 allocation and a fixed direct runner.
- Produces bounded delete/query results and canonical versioned proof bytes/digest without exposing raw command output.

- [ ] **Step 1: Write hostile runner/output tests**

Assert exact argv for TCP/UDP; no shell; no flush; no caller argv; default namespace/zone only; fixed `/usr/sbin/conntrack`; version gate; output/time bounds; strict XML with zero entries; malformed/trailing/truncated XML rejection; permission/syntax errors; zero-match idempotence; repeated query until zero; and unrelated tuple preservation.

- [ ] **Step 2: Run focused tests and verify failure**

Run: `go test ./internal/retirement`

Expected: FAIL because the package does not exist.

- [ ] **Step 3: Implement fixed execution and canonical proof**

Generate arguments solely from normalized structures. Bound every execution and XML body. Map failures to the spec's closed error set. Encode proof through a fixed Go struct with schema version, normalized fields, UTC timestamps, stable field order, and SHA-256.

- [ ] **Step 4: Run retirement package tests**

Run: `go test ./internal/retirement`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/retirement
git commit -m "feat: add exact conntrack retirement executor"
```

### Task 5: Crash-Safe Local Purge and Atomic Reuse

**Files:**
- Create: `internal/dataplane/retirement.go`
- Create: `internal/dataplane/retirement_test.go`
- Modify: `internal/dataplane/adapter.go`
- Modify: `internal/dataplane/state.go`
- Modify: `internal/dataplane/recovery_test.go`
- Modify: `internal/dataplane/integration_test.go`

**Interfaces:**
- Produces `Adapter.PurgeAllocation`, durable `PurgedPendingACK`, `ConfirmRetirementACK`, restart recovery, and predecessor-bound quarantine-to-DNAT handoff.

- [ ] **Step 1: Write failure-injection tests for every boundary**

Cover crash before/after journal intent, guard verification, conntrack delete, zero query, forward-guard removal, nft rename, state rename/fsync, proof save, ACK loss, local ACK confirmation, and future reuse apply. Assert uncertain transitions restore both guards or retain quarantine, never silently release, and never allow an old ID to affect a new ID.

- [ ] **Step 2: Run focused tests and verify failure**

Run: `go test ./internal/dataplane -run 'Retirement|Purge|Reusable|Recovery'`

Expected: FAIL with missing purge lifecycle.

- [ ] **Step 3: Implement the durable transition under the existing lock**

Reuse Candidate/Pending/Recovery semantics. Verify both guards, execute the injected exact conntrack purger, remove only the old forward guard in one checked nft replacement, persist `PurgedPendingACK`, and make proof replay stable. ACK confirmation changes only local classification, not kernel state.

- [ ] **Step 4: Implement atomic tuple takeover**

Require new allocation ID plus exact predecessor. One nft transaction removes predecessor quarantine and adds active DNAT/forwarding; rollback restores quarantine. Surface consumed lineage only through applied ACK.

- [ ] **Step 5: Run dataplane race tests**

Run: `go test -race ./internal/dataplane`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/dataplane/retirement.go internal/dataplane/retirement_test.go internal/dataplane/adapter.go internal/dataplane/state.go internal/dataplane/recovery_test.go internal/dataplane/integration_test.go
git commit -m "feat: retire dataplane allocations safely"
```

### Task 6: mTLS Retirement API and Agent Supervisor

**Files:**
- Create: `internal/agent/retirement.go`
- Create: `internal/agent/retirement_test.go`
- Create: `internal/agent/retirement_state.go`
- Create: `internal/agent/retirement_state_test.go`
- Create: `internal/web/retirement_http_test.go`
- Modify: `internal/agent/client.go`
- Modify: `internal/agent/supervisor.go`
- Modify: `internal/agent/supervisor_test.go`
- Modify: `internal/web/agent.go`
- Modify: `cmd/reladock-agent/main.go`

**Interfaces:**
- Produces `POST /agent/retirements/claim`, `/begin`, and `/report`; Agent journal recovery; and a supervisor hook that runs before configuration apply only in explicit apply mode.

- [ ] **Step 1: Write failing HTTP and Agent lifecycle tests**

Assert mTLS node/fingerprint binding, strict bodies, no browser Origin, wrong-node denial, response bounds, token secrecy, ambiguous begin retry without purge, report replay, validation-only 204/no claim, apply-mode recovery before config, shared control lock, active purge blocking config, and rotated same-node certificate recovery.

- [ ] **Step 2: Run focused tests and verify failure**

Run: `go test ./internal/web ./internal/agent -run 'Retirement|Supervisor'`

Expected: FAIL with missing endpoints and hook.

- [ ] **Step 3: Implement strict Agent endpoints and client**

Reuse existing TLS identity extraction, credential recheck under node lock, rate limits, strict decoder, no redirects, and bounded contexts. Never return executable arguments or accept an allocation from the Agent.

- [ ] **Step 4: Implement durable Agent orchestration**

Persist journal before begin, require explicit begin success, invoke Task 5 under the existing root control lock, persist proof before report, replay report until accepted, then confirm local ACK and clear journal. Supervisor startup always resolves an existing journal first.

- [ ] **Step 5: Run Agent/web tests**

Run: `go test -race ./internal/agent ./internal/web ./cmd/reladock-agent`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/agent/retirement.go internal/agent/retirement_test.go internal/agent/retirement_state.go internal/agent/retirement_state_test.go internal/agent/client.go internal/agent/supervisor.go internal/agent/supervisor_test.go internal/web/retirement_http_test.go internal/web/agent.go cmd/reladock-agent/main.go
git commit -m "feat: run authenticated retirement jobs"
```

### Task 7: Draining and Final Node Attestation

**Files:**
- Create: `internal/platform/node_retirement.go`
- Create: `internal/platform/node_retirement_test.go`
- Create: `internal/agent/drain.go`
- Create: `internal/agent/drain_test.go`
- Create: `internal/web/node_retirement_http_test.go`
- Modify: `internal/platform/admin_nodes.go`
- Modify: `internal/platform/heartbeat.go`
- Modify: `internal/platform/rules.go`
- Modify: `internal/platform/node_upgrades.go`
- Modify: `internal/web/admin.go`
- Modify: `internal/web/agent.go`
- Modify: `cmd/reladock-agent/main.go`

**Interfaces:**
- Produces administrator `start-draining/finalize-retirement`, Agent drain-attestation reporting, server-computed eligibility/reasons, and the separate never-deployed registration revocation path.

- [ ] **Step 1: Write failing lifecycle and attestation tests**

Cover maintenance-only entry; irreversible draining; operation allow/deny matrix; new allocation/resume/edit/enroll/revoke/upgrade rejection; delete/retry/rotation/upgrade-cancel allowance; exact expected reusable quarantine set; final usage WAL ACK; no pending dataplane/updater/journal; certificate fingerprint, generation, boot ID, started-at, sequence and Good digest binding; two-minute/30-second skew; single use; all invalidation events; and transactional certificate/bootstrap/rotation revocation plus audit rollback.

- [ ] **Step 2: Run focused tests and verify failure**

Run: `go test ./internal/platform ./internal/agent ./internal/web -run 'Drain|NodeRetirement|Attestation'`

Expected: FAIL with missing lifecycle.

- [ ] **Step 3: Implement final Agent evidence and server eligibility**

Add boot ID to authenticated runtime facts. Build attestation only after stable dataplane readback, empty local work, and drained usage WAL. Store only bounded canonical evidence/digests. Compute eligibility entirely on the server and invalidate stale evidence on every specified transition.

- [ ] **Step 4: Implement irreversible finalization**

Under platform and node locks, recheck every gate, consume the attestation, revoke certificate/bootstrap/rotation state, set retired, increment revision, and audit in one transaction. Never delete the node, history, proof, VPS, DNS, or quarantine.

- [ ] **Step 5: Run lifecycle tests**

Run: `go test -race ./internal/platform ./internal/agent ./internal/web`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/platform/node_retirement.go internal/platform/node_retirement_test.go internal/platform/admin_nodes.go internal/platform/heartbeat.go internal/platform/rules.go internal/platform/node_upgrades.go internal/agent/drain.go internal/agent/drain_test.go internal/web/node_retirement_http_test.go internal/web/admin.go internal/web/agent.go cmd/reladock-agent/main.go
git commit -m "feat: drain and retire node identities"
```

### Task 8: Admin Presentation, Packaging, and Operations Documentation

**Files:**
- Modify: `public/admin.js`
- Modify: `public/admin.html`
- Modify: `public/app.js`
- Modify: `internal/web/static_assets_test.go`
- Modify: `deploy/install-linux.sh`
- Modify: `deploy/reladock-agent-apply.service`
- Modify: `test/linux-install.test.mjs`
- Modify: `scripts/release.mjs`
- Modify: `test/release-package.test.mjs`
- Modify: `README.md`
- Modify: `docs/ARCHITECTURE.md`
- Modify: `docs/DATAPLANE.md`
- Modify: `docs/DEPLOYMENT.md`
- Modify: `docs/OPERATIONS.md`
- Modify: `docs/ROADMAP.md`

**Interfaces:**
- Produces truthful retirement states/actions, fixed conntrack dependency/capability, release packaging, and operator runbooks without force-release instructions.

- [ ] **Step 1: Write failing static/install/package tests**

Assert server-derived labels/actions; no client eligibility; no force release; draining confirmation text; fixed error explanations; conntrack-tools installation and `/usr/sbin/conntrack` preflight; apply unit capability only; validation unit cannot purge; release includes all required binaries/services; and installation remains apply-disabled by default.

- [ ] **Step 2: Run focused tests and verify failure**

Run: `go test ./internal/web -run 'Static' && node --test test/linux-install.test.mjs test/release-package.test.mjs`

Expected: FAIL because UI/package contracts are absent.

- [ ] **Step 3: Implement UI and packaging**

Render waiting/cleaning/released/needs-attention and node draining/retired states from server fields. Add guarded actions with revision only. Install/verify conntrack-tools for apply hosts without enabling apply or changing network prerequisites.

- [ ] **Step 4: Update documentation truthfully**

Document state machines, pre-conntrack quarantine, residual retired-node quarantine, no force release, wildcard limitation, recovery, exact acceptance gates, and which privileged/VPS tests remain unexecuted.

- [ ] **Step 5: Run focused UI/package tests**

Run: `go test ./internal/web && node --test test/linux-install.test.mjs test/release-package.test.mjs`

Expected: PASS, with the existing privileged installer test skipped unless explicitly enabled.

- [ ] **Step 6: Commit**

```bash
git add public internal/web/static_assets_test.go deploy test/linux-install.test.mjs scripts/release.mjs test/release-package.test.mjs README.md docs
git commit -m "feat: expose safe retirement operations"
```

### Task 9: Integrated Safety Verification

**Files:**
- Modify: `internal/dataplane/integration_test.go`
- Create: `docs/acceptance/SAFE_RETIREMENT.md`
- Modify: `docs/DEPLOYMENT.md`
- Modify: `docs/ROADMAP.md`

- [ ] **Step 1: Add opt-in Linux acceptance scenarios**

Cover long-lived TCP, UDP state, packets racing purge, unrelated tuples, duplicate purge, crash after delete/before ACK, lost platform response, restart recovery, atomic tuple reuse, rollback, concurrent create, wildcard refusal, final drain gates, and retained quarantine after identity revocation.

- [ ] **Step 2: Run all locally available verification**

Run:

```bash
npm test
npm run vet
npm run build:release
git diff --check
```

Expected: PASS. Record exact skipped privileged/VPS scenarios; do not convert skips into success claims.

- [ ] **Step 3: Request an independent code review and fix findings**

Review database races, raw-hook correctness, parser hostility, WAL/recovery, certificate binding, proof replay, UI truthfulness, installer capabilities, and failure-injection coverage. Repeat focused and full tests after fixes.

- [ ] **Step 4: Write the external acceptance checklist**

Provide commands and expected evidence for an isolated Linux/VPS rehearsal, but do not run against production without a new explicit authorization. Keep “production ready” unchecked until that rehearsal and off-host recovery are actually completed.

- [ ] **Step 5: Commit**

```bash
git add internal/dataplane/integration_test.go docs/acceptance/SAFE_RETIREMENT.md docs/DEPLOYMENT.md docs/ROADMAP.md
git commit -m "test: verify safe retirement lifecycle"
```

