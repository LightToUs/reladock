# Encrypted Offsite Recovery Implementation Plan

> **For Codex:** REQUIRED SUB-SKILL: Use executing-plans to implement this plan task-by-task.

**Goal:** Add encrypted, no-overwrite control-plane and node recovery bundles that can be verified and staged without modifying live state.

**Architecture:** A new statically linked `reladock-recovery` Go binary owns a versioned manifest and safe tar format, invokes an absolute external `age` binary without a shell, and exposes separate control and node workflows. Control verification restores only into a generated temporary PostgreSQL database; node verification extracts only into a newly created staging directory. Promotion to a replacement host remains an explicit operator procedure.

**Tech Stack:** Go 1.24 standard library, pgx v5 for verification queries, PostgreSQL command-line clients, external age CLI, Node.js release packaging, Markdown operations documentation.

---

### Task 1: Define the bundle manifest and safe archive contract

**Files:**
- Create: `internal/recovery/manifest.go`
- Create: `internal/recovery/manifest_test.go`
- Create: `internal/recovery/archive.go`
- Create: `internal/recovery/archive_test.go`

**Step 1: Write failing tests**

Cover deterministic manifest encoding, SHA-256 validation, kind/version mismatch, duplicate and unexpected entries, traversal and absolute paths, symlink/device/hard-link rejection, entry-count and byte limits, mode preservation, and staging-directory no-overwrite behavior.

**Step 2: Run the focused tests and confirm failure**

Run: `go test ./internal/recovery -run 'TestManifest|TestArchive'`

Expected: FAIL because the recovery package does not exist.

**Step 3: Implement the minimum safe format**

Add a canonical JSON manifest with explicit format version, bundle kind, creation time, source identifier, declared invariants, sorted file records, mode, size and SHA-256. Build and read tar streams directly in Go, accepting only declared regular files and directories and enforcing fixed limits before writing.

**Step 4: Run focused tests**

Run: `go test ./internal/recovery -run 'TestManifest|TestArchive'`

Expected: PASS.

**Step 5: Commit**

Commit: `feat: define safe recovery bundles`

### Task 2: Add encrypted no-overwrite bundle I/O

**Files:**
- Create: `internal/recovery/age.go`
- Create: `internal/recovery/age_test.go`

**Step 1: Write failing tests**

Use a fake executable to cover absolute executable enforcement, supported recipient validation, identity-file checks, bounded stderr, failed encryption cleanup, destination-directory symlink rejection, random no-overwrite output names, mode 0600 and successful file sync.

**Step 2: Run the focused tests and confirm failure**

Run: `go test ./internal/recovery -run 'TestAge|TestEncrypted'`

Expected: FAIL because encrypted bundle I/O is absent.

**Step 3: Implement encrypted streaming**

Invoke `age` with `exec.CommandContext` and fixed arguments, stream tar plaintext through pipes instead of persisting a final plaintext bundle, create output with exclusive mode, sync file and directory, and remove only the task-owned partial output on failure.

**Step 4: Run focused tests**

Run: `go test ./internal/recovery -run 'TestAge|TestEncrypted'`

Expected: PASS.

**Step 5: Commit**

Commit: `feat: encrypt recovery bundles safely`

### Task 3: Implement control-plane creation and isolated verification

**Files:**
- Create: `internal/recovery/control.go`
- Create: `internal/recovery/control_test.go`
- Create: `internal/recovery/postgres.go`
- Create: `internal/recovery/postgres_test.go`

**Step 1: Write failing tests**

Cover safe `DATABASE_URL` parsing, secrets absent from arguments/errors/manifests, absolute PostgreSQL tool paths, custom-format dump failure cleanup, generated temporary database names, restore failure cleanup, source database rejection, schema-version mismatch, wallet/ledger invariant mismatch and successful temporary database removal.

**Step 2: Run the focused tests and confirm failure**

Run: `go test ./internal/recovery -run 'TestControl|TestPostgres'`

Expected: FAIL because control recovery is absent.

**Step 3: Implement control workflows**

Collect schema and non-secret invariant metadata from the source database, run `pg_dump --format=custom` with libpq environment values, package the dump, and verify by decrypting then restoring into a generated temporary database. Query the restored database through pgx, compare declared invariants, and drop only the generated database in a deferred cleanup.

