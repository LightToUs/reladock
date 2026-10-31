# Agent Safe Upgrade Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add administrator-requested, fixed-release Agent/Dataplane upgrades with checksum verification, crash recovery, and automatic binary rollback.

**Architecture:** PostgreSQL owns the upgrade state machine and only exposes a control-configured Release target. A separate root `reladock-updater` claims work over node mTLS, verifies a fixed GitHub Release, atomically swaps only the Agent and Dataplane binaries, and rolls back unless the new Agent proves both heartbeat and configuration polling. Existing nodes receive the updater once through the reviewed installer; subsequent upgrades never execute arbitrary commands or modify network state.

**Tech Stack:** Go 1.24+, PostgreSQL, net/http with TLS 1.3 mTLS, systemd, POSIX shell installers, Node.js release tooling, vanilla admin UI.

**Spec:** `docs/superpowers/specs/2026-10-31-agent-safe-upgrade-design.md`

## Global Constraints

- Payment behavior and accounting semantics remain unchanged.
- The administrator cannot submit a URL, command, path, checksum, architecture, or target version.
- The only automatic target is the complete `BootstrapConfig` fixed Release for `LightToUs/reladock`.
- Production versions are strict `vMAJOR.MINOR.PATCH`; only increasing versions with the same major are eligible; `dev` is never eligible.
- At most one upgrade is active globally and at most one is active per node.
- The updater replaces only `/opt/reladock/reladock-agent` and `/opt/reladock/reladock-dataplane`.
- The updater preserves PKI, Agent journals, WAL, metering, shaping, and network state, and never invokes a shell.
- A successful upgrade means target-version heartbeat plus one successful configuration poll; it does not mean end-to-end forwarding health.
- Existing production VPSs are not modified or contacted by implementation or test commands.

## Review Focus

- A valid archive with an extra file, duplicate entry, link, path traversal, oversized entry, or wrong inner checksum must be rejected before any installed file changes (Task 4 tests).
- A crash after one binary is published must resume from persisted hashes and restore both old binaries rather than mixing versions (Task 5 tests).
- An expired pre-install lease may be reclaimed, but an `installing` or `verifying` task must never be handed to a second executor (Task 2 tests).
- A forged, stale, mismatched-version, or mismatched-challenge health marker must not complete an upgrade (Task 6 tests).
- A concurrent configuration application or persisted delivery journal must prevent replacement and leave the old Agent running (Task 5 tests).

---

### Task 1: Shared Build Version

**Files:**
- Create: `internal/buildinfo/buildinfo.go`
- Create: `internal/buildinfo/buildinfo_test.go`
- Modify: `cmd/reladock-agent/main.go`
- Modify: `internal/agent/heartbeat_test.go`
- Modify: `scripts/release.mjs`
- Modify: `package.json`

**Interfaces:**
- Consumes: package version from `package.json` during Release builds.
- Produces: `buildinfo.Current() string`, `buildinfo.ParseProduction(string) (major int, minor int, patch int, ok bool)`, and `buildinfo.CanUpgrade(current, target string) bool`.

- [ ] **Step 1: Write failing build-version tests**

Add table tests proving the default is `dev`, strict versions accept `v0.9.0`, prereleases and prefixes are rejected for automatic upgrade, and `CanUpgrade` requires the same major and a strictly greater target.

- [ ] **Step 2: Run the focused tests and verify failure**

Run: `go test ./internal/buildinfo ./internal/agent`

Expected: FAIL because `internal/buildinfo` and the new pure-version heartbeat behavior do not exist.

- [ ] **Step 3: Implement the build information contract**

Define `var ReleaseVersion = "dev"`; make `Current` return only `dev` or a strict production version, and make the Agent heartbeat/header use `buildinfo.Current()` while mode remains solely in `AgentHeartbeat.Mode`. Inject `-X reladock/internal/buildinfo.ReleaseVersion=v${packageVersion}` into every Go Release build and bump `package.json` to `0.9.0`.

- [ ] **Step 4: Run focused tests and release-tool tests**

