# Withdraw Empty Node Registration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Allow a platform administrator to remove an accidentally registered node only while it has never entered deployment or traffic lifecycle state.

**Architecture:** PostgreSQL remains the authority for withdrawal eligibility. The node list exposes a server-derived `withdrawable` fact, and a revision-guarded transaction rechecks that fact, removes only the untouched initial configuration job and node row, then records a durable platform audit entry; deployed nodes continue to use safe retirement.

**Tech Stack:** Go 1.24+, PostgreSQL integration tests, existing static JavaScript admin UI.

**Spec:** `AGENTS.md`, `docs/ARCHITECTURE.md`, and the approved 2026-11-01 chat scope: only never-deployed empty nodes may be withdrawn.

## Global Constraints

- Do not add a force-delete path for deployed, enrolled, used, draining, or retired nodes.
- Preserve platform audit history and write the withdrawal audit entry in the same transaction as deletion.
- Keep optimistic revision checks and database authorization on every management write.
- Do not delete rule, allocation, billing, Agent, upgrade, retirement, cost, incident, or observation history.
- The automatically created generation-1 queued configuration is the only dependent lifecycle row that withdrawal may remove.

## Review Focus

- A stale node revision must return a conflict and preserve the node.
- A claimed, attempted, reported, retried, validated, or applied configuration must block withdrawal.
- Any certificate/Agent observation, rule history, port allocation, upgrade, retirement, cost, incident, probe, or resource sample must block withdrawal.
- An unredeemed bootstrap token may be revoked and removed atomically; a redeemed token must block withdrawal.
- Audit insertion failure must roll back both the initial configuration deletion and node deletion.

---

### Task 1: Transactional eligibility and withdrawal

**Files:**
- Modify: `internal/platform/admin_nodes.go`
- Test: `internal/platform/admin_test.go`

**Interfaces:**
- Produces: `ManagedNode.Withdrawable bool` and `Store.AdminWithdrawNodeRegistration(ctx context.Context, who Identity, id string, revision int64) error`.

- [ ] **Step 1: Write failing PostgreSQL integration tests**

Cover successful withdrawal and ID reuse, stale revisions, blocking lifecycle facts, unredeemed versus redeemed bootstrap rows, and rollback when platform audit insertion fails.

- [ ] **Step 2: Run the focused platform tests and verify they fail because the API is absent**

Run: `npm run test:go -- ./internal/platform -run 'TestAdminWithdraw'`

- [ ] **Step 3: Implement the shared eligibility query and transactional withdrawal**

The action must lock authorization and the node, recheck revision and eligibility, delete only unredeemed bootstrap rows plus the untouched generation-1 queued configuration, delete the node, append `node.withdraw-registration` to `platform_audit_logs`, and commit atomically.

- [ ] **Step 4: Run focused platform tests until green**

Run: `npm run test:go -- ./internal/platform -run 'TestAdminWithdraw'`

### Task 2: HTTP contract and admin control

**Files:**
- Modify: `internal/web/admin.go`
- Modify: `public/admin.js`
- Modify: `public/admin.html`
- Create: `public/node-withdrawal.js`
- Test: `internal/web/server_test.go`
- Test: `public/node-withdrawal.test.js`

**Interfaces:**
- Consumes: `ManagedNode.withdrawable` and `AdminWithdrawNodeRegistration` from Task 1.
- Produces: `POST /api/admin/nodes/{id}/withdraw-registration` with `{revision}`, plus a danger-styled `撤销登记` control only for withdrawable nodes.

- [ ] **Step 1: Write failing HTTP and JavaScript behavior tests**

Assert strict revision input, platform authorization, successful removal, and a pure withdrawal-control descriptor that is absent for ineligible nodes and contains the expected route/body for eligible nodes.

- [ ] **Step 2: Run focused tests and verify expected failures**

Run: `npm run test:go -- ./internal/web -run 'TestAdminWithdraw'`

Run: `node --test public/node-withdrawal.test.js`

- [ ] **Step 3: Implement the route and render the guarded control**

Use the descriptor module from `admin.js`; retain a confirmation explaining that the action is only for an undeployed empty registration and is irreversible.

- [ ] **Step 4: Run focused tests until green**

Run the two focused commands from Step 2.

### Task 3: Documentation and full verification

**Files:**
- Modify: `README.md`
- Modify: `docs/ARCHITECTURE.md`
- Modify: `docs/ROADMAP.md`

**Interfaces:**
- Consumes: completed behavior from Tasks 1 and 2.
- Produces: accurate operator documentation distinguishing withdrawal from safe retirement.

- [ ] **Step 1: Document the narrow withdrawal exception**

State that only an undeployed, empty registration can be removed and that any lifecycle history continues through irreversible safe retirement with retained evidence.

- [ ] **Step 2: Run formatting, full tests, and static checks**

Run: `gofmt -w internal/platform/admin_nodes.go internal/platform/admin_test.go internal/web/admin.go internal/web/server_test.go`

Run: `npm test`

Run: `npm run vet`

- [ ] **Step 3: Review the final diff for scope and lifecycle safety**

Confirm no deployed-node deletion route, history cascade, force release, network mutation, or production action was introduced.
