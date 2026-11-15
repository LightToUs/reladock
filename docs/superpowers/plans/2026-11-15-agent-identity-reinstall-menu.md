# Agent Identity Reinstall Menu Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a safe interactive reinstall action that replaces a stale validation-only Agent identity with the bootstrap command's intended node identity while retaining a recoverable copy of all prior local state.

**Architecture:** Extend the existing `/dev/tty` menu and `--existing-action` parser with `reinstall`. Download and fully verify the pinned release before changing the host, then stop and disable validation/updater services, create a root-only persistent backup, remove only the old identity and node-bound runtime state, and enter the existing fresh enrollment path so the token remains on stdin. Refuse apply-capable installations and preserve any new partial enrollment after the enrollment boundary instead of restoring an obsolete identity.

**Tech Stack:** POSIX shell, systemd, Node test runner, isolated Docker installer fixture.

**Spec:** `docs/DEPLOYMENT.md`

## Global Constraints

- Never clear PKI, WAL, journal, or updater state without first creating and verifying a root-only persistent backup.
- Never offer identity reinstall while apply is active, enabled, configured, or has shaping configuration present.
- Never modify nftables, tc, routes, offload, forwarding, cloud security groups, or unrelated firewall state.
- Bootstrap tokens remain on stdin and reinstall selection remains on `/dev/tty`.
- A failed or uncertain new enrollment retains its new private enrollment state and the old backup for documented recovery.

## Review Focus

- A piped token must still reach enrollment after `/dev/tty` menu selection.
- Apply-active, apply-enabled, or apply-configured hosts must fail before any backup or state mutation.
- Release download or checksum failure must leave the existing Agent untouched.
- Backup failure must leave the existing Agent identity and service state recoverable.
- Enrollment response loss must retain the new CSR/key state rather than restoring the obsolete identity.

---

### Task 1: Reinstall Regression Tests

**Files:**
- Test: `test/bootstrap-linux.test.mjs`

**Interfaces:**
- Consumes: current Docker fixture and `--existing-action upgrade|none` behavior.
- Produces: executable expectations for `--existing-action reinstall` and the three-item TTY menu.

- [x] Add a failing test that starts from a complete validation installation, writes old journal/updater markers, selects reinstall with a piped token, and asserts a second enrollment, a root-only backup, clean new node-bound state, active/enabled validation, disabled apply, and active updater timer.
- [x] Add failing refusal tests for apply active/enabled/configured state and confirm no metadata request or backup mutation.
- [x] Run `RELADOCK_INSTALLER_DOCKER_TESTS=1 node --test test/bootstrap-linux.test.mjs` and verify the new expectations fail for missing reinstall support.

### Task 2: Safe Identity Reinstall

**Files:**
- Modify: `deploy/bootstrap-agent.sh`

**Interfaces:**
- Consumes: existing complete-install detection, verified release directory, stdin bootstrap token, fresh enrollment path.
- Produces: `--existing-action reinstall` and menu action `2`, with no-action moved to `3`.

- [x] Parse `reinstall`, update the TTY menu, and leave default/invalid input as no action.
- [x] Before metadata access, reject reinstall when apply is active, enabled, configured in `agent.env`, or `/etc/reladock/shaping.json` exists.
- [x] After release verification, stop/disable validation and updater units, create `/var/backups/reladock/agent-reinstall.XXXXXX` mode `0700`, copy old env/PKI/binaries/units and present Agent/updater/lock state with preserved metadata, and verify the copies exist.
- [x] Remove only old `agent.env`, PKI, Agent state, updater state, and volatile runtime directories, then reuse the fresh install/enrollment path.
- [x] Before enrollment, restore the backup and prior service enable/activity state on preparation failure; after enrollment begins, retain new partial state and print both recovery locations on failure.
- [x] Run the focused installer test until green, then run `sh -n`, `npm run vet`, and the complete PostgreSQL-backed test suite.

### Task 3: Documentation and Production Installer Deployment

**Files:**
- Modify: `README.md`
- Modify: `docs/DEPLOYMENT.md`
- Modify: `docs/ROADMAP.md`
- Modify: `docs/IMPLEMENTATION_STATUS.md`

**Interfaces:**
- Consumes: verified installer hash and production route.
- Produces: operator recovery guidance and an auditable production deployment record.

- [x] Document menu semantics, validation-only gate, persistent backup path, enrollment boundary, token handling, and the fact that reinstall is not apply or forwarding health.
- [x] Commit and push the tested implementation, deploy only the public installer script behind the exact Nginx route with precondition hashes and rollback backup, then verify public hash/headers, health, metadata, services, and warnings.
- [x] Record production hashes and unresolved real-VPS acceptance boundaries, commit/push the documentation, and leave the immutable `v0.9.10` tag unchanged.
