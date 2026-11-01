# Node Row Dialog Actions Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make every enabled action in an entrance-node table row open a native dialog for its content or confirmation.

**Architecture:** Add one reusable node-action dialog controller for state-changing confirmations, while keeping the existing dedicated editor and Agent-install dialogs. Convert certificate management and per-node delivery history from inline/navigation panels to dedicated native dialogs. Existing APIs, revision guards, lifecycle rules, and health wording remain unchanged.

**Tech Stack:** Static HTML/CSS, browser ES modules, Node.js built-in test runner, Go embedded assets.

**Spec:** User request in the current conversation: “入口节点列表的 item 每个按钮，点击都是通过弹出对话框的方式显示内容”.

## Global Constraints

- Do not add a frontend framework or dependency.
- Do not weaken revision checks, lifecycle gates, audit behavior, or credential clearing.
- Do not use browser-native `confirm` for entrance-node row actions.
- Disabled informational controls remain disabled and do not open dialogs.
- No action may claim end-to-end health.

## Review Focus

- A failed action remains visible in its dialog and can be retried safely.
- Closing a dialog never performs its pending action.
- Revision conflicts reload current data without silently submitting again.
- Delivery history loads only the selected node and exposes no payload or lease token.
- Agent installation close/Escape still clears the one-time command.

---

### Task 1: Reusable confirmation dialog

**Files:**
- Create: `public/node-action-dialog.js`
- Create: `public/node-action-dialog.test.js`
- Modify: `public/admin.html`
- Modify: `public/admin.css`
- Modify: `public/assets.go`
- Modify: `internal/web/server.go`
- Modify: `public/assets_test.go`
- Modify: `internal/web/static_assets_test.go`

**Interfaces:**
- Produces: `bindNodeActionDialog({ dialog, title, description, status, confirmButton, cancelButton })` with `open(action)` and `close()`.

- [x] Write failing tests for opening, cancellation, one-shot confirmation, error retention, and successful close.
- [x] Run `node --test public/node-action-dialog.test.js` and observe the expected failure.
- [x] Implement the controller, dialog markup/style, and static asset serving.
- [x] Run the focused Node and Go static-asset tests.

### Task 2: Convert node row state changes

**Files:**
- Modify: `public/admin.js`
- Test: `public/node-action-dialog.test.js`

**Interfaces:**
- Consumes: `nodeActionDialog.open({ title, description, confirmLabel, danger, run })`.

- [x] Route maintenance, enable, retry, upgrade, upgrade cancellation, draining, final retirement, and registration withdrawal through the reusable dialog.
- [x] Remove browser-native `confirm` usage from node-row control construction.
- [x] Keep disabled lifecycle controls unchanged.
- [x] Run focused tests and syntax checks.

### Task 3: Convert certificate and delivery content

**Files:**
- Modify: `public/admin.html`
- Modify: `public/admin.js`
- Modify: `public/admin.css`
- Test: `public/node-dialog-content.test.js`

**Interfaces:**
- Produces: certificate and per-node delivery dialogs opened directly from their row controls.

- [x] Write failing tests for dialog open/close state and stale async delivery-load protection.
- [x] Convert the certificate panel to a dialog without changing certificate APIs.
- [x] Add a delivery-history dialog that fetches the selected node’s recent records.
- [x] Keep the existing global delivery page and navigation available.
- [x] Run focused tests and syntax checks.

### Task 4: Documentation and full verification

**Files:**
- Modify: `README.md`
- Modify: `docs/ROADMAP.md`

- [x] Document the unified row-dialog interaction.
- [x] Run `npm test` against the real isolated PostgreSQL test database.
- [x] Run `npm run vet`.
- [x] Run `git diff --check` and review the final diff without modifying unrelated user work.