**Step 4: Run focused and real-database tests**

Run: `go test ./internal/recovery -run 'TestControl|TestPostgres'`

Run opt-in integration coverage through the project test harness when PostgreSQL clients and `age` are available.

Expected: unit tests PASS; unavailable external tools skip only the opt-in integration case.

**Step 5: Commit**

Commit: `feat: verify encrypted control backups`

### Task 4: Implement quiesced node collection and staging-only verification

**Files:**
- Create: `internal/recovery/node.go`
- Create: `internal/recovery/node_test.go`

**Step 1: Write failing tests**

Cover Linux/root gating, both Agent services and updater inactivity, non-blocking control-lock acquisition, fixed source roots, optional updater state, source symlink/device/hard-link rejection, live-path staging rejection, existing staging rejection, and proof that verification neither starts services nor writes outside staging.

**Step 2: Run the focused tests and confirm failure**

Run: `go test ./internal/recovery -run 'TestNode'`

Expected: FAIL because node recovery is absent.

**Step 3: Implement node workflows**

Add injectable operating-system probes for tests, acquire the existing lock before enumeration, collect only whitelisted disk state under stable logical archive names, encrypt it, and verify/extract only to a newly created absolute staging directory.

**Step 4: Run focused tests**

Run: `go test ./internal/recovery -run 'TestNode'`

Expected: PASS.

**Step 5: Commit**

Commit: `feat: stage encrypted node recovery`

### Task 5: Add the operator CLI and release packaging

**Files:**
- Create: `cmd/reladock-recovery/main.go`
- Create: `cmd/reladock-recovery/main_test.go`
- Modify: `scripts/release.mjs`
- Modify: `scripts/release-package.mjs`
- Modify: `deploy/install-linux.sh`
- Modify: `package.json`

**Step 1: Write failing tests**

Cover the four commands, unknown/missing options, secret-free errors, production-safe defaults, distinct exit failures, and release/install inclusion.

**Step 2: Run focused tests and confirm failure**

Run: `go test ./cmd/reladock-recovery`

Run: `node --test scripts/*.test.mjs`

Expected: FAIL because the command and release entry are absent.

**Step 3: Implement CLI and packaging**

Wire `control-create`, `control-verify`, `node-create`, and `node-verify`; add explicit absolute tool-path flags; package and install `reladock-recovery`; expose a local `npm run recovery` helper without changing the existing local-only backup command.

**Step 4: Run focused tests and build the release**

Run: `go test ./cmd/reladock-recovery`

Run: `node --test scripts/*.test.mjs`

Run: `npm run build:release`

Expected: PASS and the release manifest includes `reladock-recovery`.

**Step 5: Commit**

Commit: `feat: ship recovery operator tooling`

### Task 6: Document and verify the disaster-recovery drill

**Files:**
- Create: `docs/acceptance/OFFSITE_RECOVERY.md`
- Modify: `README.md`
- Modify: `docs/ARCHITECTURE.md`
- Modify: `docs/DEPLOYMENT.md`
- Modify: `docs/ROADMAP.md`
- Modify: `scripts/release.mjs`

**Step 1: Add the acceptance runbook**

Document key custody, encrypted bundle replication, retention, control verification, node staging, replacement-host promotion, validation-before-apply, rollback boundaries, evidence capture and measured RPO/RTO. State explicitly that local verification does not prove offsite durability or production recovery.

**Step 2: Update product documentation and release truthfulness**

Describe the implemented tooling and keep off-host storage, key custody, measured RPO/RTO and a real replacement-host drill in the unverified release list.

**Step 3: Run complete verification**

Run: `npm run vet`

Run: `npm test`

Run: `npm run build:release`

Run: `git diff --check`

Expected: all automated checks PASS; privileged node drill and real offsite restore remain explicitly unverified unless separately authorized and executed.

**Step 4: Request independent code review and fix findings**

Review archive extraction, subprocess secret handling, temporary database cleanup, live-path isolation and release truthfulness. Add regressions for every accepted finding.

**Step 5: Commit**

Commit: `docs: add offsite recovery drill`