Run: `go test ./internal/buildinfo ./internal/agent && node --test test/release-package.test.mjs`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/buildinfo cmd/reladock-agent/main.go internal/agent/heartbeat_test.go scripts/release.mjs package.json
git commit -m "feat: add authoritative build versions"
```

### Task 2: Upgrade State Machine and Persistence

**Files:**
- Create: `internal/platform/migration21.sql`
- Create: `internal/platform/node_upgrades.go`
- Create: `internal/platform/node_upgrades_test.go`
- Modify: `internal/platform/store.go`
- Modify: `internal/platform/admin_nodes.go`
- Modify: `internal/platform/heartbeat_test.go`

**Interfaces:**
- Consumes: `buildinfo.CanUpgrade`, authenticated `platform.Identity`, node fingerprint, node revision, and `platform.ReleaseTarget{Version, AMD64SHA256, ARM64SHA256}`.
- Produces: `AgentUpgrade`, `UpgradeClaim`, `UpgradeReport`, `RequestAgentUpgrade`, `CancelAgentUpgrade`, `ClaimAgentUpgrade`, and `ReportAgentUpgrade` store methods; `ManagedNode.Upgrade *AgentUpgradeSummary`.

- [ ] **Step 1: Write failing migration and state-machine tests**

Cover: global and per-node active uniqueness; admin authorization; online/non-maintenance/current-version gates; revision conflict; same-node/same-target request replay; audit rollback; architecture selection; expired `claimed` reclaim; no reclaim after `installing`; allowed edges; terminal immutability; lease mismatch; bounded error codes; and `succeeded` requiring the node's stored heartbeat version to equal the target.

- [ ] **Step 2: Run the focused tests and verify failure**

Run: `go test ./internal/platform -run 'AgentUpgrade|Migration'`

Expected: FAIL because schema version 21 and upgrade methods do not exist.

- [ ] **Step 3: Add migration 21 and typed state transitions**

Create `node_agent_upgrades` with UUID identity, target hashes, previous version, status check, lease digest/expiry, timestamps, bounded error code, and partial unique indexes for active rows. Increment `SchemaVersion`, embed/apply migration 21, and keep every request/report/audit transition transactional.

- [ ] **Step 4: Expose latest upgrade facts with managed nodes**

Extend `AdminNodes` with a lateral latest-upgrade lookup returning only task ID, target/current version, state, error code, and public timestamps. Do not expose lease material or local paths.

- [ ] **Step 5: Run the platform tests**

Run: `go test ./internal/platform`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/platform/migration21.sql internal/platform/node_upgrades.go internal/platform/node_upgrades_test.go internal/platform/store.go internal/platform/admin_nodes.go internal/platform/heartbeat_test.go
git commit -m "feat: persist agent upgrade state machine"
```

### Task 3: Administrator and mTLS Upgrade APIs

**Files:**
- Create: `internal/web/upgrades_http_test.go`
- Modify: `internal/web/server.go`
- Modify: `internal/web/admin.go`
- Modify: `internal/web/agent.go`
- Modify: `cmd/reladock/main.go`

**Interfaces:**
- Consumes: Task 2 store methods and `BootstrapOptions.Config` fixed Release metadata.
- Produces: `POST /api/admin/nodes/{id}/upgrade`, `POST /api/admin/nodes/{id}/upgrade-cancel`, `POST /agent/upgrade/claim`, and `POST /agent/upgrade/report`.

- [ ] **Step 1: Write failing HTTP boundary tests**

Assert platform-admin-only access, strict `{revision}` bodies, unavailable response without complete fixed Release metadata, no client-selected target fields, mTLS node binding, strict architecture/current-version claim JSON, strict task/lease/state report JSON, wrong-node denial, exact replay success, conflicting replay rejection, and no browser Origin on Agent endpoints.

- [ ] **Step 2: Run focused web tests and verify failure**

Run: `go test ./internal/web -run 'Upgrade'`

Expected: FAIL with missing routes.

- [ ] **Step 3: Wire the administrator routes to fixed server metadata**

The handler derives `platform.ReleaseTarget` exclusively from `BootstrapOptions.Config`; it accepts no target data from JSON and returns the persisted task summary.

- [ ] **Step 4: Wire mTLS claim and report routes**

Reuse the existing certificate identity, fingerprint authorization, request timeout, rate limit, strict decoder, and no-Origin policy. Extend `NewAgent` wiring so the mTLS handler receives the same validated fixed Release configuration where needed, without returning an arbitrary URL.

