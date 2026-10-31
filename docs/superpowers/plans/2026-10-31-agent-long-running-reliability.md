# Agent Long-Running Reliability Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add independent jittered Agent reconnect loops, factual heartbeats, and crash-safe two-phase automatic node-certificate renewal without changing the nftables/tc data plane.

**Architecture:** Keep the current mTLS Agent listener and exact node-fingerprint authorization. Add schema v19 for heartbeat and one pending rotation per node, issue/activate certificates transactionally with the existing node lock, and add a local credential manager that atomically publishes a single PEM identity bundle. A Supervisor schedules configuration, heartbeat, usage/probe/preflight, and renewal independently so one failing activity cannot block another.

**Tech Stack:** Go 1.24+, PostgreSQL migrations and transactions, TLS 1.3/mTLS, static JavaScript admin UI, Node.js test runner.

**Spec:** `docs/superpowers/specs/2026-10-31-agent-long-running-reliability-design.md`

## Global Constraints

- Do not modify or replace the nftables/tc forwarding implementation or its persistent journal/WAL semantics.
- Do not deploy FRP, GOST, Rathole, NPS, Realm, Xray, or sing-box.
- Never label heartbeat, certificate state, applied ACK, or target TCP reachability as end-to-end forwarding health.
- Certificates remain valid for 30 days and renewal begins with 7 days remaining.
- Online means a successful heartbeat in the previous 90 seconds; stale contact is offline, not confirmed reconnecting.
- Agent activities use independent jittered exponential backoff: 1 second base, 60 second cap; a successful activity resets only its own failures.
- Production with an enabled mTLS Agent listener requires a usable node signing CA; GitHub Release metadata remains independently optional for renewal.
- Private keys never leave the node. Unknown or corrupt local rotation state fails closed and is never repaired by deleting PKI, journal, WAL, or applying records.
- Changes require tests first, relevant documentation updates, `npm test`, and `npm run vet`; no production deployment or release is part of this plan.

## Review Focus

- Renewal response lost after signing: retrying the same CSR must return the byte-identical certificate and must not issue a second pending identity (Task 3 tests).
- Activation committed but response lost: the new certificate must be able to repeat activation and the old certificate must remain rejected (Task 3 tests).
- Crash between every local rotation phase: restart must continue the same CSR/key and never overwrite the last usable identity (Task 4 tests).
- Manual enrollment/revocation racing a pending rotation: the manual transaction must cancel the pending identity so it can never activate later (Task 3 tests).
- A probe, heartbeat, renewal, or control outage must not prevent local metering/WAL persistence or permanently delay unrelated Agent activities (Tasks 1 and 5 tests).

---

### Task 1: Independent Backoff and Supervisor Scheduling

**Files:**
- Create: `internal/agent/backoff.go`
- Create: `internal/agent/backoff_test.go`
- Create: `internal/agent/supervisor.go`
- Create: `internal/agent/supervisor_test.go`
- Modify: `cmd/reladock-agent/main.go`

**Interfaces:**
- Produces: `type Backoff struct`, `func NewBackoff(base, max time.Duration, jitter func(time.Duration) time.Duration) *Backoff`, `func (b *Backoff) Failure() time.Duration`, and `func (b *Backoff) Success()`.
- Produces: `type Activity struct { Name string; Interval time.Duration; Run func(context.Context) error }` and `func RunSupervisor(context.Context, []Activity, func(string, error)) error`.
- Consumes later: Task 5 registers configuration, heartbeat, preflight, engine tick, and renewal as separate activities.

- [ ] **Step 1: Write failing backoff tests**

  Add tests asserting 1s/2s/4s growth, a 60s cap, deterministic injected jitter, and reset to 1s after `Success`.

- [ ] **Step 2: Run the focused test and confirm RED**

  Run: `go test ./internal/agent -run 'TestBackoff'`

  Expected: FAIL because `NewBackoff` does not exist.

- [ ] **Step 3: Implement the minimal backoff**

  Implement the exact interfaces above without third-party dependencies or global random state in tests.

- [ ] **Step 4: Run the focused test and confirm GREEN**

  Run: `go test ./internal/agent -run 'TestBackoff'`

  Expected: PASS.

- [ ] **Step 5: Write failing Supervisor tests**

  Assert that activities retain independent failure counts, one activity's error does not block another, success resets only that activity, and context cancellation interrupts waiting immediately.

- [ ] **Step 6: Run the Supervisor test and confirm RED**

  Run: `go test ./internal/agent -run 'TestRunSupervisor'`

  Expected: FAIL because `RunSupervisor` does not exist.

