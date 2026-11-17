# Agent Apply Onboarding Implementation Plan

> **For Codex:** REQUIRED SUB-SKILL: Use executing-plans to implement this plan task-by-task.

**Goal:** Add an explicit, fail-closed fresh-install path that prepares and enables apply mode, persists its reviewed prerequisites over reboot, and automatically redelivers a configuration validated before the mode transition.

**Architecture:** Keep privileged host preparation in a packaged root oneshot systemd unit gated by an installer-created authorization marker. Keep the Agent's existing apply preflight as the final safety gate. Add a transactional, idempotent control-plane redelivery when an authenticated heartbeat transitions to apply with a current validated-but-unapplied generation.

**Tech Stack:** POSIX shell, systemd, Go 1.24, PostgreSQL/pgx, Node.js test runner.

---

### Task 1: Specify transition redelivery with failing tests

**Files:**
- Modify: `internal/platform/heartbeat_test.go`
- Modify: `internal/platform/heartbeat.go`

Add database tests for one-time redelivery on a non-apply-to-apply transition and negative cases. Run the focused test and observe the expected failure. Implement the transaction-local state check and call the existing snapshot enqueue helper. Re-run the focused tests.

### Task 2: Specify release and systemd wiring with failing tests

**Files:**
- Modify: `test/install-linux.test.mjs`
- Modify: `test/release-package.test.mjs`
- Modify: `test/bootstrap-linux.test.mjs`
- Create: `deploy/reladock-agent-network.service`
- Modify: `deploy/reladock-agent-apply.service`
- Modify: `deploy/install-linux.sh`
- Modify: `scripts/release-package.mjs`
- Modify: `scripts/release.mjs`

Assert the new unit is packaged, installed, ordered before apply, root-only, tightly sandboxed and gated by `/etc/reladock/network-managed`. Observe test failure, add the unit and all release/install allowlists, then re-run focused tests.

### Task 3: Add confirmed bootstrap apply onboarding

**Files:**
- Modify: `test/bootstrap-linux.test.mjs`
- Modify: `deploy/bootstrap-agent.sh`
- Modify: `deploy/agent.env.example`

Add failing installer tests for the validation default and confirmed apply path. Implement bounded discovery, exact display/confirmation, lease/environment writes, marker creation, persistent network unit activation, service switch, and validation rollback on pre-application failure. Preserve existing installation upgrade semantics and private state.

### Task 4: Update operator contracts and implementation status

**Files:**
- Modify: `docs/DEPLOYMENT.md`
- Modify: `README.md`
- Modify: `docs/IMPLEMENTATION_STATUS.md`

Document the menu, supported automatic-preparation envelope, fallback behavior, redelivery semantics, reboot persistence and the boundary between applied ACK and forwarding health. Record local verification and any unverified production acceptance.

### Task 5: Full verification

Run focused Go and Node tests, `npm test`, and `npm run vet`. Inspect the final diff for unrelated dirty-worktree changes and do not overwrite them. If Docker installer tests are unavailable, report that boundary explicitly rather than claiming them passed.
