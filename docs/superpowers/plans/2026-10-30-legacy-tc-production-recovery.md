# Legacy tc Production Recovery Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Support iproute2 5.15 HTB class readback, make fail-closed freezing idempotent, and safely recover the production entry node before enabling a paid test rule.

**Architecture:** Keep JSON readback primary and add a strict legacy-text parser only at the shaping backend boundary. Detect an existing valid meter freeze from owned nft JSON before inserting another. Reuse the current durable Agent recovery path; never clear production journals or applying rows by hand.

**Tech Stack:** Go 1.24+, nftables, iproute2/tc, PostgreSQL, systemd, existing release scripts.

**Spec:** `docs/superpowers/specs/2026-10-30-legacy-tc-production-recovery-design.md`

## Global Constraints

- Preserve fail-closed behavior, platform-only ownership, durable generation recovery, WAL facts, and per-rule quota/bandwidth semantics.
- Unknown or malformed kernel output must be rejected; no permissive parser fallback.
- Do not clear production firewall tables, Agent state, WAL, or PostgreSQL applying rows manually.
- Active forwarding requires an effective rule subscription; applied ACK remains distinct from path health.
- Update release version, README, architecture/roadmap status, and report unverified protocol health.

## Review Focus

- Legacy text that starts like an HTB class but omits/duplicates rate or ceil must fail closed.
- JSON-looking malformed output must not silently enter the text parser.
- Rate units and multiplication overflow must be rejected rather than wrapped.
- Existing freeze comments on non-drop rules must not suppress a required freeze.
- Repeated recovery/Observe calls must not grow freeze rules or sample before traffic is frozen.

---

### Task 1: Strict legacy HTB class readback

**Files:**
- Modify: `internal/shaping/backend.go`
- Modify: `internal/shaping/backend_test.go`
- Modify: `internal/shaping/integration_test.go`

**Interfaces:**
- Consumes: raw bytes returned by `tc -j class show dev <device>`.
- Produces: `parseClassOutput([]byte) ([]map[string]json.RawMessage, error)` or an equivalent focused parser used by `SystemBackend.Inspect` without changing `Snapshot`.

- [ ] **Step 1: Write failing backend tests** for the exact production iproute2 5.15 line, root/child classes, JSON preservation, empty output, malformed JSON-looking input, unknown kind, missing/duplicate fields, unsupported units and overflow.
- [ ] **Step 2: Run `go test ./internal/shaping -run 'Test.*Legacy|TestSystemBackend' -count=1`** and confirm the production fixture fails because JSON decoding sees `c`.
- [ ] **Step 3: Implement the strict parser and connect it only to class readback.**
- [ ] **Step 4: Run focused shaping tests and the existing Docker HTB readback test where available.**
- [ ] **Step 5: Commit `fix: support legacy tc class readback`.**

### Task 2: Idempotent fail-closed meter freeze

**Files:**
- Modify: `internal/dataplane/adapter.go`
- Modify: `internal/dataplane/hooks_test.go`
- Modify: `internal/dataplane/adapter_test.go` if shared nft fixtures belong there.

**Interfaces:**
- Consumes: verified owned-table JSON from the existing runner.
- Produces: one internal `ensureFrozen(context.Context) error` path shared by `freezeOwned` and `freezeAndSample`.

- [ ] **Step 1: Write failing tests** proving repeated freeze calls insert once, an existing duplicate valid freeze inserts none, malformed/comment-only non-drop rules fail, and `BeforeReplace` still occurs after a confirmed freeze.
- [ ] **Step 2: Run `go test ./internal/dataplane -run 'Test.*Freeze|TestTransition' -count=1`** and confirm duplicate insertion fails the assertion.
- [ ] **Step 3: Implement strict relay freeze detection and replace direct insert calls with the shared helper.**
- [ ] **Step 4: Run focused dataplane tests and opted-in isolated Docker dataplane tests.**
- [ ] **Step 5: Commit `fix: make dataplane freeze idempotent`.**

### Task 3: Release, review, and production recovery

**Files:**
- Modify: version constants and release metadata files located by repository search.
- Modify: `README.md`
- Modify: `docs/ARCHITECTURE.md`
- Modify: `docs/ROADMAP.md`

**Interfaces:**
- Consumes: Tasks 1–2 behavior and existing durable production generation 5 journal.
- Produces: fixed GitHub release, upgraded control/entry binaries, recovered applied generation, and a separately entitled test rule.

- [ ] **Step 1: Update to the next patch version and document legacy tc compatibility plus production recovery boundaries.**
- [ ] **Step 2: Run `RELADOCK_INSTALLER_DOCKER_TESTS=1 npm test`, `npm run vet`, release builds/checksums, and `git diff --check`.**
- [ ] **Step 3: Request whole-branch code review and resolve Critical/Important findings with RED→GREEN tests.**
- [ ] **Step 4: Publish the fixed GitHub Release, verify anonymous hashes, back up and upgrade control then entry.**
- [ ] **Step 5: Observe automatic exact recovery without deleting journals; verify generation 5 terminates and the latest paused snapshot applies.**
- [ ] **Step 6: Create an explicit test plan version and audited test wallet adjustment/purchase through existing application interfaces; do not insert subscription rows manually.**
- [ ] **Step 7: Verify the resulting active generation, nft DNAT/SNAT/counters, tc classes/filters, one clean physical plan, Agent/service health, and target route.**
- [ ] **Step 8: Commit documentation/release changes, push the feature branch, and use the agreed branch integration workflow.**