- [ ] **Step 7: Implement Supervisor and replace the fixed loop**

  Move scheduling out of `main.go`; preserve validation/apply logs and `--once` behavior. Do not change `Client.Once`, Agent journal, adapter, or WAL semantics.

- [ ] **Step 8: Run Agent unit tests**

  Run: `go test ./internal/agent ./cmd/reladock-agent`

  Expected: PASS.

- [ ] **Step 9: Commit**

  Run: `git add internal/agent/backoff.go internal/agent/backoff_test.go internal/agent/supervisor.go internal/agent/supervisor_test.go cmd/reladock-agent/main.go && git commit -m 'feat: add resilient agent supervisor'`

### Task 2: Schema v19 and Factual Heartbeats

**Files:**
- Create: `internal/platform/migration19.sql`
- Create: `internal/platform/heartbeat.go`
- Create: `internal/platform/heartbeat_test.go`
- Modify: `internal/platform/store.go`
- Modify: `internal/platform/admin_nodes.go`
- Modify: `internal/platform/admin_test.go`
- Create: `internal/agent/heartbeat.go`
- Create: `internal/agent/heartbeat_test.go`
- Modify: `internal/web/agent.go`
- Modify: `internal/web/agent_test.go`

**Interfaces:**
- Produces: `platform.AgentHeartbeat` with `Version`, `Mode`, `StartedAt`, `CertificateNotAfter`, `RotationState`, and `RotationErrorCode` JSON fields.
- Produces: `func (s *Store) ReportAgentHeartbeat(ctx context.Context, nodeID, fingerprint string, heartbeat AgentHeartbeat) error`.
- Produces: `func (c *Client) ReportHeartbeat(ctx context.Context, heartbeat platform.AgentHeartbeat) error`.
- Extends: `platform.ManagedNode` with non-sensitive Agent mode/version/start, certificate expiry, rotation state/error, and existing three-value contact status.

- [ ] **Step 1: Write failing migration and store tests**

  Test migration idempotence, strict mode/error-code constraints, invalid/future timestamps, fingerprint mismatch, 90-second online boundary, and that no certificate/CSR/private material appears in `AdminNodes` JSON.

- [ ] **Step 2: Run the focused platform tests and confirm RED**

  Run: `node scripts/start.mjs test -race -run 'Test(AgentHeartbeat|AdminNodesExposeAgent)' ./internal/platform`

  Expected: FAIL because schema v19 and heartbeat APIs do not exist.

- [ ] **Step 3: Implement migration and transactional heartbeat storage**

  Embed `migration19.sql`, set `SchemaVersion = 19`, and update contact time only after exact credential authorization under the node transaction lock. Keep legacy preflight data separate.

- [ ] **Step 4: Run platform tests and confirm GREEN**

  Run: `node scripts/start.mjs test -race -run 'Test(AgentHeartbeat|AdminNodesExposeAgent)' ./internal/platform`

  Expected: PASS.

- [ ] **Step 5: Write failing Agent HTTP/client tests**

  Assert POST-only JSON, mTLS identity enforcement, bounded values, no redirects, and that a successful heartbeat updates contact independently of config claim.

- [ ] **Step 6: Run HTTP/client tests and confirm RED**

  Run: `go test ./internal/agent ./internal/web -run 'Test.*Heartbeat'`

  Expected: FAIL because `/agent/heartbeat` and `Client.ReportHeartbeat` do not exist.

- [ ] **Step 7: Implement heartbeat client and endpoint**

  Route through the existing Agent request budget and sanitized error behavior. Do not accept browser Cookie/API Key/bootstrap credentials.

- [ ] **Step 8: Run focused tests and commit**

  Run: `node scripts/start.mjs test -race -run 'Test.*Heartbeat' ./internal/agent ./internal/platform ./internal/web`

  Expected: PASS.

  Run: `git add internal/platform/migration19.sql internal/platform/heartbeat.go internal/platform/heartbeat_test.go internal/platform/store.go internal/platform/admin_nodes.go internal/platform/admin_test.go internal/agent/heartbeat.go internal/agent/heartbeat_test.go internal/web/agent.go internal/web/agent_test.go && git commit -m 'feat: add agent heartbeat state'`

### Task 3: Transactional Two-Phase Certificate Renewal