- [ ] **Step 5: Run web and command tests**

Run: `go test ./internal/web ./cmd/reladock`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/web/upgrades_http_test.go internal/web/server.go internal/web/admin.go internal/web/agent.go cmd/reladock/main.go
git commit -m "feat: add controlled upgrade APIs"
```

### Task 4: Updater Client and Release Verifier

**Files:**
- Create: `internal/updater/client.go`
- Create: `internal/updater/client_test.go`
- Create: `internal/updater/archive.go`
- Create: `internal/updater/archive_test.go`

**Interfaces:**
- Consumes: node mTLS `*http.Client`, fixed control origin, claim/report DTOs from Task 2, and Release tag/digest.
- Produces: `Client.Claim(context.Context, arch, current string) (*platform.UpgradeClaim, error)`, `Client.Report(context.Context, platform.UpgradeReport) error`, and `VerifyArchive(path, expectedSHA256, destination string) (StagedBinaries, error)`.

- [ ] **Step 1: Write failing client and hostile-archive tests**

Client tests pin POST paths, strict JSON, no redirects, response-size bounds, 204 behavior, and bounded server errors. Archive tests cover valid amd64/arm64 packages plus outer mismatch, traversal, nested paths, duplicate names, missing/extra names, symlink/hardlink/device/directory entries, decompression bounds, bad `SHA256SUMS`, and non-regular staged binaries.

- [ ] **Step 2: Run focused updater tests and verify failure**

Run: `go test ./internal/updater -run 'Client|Archive'`

Expected: FAIL because the package does not exist.

- [ ] **Step 3: Implement the mTLS client without redirects**

Use only the injected HTTP client and fixed control origin. Never accept or construct a server-provided URL; derive the GitHub asset URL from the constant repository, validated target tag, and normalized local architecture.

- [ ] **Step 4: Implement streaming, bounded archive verification**

Read gzip/tar in Go without executing `tar` or a shell. Require the exact Release allowlist, unique flat regular entries, bounded per-file and total extracted bytes, exact outer SHA256, exact package `SHA256SUMS`, and 0755 staged Agent/Dataplane files.

- [ ] **Step 5: Run updater package tests**

Run: `go test ./internal/updater`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/updater/client.go internal/updater/client_test.go internal/updater/archive.go internal/updater/archive_test.go
git commit -m "feat: verify fixed updater releases"
```

### Task 5: Crash-Safe Binary Installation and Rollback

**Files:**
- Create: `internal/updater/state.go`
- Create: `internal/updater/state_test.go`
- Create: `internal/updater/system.go`
- Create: `internal/updater/system_test.go`
- Create: `internal/updater/run.go`
- Create: `internal/updater/run_test.go`
- Create: `cmd/reladock-updater/main.go`
- Create: `cmd/reladock-updater/main_test.go`

**Interfaces:**
- Consumes: Task 4 client/verifier, `/var/lib/reladock-updater`, `/opt/reladock`, two fixed systemd unit names, and `/var/lib/reladock-agent/control`.
- Produces: `updater.Run(context.Context, Options) error`, persisted `State`, `System` interface for file/service operations, and the root updater command.

- [ ] **Step 1: Write failing lifecycle, crash, and rollback tests**

Use a fake `System` and temporary files to test no-task exit, one local executor lock, zero/two active services, graceful stop, journal-lock contention, persisted delivery refusal, backup hashes/fsync, atomic two-binary publication, restart failure, 120-second health timeout, successful rollback, rollback failure retention, and restart recovery at every persisted phase. Include the Review Focus cases for mixed-version crash recovery and active configuration application.

- [ ] **Step 2: Run focused lifecycle tests and verify failure**

Run: `go test ./internal/updater ./cmd/reladock-updater -run 'Run|Recovery|Rollback|Command'`

Expected: FAIL with missing lifecycle implementation.

- [ ] **Step 3: Implement private durable state and fixed system operations**

State writes use temporary regular files, `fsync`, rename, and parent-directory `fsync`; reject unsafe ownership/mode, symlinks, oversize data, unknown phases, and hash mismatches. The production `System` invokes `systemctl` directly with fixed argument arrays only and performs file operations without a shell.

