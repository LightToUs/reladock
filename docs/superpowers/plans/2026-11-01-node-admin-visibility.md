# Node Admin Visibility Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let platform administrators hide mistaken or operationally noisy nodes from the default node list without deleting customer rules, changing node lifecycle state, or requiring an Agent.

**Architecture:** Store platform-wide node-list visibility in a dedicated PostgreSQL table so visibility changes do not mutate `nodes.revision` or invalidate retirement attestations. Return visibility facts with the existing admin node API, expose audited hide/restore actions with an independent optimistic revision, and filter only the node-management table in the browser while retaining hidden nodes in operational selectors, rules, alerts, and monitoring APIs.

**Tech Stack:** Go 1.24+, PostgreSQL migrations and transactions, static ES modules, Node test runner.

**Spec:** User-approved behavior in this chat, constrained by `docs/ARCHITECTURE.md` node lifecycle and platform audit requirements.

## Global Constraints

- Hiding is presentation metadata only; it must not change rules, allocations, Agent identity, delivery, billing, monitoring, or retirement state.
- Hidden nodes remain available to global rules, alerts, costs, delivery selectors, and API responses.
- Default node-table view excludes hidden nodes; an explicit control reveals them and allows restoring visibility.
- Hide and restore require platform-admin authorization, use an independent visibility revision, and write platform audit in the same transaction.
- Existing uncommitted navigation and node-table changes must be preserved.

## Review Focus

- A visibility write must not increment `nodes.revision` or invalidate a valid drain attestation.
- A stale visibility revision must return conflict without changing state or writing audit.
- A non-platform tenant must not hide or restore a node.
- Hidden nodes must remain present in the admin API and operational selectors even when the default table filters them.
- With the “show hidden” control enabled, hidden rows must expose restore rather than hide.

---

### Task 1: Persisted, Audited Visibility

**Files:**
- Create: `internal/platform/migration25.sql`
- Modify: `internal/platform/store.go`
- Modify: `internal/platform/admin_nodes.go`
- Test: `internal/platform/admin_test.go`
- Test: `internal/platform/heartbeat_test.go`

**Interfaces:**
- Produces: `ManagedNode.Hidden bool`, `ManagedNode.VisibilityRevision int64`, and `Store.AdminSetNodeVisibility(ctx, who, nodeID, visibilityRevision, hidden) error`.

- [ ] Write integration tests for hide/restore, stale revision, audit rollback, and unchanged operational node revision.
- [ ] Run the focused Go test and verify it fails because migration 25 and visibility behavior are absent.
- [ ] Add migration 25, register schema version 25, join visibility facts into `AdminNodes`, and implement the transactional write.
- [ ] Run focused tests and verify they pass.

### Task 2: Authorized HTTP Contract

**Files:**
- Modify: `internal/web/admin.go`
- Test: `internal/web/server_test.go`

**Interfaces:**
- Consumes: `Store.AdminSetNodeVisibility`.
- Produces: `POST /api/admin/nodes/:id/visibility` with strict `{visibilityRevision, hidden}` JSON.

- [ ] Write HTTP tests for platform success, strict JSON rejection, stale conflict, and tenant denial.
- [ ] Run the focused HTTP test and verify it fails because the route is absent.
- [ ] Implement the strict platform-admin route.
- [ ] Run focused tests and verify they pass.

### Task 3: Default-Hidden Admin List and Recovery UI

**Files:**
- Modify: `public/admin.html`
- Modify: `public/admin.js`
- Modify: `public/node-action-dialog.js`
- Test: `public/node-action-dialog.test.js`
- Test: `internal/web/static_assets_test.go`

**Interfaces:**
- Consumes: `hidden` and `visibilityRevision` from the admin node API.
- Produces: default filtering, “显示已隐藏节点”, “隐藏节点”, and “恢复显示” controls.

- [ ] Write JS and static-asset tests proving visibility actions use the independent revision and the UI exposes a recovery path.
- [ ] Run focused tests and verify they fail for missing behavior.
- [ ] Implement filtering and dialog actions without removing hidden nodes from shared client state.
- [ ] Run focused tests and verify they pass.

### Task 4: Documentation and Full Verification

**Files:**
- Modify: `README.md`
- Modify: `docs/ARCHITECTURE.md`
- Modify: `docs/ROADMAP.md`
- Modify: `docs/IMPLEMENTATION_STATUS.md`

- [ ] Document that visibility is not deletion, retirement, health, or traffic control.
- [ ] Run `npm test` with the real PostgreSQL test database and record pass/fail/skip counts.
- [ ] Run `npm run vet` and record the result.
- [ ] Review the final diff to confirm unrelated working-tree changes remain intact.