**Files:**
- Create: `internal/platform/certificate_rotation.go`
- Create: `internal/platform/certificate_rotation_test.go`
- Modify: `internal/platform/migration19.sql`
- Modify: `internal/platform/admin_nodes.go`
- Modify: `internal/platform/bootstrap.go`
- Modify: `internal/platform/admin_test.go`
- Modify: `internal/platform/bootstrap_test.go`
- Modify: `internal/web/agent.go`
- Modify: `internal/web/agent_test.go`
- Modify: `internal/nodeauth/bootstrap.go`
- Modify: `internal/nodeauth/bootstrap_test.go`

**Interfaces:**
- Produces: `platform.CertificateRotation` response containing `Certificate` and `ExpiresAt`, never a private key.
- Produces: `func (s *Store) RenewNodeCertificate(ctx context.Context, nodeID, currentFingerprint string, csr []byte, sign BootstrapSigner) (CertificateRotation, error)`.
- Produces: `func (s *Store) ActivateNodeCertificate(ctx context.Context, nodeID, presentedFingerprint string) error` with idempotent success for the already-promoted fingerprint.
- Produces: `func (s *Store) AuthenticateNodeForActivation(ctx context.Context, nodeID, fingerprint string) error`; all other Agent routes continue using `AuthenticateNode`.
- Extends: `web.NewAgent(store *platform.Store, signer platform.BootstrapSigner)`; a nil signer returns a bounded unavailable response only for renewal.

- [ ] **Step 1: Write failing store tests for issue/idempotency/concurrency**

  Assert current-fingerprint authorization, Ed25519 CSR enforcement, fixed SPIFFE identity, same-CSR byte-identical replay, different-CSR conflict, one pending row under concurrent calls, and no current-fingerprint change before activation.

- [ ] **Step 2: Run rotation store tests and confirm RED**

  Run: `node scripts/start.mjs test -race -run 'TestCertificateRotation' ./internal/platform`

  Expected: FAIL because rotation storage does not exist.

- [ ] **Step 3: Implement renewal transaction and strict signer behavior**

  Reuse the existing node lock, SHA-256 helpers, 32 KiB certificate bound, and CA signer. Restrict automatic renewal CSRs to Ed25519 while leaving existing manual enrollment compatibility unchanged.

- [ ] **Step 4: Write failing activation and manual-race tests**

  Cover lost activation response replay, immediate old-certificate rejection, pending-certificate rejection on non-activation endpoints, and cancellation by manual enroll/revoke/bootstrap redemption in the same node transaction.

- [ ] **Step 5: Run activation tests and confirm RED**

  Run: `node scripts/start.mjs test -race -run 'Test(CertificateActivation|CertificateRotationManual)' ./internal/platform`

  Expected: FAIL until activation/cancellation behavior exists.

- [ ] **Step 6: Implement activation and cancellation invariants**

  Activation atomically promotes the pending fingerprint, clears pending state, records non-sensitive platform audit, and never modifies config applying/WAL state. Manual identity operations delete pending rotation before commit.

- [ ] **Step 7: Write and implement Agent endpoint tests**

  Test `/agent/certificate/renew` with the current certificate and `/agent/certificate/activate` with only the pending/already-promoted certificate, including request limits and sanitized responses.

- [ ] **Step 8: Run focused tests and commit**

  Run: `node scripts/start.mjs test -race -run 'Test.*Certificate(Rotation|Activation|Renew)' ./internal/nodeauth ./internal/platform ./internal/web`

  Expected: PASS.

  Run: `git add internal/platform/certificate_rotation.go internal/platform/certificate_rotation_test.go internal/platform/migration19.sql internal/platform/admin_nodes.go internal/platform/bootstrap.go internal/platform/admin_test.go internal/platform/bootstrap_test.go internal/web/agent.go internal/web/agent_test.go internal/nodeauth/bootstrap.go internal/nodeauth/bootstrap_test.go && git commit -m 'feat: add two-phase node certificate renewal'`

### Task 4: Crash-Safe Local Credential Manager and Rotation State Machine

**Files:**
- Create: `internal/agent/credentials.go`
- Create: `internal/agent/credentials_test.go`
- Create: `internal/agent/rotation.go`
- Create: `internal/agent/rotation_test.go`
- Modify: `internal/agent/bootstrap.go`
- Modify: `internal/agent/bootstrap_test.go`
- Modify: `internal/nodeauth/pki.go`

**Interfaces:**
- Produces: `func OpenCredentialManager(pkiDir, nodeID string) (*CredentialManager, error)`.
- Produces: `func (m *CredentialManager) TLSConfig() (*tls.Config, error)`, `func (m *CredentialManager) Current() CertificateInfo`, and `func (m *CredentialManager) CloseIdleConnections()`.
- Produces: `type RotationAPI interface { Renew(context.Context, []byte) (platform.CertificateRotation, error); Activate(context.Context, *tls.Certificate) error }`.
- Produces: `func (m *CredentialManager) ReconcileRotation(context.Context, RotationAPI, time.Time) error`.
- Uses: a 0600 `identity.pem` current bundle plus separately persisted pending bundle/state, all written with temp file, file sync, atomic rename, and directory sync.