- [ ] **Step 4: Implement the resumable lifecycle**

Persist before each irreversible step. Stop exactly one known Agent unit, acquire/check the existing control journal lock after stop, refuse a remaining delivery journal, back up both binaries, publish both, project the health request, start the original unit, report `verifying`, then either report `succeeded` or restore both backups and report `rolled-back`; preserve state on rollback failure.

- [ ] **Step 5: Implement the updater command boundary**

Read fixed paths/defaults plus existing root-managed Agent environment and PKI, normalize `runtime.GOARCH`, construct TLS 1.3 mTLS transport, set bounded HTTP/download timeouts, and reject positional or arbitrary execution inputs.

- [ ] **Step 6: Run updater and command tests**

Run: `go test ./internal/updater ./cmd/reladock-updater`

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/updater cmd/reladock-updater
git commit -m "feat: add crash-safe agent updater"
```

### Task 6: New-Agent Health Proof

**Files:**
- Create: `internal/agent/upgrade_health.go`
- Create: `internal/agent/upgrade_health_test.go`
- Modify: `cmd/reladock-agent/main.go`
- Modify: `internal/agent/supervisor_test.go`
- Modify: `deploy/reladock-agent.service`
- Modify: `deploy/reladock-agent-apply.service`

**Interfaces:**
- Consumes: read-only `/run/reladock-updater/health-request.json`, writable `/run/reladock-agent`, `buildinfo.Current()`, and results of the existing heartbeat/configuration hooks.
- Produces: `agent.UpgradeHealth` with `HeartbeatSucceeded()` and `ConfigurationSucceeded()`; a marker containing only task ID, challenge, version, and completion time.

- [ ] **Step 1: Write failing health-proof tests**

Test missing/unsafe/oversized/trailing/unknown-field requests, strict UUID/challenge/version validation, either hook arriving first, failed hooks not counting, stale task replacement resetting progress, atomic marker publication, and rejection of stale/mismatched marker by the updater.

- [ ] **Step 2: Run focused tests and verify failure**

Run: `go test ./internal/agent ./internal/updater -run 'UpgradeHealth|HealthMarker'`

Expected: FAIL because health tracking does not exist.

- [ ] **Step 3: Implement the two-signal tracker and hook integration**

After each successful heartbeat and each successful configuration poll (including a no-work 204 poll), record the signal for the current task/challenge/version. Publish only after both signals match the current target version. Do not treat process start or systemd active state as a signal.

- [ ] **Step 4: Harden service access to runtime health paths**

Add only the minimum runtime-directory read/write permissions needed by both validation and apply units; keep `ProtectSystem=strict`, non-root Agent execution, and existing network/capability restrictions.

- [ ] **Step 5: Run Agent, updater, and supervisor tests**

Run: `go test ./internal/agent ./internal/updater ./cmd/reladock-agent`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/agent/upgrade_health.go internal/agent/upgrade_health_test.go cmd/reladock-agent/main.go internal/agent/supervisor_test.go deploy/reladock-agent.service deploy/reladock-agent-apply.service
git commit -m "feat: prove upgraded agent readiness"
```

### Task 7: Release Packaging and One-Time Updater Installation

**Files:**
- Create: `deploy/reladock-updater.service`
- Create: `deploy/reladock-updater.timer`
- Modify: `deploy/install-linux.sh`
- Modify: `deploy/bootstrap-agent.sh`
- Modify: `scripts/release.mjs`
- Modify: `scripts/release-package.mjs`
- Modify: `test/release-package.test.mjs`
- Modify: `test/bootstrap-install.test.mjs`
- Modify: `test/bootstrap-linux.test.mjs`
- Modify: `internal/web/bootstrap_test.go`
- Modify: `docs/DEPLOYMENT.md`

**Interfaces:**
- Consumes: Task 5 updater binary and fixed service paths.
- Produces: Release assets containing updater binary/unit/timer; new-node install enables the timer; reviewed manual agent install lays down but does not silently switch Agent mode.

- [ ] **Step 1: Write failing package and installer tests**

Assert exact archive allowlist/checksums include the updater assets, Release version injection is present, bootstrap rejects archives missing/adding updater files, manual agent install preserves environment/PKI and Agent mode, and updater service hardening grants only required paths/capabilities. Keep privileged Docker installer coverage opt-in and report it separately.

