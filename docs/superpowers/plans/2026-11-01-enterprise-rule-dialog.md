# Enterprise Rule Dialog Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add safe enterprise rule editing and present rule creation/editing in one polished native dialog.

**Architecture:** Keep node and transport allocation immutable after creation, while allowing tenant administrators to update the rule name, target host/port, and per-rule bandwidth with optimistic version checks. A focused browser module owns dialog state; `app.js` supplies data and persistence callbacks, and the existing platform transaction/outbox/audit path remains authoritative.

**Tech Stack:** Go 1.24, PostgreSQL/pgx, browser ES modules, native HTML `<dialog>`, CSS.

**Spec:** `AGENTS.md` plus the 2026-11-01 user request to optimize the enterprise workspace and use dialogs for rule creation/editing.

## Global Constraints

- Preserve tenant isolation, role authorization, audit atomicity, and node configuration outbox semantics.
- Do not imply that Agent validation, applied ACK, target reachability, or UI state proves end-to-end health.
- Keep protocol, entry node, and allocated listen port immutable during edit.
- Preserve the existing dark RelaDock identity; apply the named design skill at component scope, not as a marketing-page redesign.
- Add no new runtime dependency.

## Review Focus

- A stale edit version must fail without overwriting a newer rule.
- Cross-tenant and viewer edits must reveal no rule data and must not persist.
- Invalid targets or bandwidth above node capacity must fail atomically.
- Closing and reopening the dialog must not leak stale edit state into create mode.
- Mobile layouts must keep dialog controls and row actions reachable at 390 px.

---

### Task 1: Transactional rule update

**Files:**
- Modify: `internal/platform/rules.go`
- Test: `internal/platform/rules_test.go`
- Modify: `internal/web/server.go`
- Test: `internal/web/server_test.go`

**Interfaces:**
- Produces: `RuleUpdateInput` and `Store.UpdateRule(context.Context, Identity, string, RuleUpdateInput) (Rule, error)`.
- Produces: `PATCH /api/rules/{id}` returning the updated rule.

- [x] **Step 1: Write failing platform tests for authorized update, stale version, tenant isolation, viewer denial, validation, capacity, outbox, and audit rollback.**
- [x] **Step 2: Run the focused Go tests and confirm the missing update API fails.**
- [x] **Step 3: Implement the transactional update with row/node locks, validation, version comparison, config enqueue, and `rule.update` audit.**
- [x] **Step 4: Write and run the HTTP contract test for `PATCH /api/rules/{id}`.**

### Task 2: Create/edit dialog behavior

**Files:**
- Create: `public/rule-editor.js`
- Create: `test/rule-editor.test.mjs`
- Modify: `public/rule-submission.js`
- Test: `test/rule-submission.test.mjs`
- Modify: `public/assets.go`
- Modify: `internal/web/static_assets_test.go`

**Interfaces:**
- Produces: `createRuleEditor(dialog, form, elements)` with `openCreate`, `openEdit`, `close`, and session-guarded submit state.
- Produces: `submitRuleUpdate(input, update, refresh)` preserving committed save success when refresh fails.

- [x] **Step 1: Write failing unit tests for create/edit mode, immutable field presentation, cancel/reset, and update refresh failure.**
- [x] **Step 2: Implement the focused dialog controller and submission helper.**
- [x] **Step 3: Embed and serve the new ES module, with a static presentation contract test.**

### Task 3: Enterprise workspace integration and visual polish

**Files:**
- Modify: `public/index.html`
- Modify: `public/app.js`
- Modify: `public/app.css`
- Modify: `docs/IMPLEMENTATION_STATUS.md`

**Interfaces:**
- Consumes: `PATCH /api/rules/{id}` and `createRuleEditor(...)`.
- Produces: one “创建规则” entry point and one “编辑” action per writable rule, both using the same dialog.

- [x] **Step 1: Replace the inline form with a rule-management header and native dialog; wire create/edit persistence and accessible close behavior.**
- [x] **Step 2: Refine hierarchy, spacing, control states, table/action layout, and mobile dialog behavior within the existing palette.**
- [ ] **Step 3: Run the taste-skill linter and inspector at 1440, 768, and 390 px; critique screenshots and fix the highest-impact issues.** Linter passed with 0 FAIL / 0 WARN; the saved browser policy blocked localhost inspection after explicit approval, so screenshot review remains unavailable.
- [x] **Step 4: Update implementation status with the exact supported edit fields and immutable allocation fields.**

### Task 4: Verification

**Files:**
- Verify all modified files.

**Interfaces:**
- Consumes: all prior task outputs.
- Produces: test, vet, diff, and visual evidence for handoff.

- [x] **Step 1: Run focused Node and Go tests.**
- [x] **Step 2: Run `npm test`, `npm run vet`, and `git diff --check`.**
- [x] **Step 3: Re-run final visual inspection and record any environment-only limitations.** The browser permission limitation is recorded in `docs/IMPLEMENTATION_STATUS.md`.