- [ ] **Step 1: Write failing identity-bundle tests**

  Test safe permissions, exact node identity, chain/key match, truncated bundle, wrong key, unsafe symlink/non-regular file, and atomic preservation of the prior usable identity when publication fails.

- [ ] **Step 2: Run credential tests and confirm RED**

  Run: `go test ./internal/agent -run 'TestCredentialManager'`

  Expected: FAIL because the manager does not exist.

- [ ] **Step 3: Implement current identity loading and atomic publication**

  Prefer `identity.pem` when present; support existing `agent.crt`/`agent.key` as read-only migration input. Never overwrite or delete legacy files.

- [ ] **Step 4: Write failing rotation phase/restart tests**

  Simulate restart before request, after persisted CSR, after signing, after activation commit with lost response, and before local publication. Assert the same key/CSR resumes, old identity remains usable until activation, and corrupt state returns a bounded fail-closed error.

- [ ] **Step 5: Run rotation state tests and confirm RED**

  Run: `go test ./internal/agent -run 'TestRotation'`

  Expected: FAIL because `ReconcileRotation` does not exist.

- [ ] **Step 6: Implement renewal client and persistent state machine**

  Start at seven days remaining, validate returned node URI/client usage/CA/time/key match, activate using the pending identity, then atomically publish and close idle connections. Map errors only to the approved heartbeat error codes.

- [ ] **Step 7: Update fresh bootstrap identity output**

  New installs create the canonical identity bundle in addition to existing compatibility files without weakening `O_EXCL`, fsync, or resumable enrollment behavior.

- [ ] **Step 8: Run Agent tests and commit**

  Run: `go test ./internal/agent ./internal/nodeauth`

  Expected: PASS.

  Run: `git add internal/agent/credentials.go internal/agent/credentials_test.go internal/agent/rotation.go internal/agent/rotation_test.go internal/agent/bootstrap.go internal/agent/bootstrap_test.go internal/nodeauth/pki.go && git commit -m 'feat: rotate agent credentials safely'`

### Task 5: Runtime Configuration and Agent Integration

**Files:**
- Modify: `internal/runtimeconfig/bootstrap.go`
- Modify: `internal/runtimeconfig/config.go`
- Modify: `internal/runtimeconfig/bootstrap_test.go`
- Modify: `internal/runtimeconfig/config_test.go`
- Modify: `cmd/reladock/main.go`
- Modify: `cmd/reladock-agent/main.go`
- Modify: `internal/agent/supervisor_test.go`
- Modify: `deploy/control.env.example`
- Modify: `deploy/agent.env.example`
- Modify: `deploy/reladock-agent.service`
- Modify: `deploy/reladock-agent-apply.service`

**Interfaces:**
- Produces: runtime configuration that loads node signing CA independently from pinned release metadata while preserving the existing all-or-none bootstrap installation contract.
- Consumes: Tasks 1-4 Supervisor, heartbeat, signer, and CredentialManager interfaces.

- [ ] **Step 1: Write failing runtime-configuration tests**

  Assert production Agent listener + missing signer is rejected, development can run mTLS without renewal, signer-only plus no release metadata enables renewal but not bootstrap commands, and partial signer settings fail.

- [ ] **Step 2: Run runtime config tests and confirm RED**

  Run: `go test ./internal/runtimeconfig`

  Expected: FAIL because signer and Release configuration are still coupled.

- [ ] **Step 3: Separate signer and Release configuration**

  Keep existing environment variable names for compatibility. Pass the signer to `web.NewAgent`; only pass full release metadata to browser bootstrap handlers.

- [ ] **Step 4: Write failing integrated Supervisor tests**

  Assert configuration, heartbeat, engine tick, preflight, and renewal are independently scheduled; a failed probe/heartbeat/renewal does not skip local observe/WAL work or config polling; `--once` performs no automatic rotation surprise.

- [ ] **Step 5: Run integrated Agent tests and confirm RED**

  Run: `go test ./internal/agent ./cmd/reladock-agent`

  Expected: FAIL until the main program uses the new components.

- [ ] **Step 6: Integrate CredentialManager and Supervisor**

  Load credentials before constructing transport, report the current certificate expiry/state, schedule renewal independently, preserve existing timeouts, and close resources on signal.