- [ ] **Step 2: Run release and bootstrap tests and verify failure**

Run: `node --test test/release-package.test.mjs test/bootstrap-install.test.mjs test/bootstrap-linux.test.mjs && go test ./internal/web -run 'Bootstrap'`

Expected: FAIL because updater assets are absent.

- [ ] **Step 3: Add hardened oneshot service and timer**

The service runs as root only because it must replace root-owned binaries and control fixed units; use a private state directory, strict filesystem protections with explicit write paths, no network-administration capabilities, and a timer with bounded randomized delay.

- [ ] **Step 4: Extend both installation paths and package metadata**

Install updater binary/unit/timer only from the already verified package, daemon-reload, and enable the timer for a newly bootstrapped node. The reviewed `install-linux.sh agent` path supports one-time installation on an existing node without overwriting env/PKI or enabling apply mode.

- [ ] **Step 5: Run package, bootstrap, and non-privileged installer tests**

Run: `node --test test/release-package.test.mjs test/bootstrap-install.test.mjs test/bootstrap-linux.test.mjs && go test ./internal/web ./deploy/...`

Expected: PASS, with the privileged Docker/systemd installer test explicitly skipped unless its environment flag is enabled.

- [ ] **Step 6: Commit**

```bash
git add deploy/reladock-updater.service deploy/reladock-updater.timer deploy/install-linux.sh deploy/bootstrap-agent.sh scripts/release.mjs scripts/release-package.mjs test/release-package.test.mjs test/bootstrap-install.test.mjs test/bootstrap-linux.test.mjs internal/web/bootstrap_test.go docs/DEPLOYMENT.md
git commit -m "feat: package and install agent updater"
```

### Task 8: Administrator Upgrade Presentation and Final Documentation

**Files:**
- Modify: `public/admin.html`
- Modify: `public/admin.js`
- Modify: `public/admin.css`
- Modify: `internal/web/static_assets_test.go`
- Modify: `README.md`
- Modify: `docs/REQUIREMENTS.md`
- Modify: `docs/ARCHITECTURE.md`
- Modify: `docs/ROADMAP.md`
- Modify: `docs/DEPLOYMENT.md`

**Interfaces:**
- Consumes: `ManagedNode.Upgrade`, node revision, and Task 3 administrator endpoints.
- Produces: node-list version/upgrade facts, guarded upgrade/cancel controls, and truthful operations documentation.

- [ ] **Step 1: Write failing static-asset contract tests**

Pin the presence of current/target version, localized status/error mapping, upgrade action, pre-install cancel action, disabled in-progress controls, and copy stating that success is control connectivity/config polling rather than forwarding health.

- [ ] **Step 2: Run static tests and verify failure**

Run: `go test ./internal/web -run 'Static|Admin'`

Expected: FAIL because upgrade presentation is missing.

- [ ] **Step 3: Add concise node-list upgrade facts and actions**

Render current version, target, state, last update, and bounded error code. Show “升级到当前固定版本” only when eligible; require confirmation; send only the node revision; allow cancel only for `queued`/`claimed`; reload after conflict. Preserve the existing distinction among online, configuration, application, probing, and end-to-end health.

- [ ] **Step 4: Update project and operations documentation**

Document the manual one-time updater rollout, fixed-target security boundary, automatic rollback semantics, persistent state compatibility contract, operational recovery for stuck/rollback-failed tasks, and remaining unverified real-VPS/apply-mode acceptance. Mark the feature implemented but not production-ready until off-machine recovery and live acceptance phases complete.

- [ ] **Step 5: Run full verification**

Run: `npm test`

Expected: all Go race tests and Node tests PASS; any privileged Linux installer test is reported as skipped, not silently counted as production validation.

Run: `npm run vet`

Expected: PASS.

- [ ] **Step 6: Review branch diff and commit**

```bash
git diff --check
git status --short
git add public/admin.html public/admin.js public/admin.css internal/web/static_assets_test.go README.md docs/REQUIREMENTS.md docs/ARCHITECTURE.md docs/ROADMAP.md docs/DEPLOYMENT.md
git commit -m "feat: expose safe agent upgrades"
```