- [ ] **Step 7: Update deployment templates and run focused tests**

  Run: `go test ./internal/runtimeconfig ./internal/agent ./cmd/reladock-agent ./cmd/reladock`

  Expected: PASS.

- [ ] **Step 8: Commit**

  Run: `git add internal/runtimeconfig/bootstrap.go internal/runtimeconfig/config.go internal/runtimeconfig/bootstrap_test.go internal/runtimeconfig/config_test.go cmd/reladock/main.go cmd/reladock-agent/main.go internal/agent/supervisor_test.go deploy/control.env.example deploy/agent.env.example deploy/reladock-agent.service deploy/reladock-agent-apply.service && git commit -m 'feat: integrate automatic agent renewal'`

### Task 6: Admin Status, Certificate Incidents, Documentation, and Full Verification

**Files:**
- Modify: `internal/platform/operations.go`
- Modify: `internal/platform/operations_test.go`
- Modify: `internal/platform/notifications_test.go`
- Modify: `public/admin.html`
- Modify: `public/admin.js`
- Modify: `public/node-contact-status.js`
- Modify: `test/node-contact-status.test.mjs`
- Modify: `public/assets_test.go`
- Modify: `README.md`
- Modify: `docs/ARCHITECTURE.md`
- Modify: `docs/ROADMAP.md`
- Modify: `docs/DEPLOYMENT.md`
- Modify: `docs/OPERATIONS.md`

**Interfaces:**
- Extends: incident kinds with `certificate-expiring`, `certificate-expired`, and `certificate-rotation-failed` using the existing transactional notification Outbox.
- Extends: admin presentation with independent contact, Agent, certificate, configuration, and probe facts.

- [ ] **Step 1: Write failing incident reconciliation tests**

  Assert seven-day warning, expiry, one-hour continuous bounded-error threshold, deduplication, resolution, recurrence, unenrolled-node behavior, and transactional audit/notification rollback.

- [ ] **Step 2: Run operation/notification tests and confirm RED**

  Run: `node scripts/start.mjs test -race -run 'Test.*Certificate.*Incident' ./internal/platform`

  Expected: FAIL because certificate incidents are not reconciled.

- [ ] **Step 3: Implement certificate incident reconciliation**

  Reuse `operations_incidents`, platform audit, and notification triggers. Never include certificate, CSR, fingerprint, private key, host credentials, or raw local error text in notification payloads.

- [ ] **Step 4: Write failing UI presentation tests**

  Add Node tests for online/from-never/offline wording, auto-reconnect as explanatory text rather than confirmed state, certificate normal/expiring/expired/rotating/error labels, and separate “健康未知” configuration/probe wording.

- [ ] **Step 5: Run UI tests and confirm RED**

  Run: `node --test test/node-contact-status.test.mjs`

  Expected: FAIL until the presentation helpers expose the new states.

- [ ] **Step 6: Implement admin status presentation**

  Render only fields returned by `AdminNodes`; do not expose certificate fingerprints beyond the existing explicit certificate-management field, and never expose certificate bodies or CSR data.

- [ ] **Step 7: Update project and operations documentation**

  Record schema v19, 30-day/7-day renewal behavior, production signer requirement, manual rotation cancellation, independent backoff, factual status meanings, recovery boundaries, and the remaining isolated-VPS acceptance requirement.

- [ ] **Step 8: Run full verification**

  Run: `npm test`

  Expected: all Go race/integration tests and Node tests PASS against the dedicated test PostgreSQL schema.

  Run: `npm run vet`

  Expected: PASS with no diagnostics.

  Run: `git status --short`

  Expected: only the planned documentation/source/test changes are present before the final commit.

- [ ] **Step 9: Commit**

  Run: `git add internal/platform/operations.go internal/platform/operations_test.go internal/platform/notifications_test.go public/admin.html public/admin.js public/node-contact-status.js test/node-contact-status.test.mjs public/assets_test.go README.md docs/ARCHITECTURE.md docs/ROADMAP.md docs/DEPLOYMENT.md docs/OPERATIONS.md docs/superpowers/specs/2026-10-31-agent-long-running-reliability-design.md docs/superpowers/plans/2026-10-31-agent-long-running-reliability.md && git commit -m 'feat: surface agent certificate health'`

## Completion Boundary

The implementation is complete only when the full test suite and vet pass, a fresh review finds no critical correctness or security issue, and the final report clearly distinguishes local implementation from unperformed isolated-VPS and production deployment validation. Do not create a GitHub Release, deploy to `103.30.78.5` or `103.30.78.63`, or change live network rules under this plan.
