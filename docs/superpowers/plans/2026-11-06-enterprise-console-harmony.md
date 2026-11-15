# Enterprise Console Harmony Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Reorganize the enterprise console into five flat business pages plus an accessible account menu, and give the whole workspace one coherent, production-quality visual system.

**Architecture:** Keep the existing static HTML/CSS/ES modules and hash router. Add one focused account-menu controller, remap existing panes without changing API calls, and scope all visual changes to `#console main` or enterprise navigation selectors so login and `/admin` remain untouched.

**Tech Stack:** Go embedded static assets, vanilla JavaScript ES modules, HTML, CSS, Node test runner, Go static asset contract tests.

**Spec:** `docs/superpowers/specs/2026-11-06-enterprise-console-harmony-design.md`

## Global Constraints

- Preserve all existing user changes in the dirty worktree; do not commit or reset unrelated work.
- Keep five flat sidebar entries: overview, rules, node status, billing, finance.
- Put enterprise collaboration, audit, support, platform admin and logout in one account popover with direct links only.
- Preserve all control-plane truth boundaries and existing API/business behavior.
- Add no dependency and do not change `/admin` or the standalone authentication layout.

## Review Focus

- Direct opening and browser history for account-only routes must still show exactly one pane.
- Account popover must close on outside click, Escape and selection, restoring focus on Escape.
- Moving telemetry and delivery to rules must not duplicate IDs or stop existing refresh/render code.
- Splitting finance from package browsing must not break wallet, ledger, checkout or purchase selectors.
- Mobile widths and menus must not overflow or hide the five primary routes.

---

### Task 1: Account Menu Controller

**Files:**
- Create: `public/account-menu.js`
- Create: `test/account-menu.test.mjs`
- Modify: `public/assets.go`
- Modify: `public/assets_test.go`

**Interfaces:**
- Produces: `bindAccountMenu({ root, trigger, items, documentTarget, onSelect })` returning `{ close(), destroy() }`.

- [ ] Write tests for trigger toggling, direct view selection, outside click, Escape focus restoration and destroy cleanup.
- [ ] Run `node --test test/account-menu.test.mjs` and confirm RED because the module is missing.
- [ ] Implement the controller without DOM globals beyond injected defaults.
- [ ] Embed the module and extend embedded-asset tests.
- [ ] Run the focused Node and Go asset tests and confirm GREEN.

### Task 2: Flat Information Architecture

**Files:**
- Modify: `public/index.html`
- Modify: `public/app.js`
- Modify: `internal/web/static_assets_test.go`
- Modify: `test/console-navigation.test.mjs`

**Interfaces:**
- Consumes: `bindAccountMenu` from Task 1 and existing `bindConsoleNavigation`.
- Produces: five sidebar routes plus direct account routes `team`, `audit`, `operations`, and flow-only `checkout`.

- [ ] Add a failing static contract test asserting the five exact sidebar views and absence of traffic, delivery, team and operations sidebar links.
- [ ] Add failing assertions for account trigger/menu items and direct route panes.
- [ ] Rebuild the header account area and relocate existing panes: telemetry and delivery to rules, audit to audit, subscriptions to billing, wallet/ledger to finance.
- [ ] Update titles/descriptions and bind account-menu selections to `workspaceNavigation.select`.
- [ ] Preserve platform-admin visibility, role display, logout guard and refresh behavior.
- [ ] Run focused navigation, business-tool and static asset tests and confirm GREEN.

### Task 3: Harmonized Enterprise Visual System

**Files:**
- Modify: `public/app.css`
- Modify: `internal/web/static_assets_test.go`

**Interfaces:**
- Consumes: the DOM classes and IDs from Task 2.
- Produces: scoped enterprise styles for sidebar, account popover, page surfaces, typography, cards, tables, forms, dialogs and responsive layouts.

- [ ] Extend the enterprise visual contract test to require account-menu styling, consistent radii/spacing tokens, flat light surfaces and the mobile full-width rule.
- [ ] Run the focused Go test and confirm RED on missing visual contracts.
- [ ] Implement the scoped CSS, including AA secondary text, keyboard focus states and narrow-screen account-menu placement.
- [ ] Run the focused test and confirm GREEN.

### Task 4: Documentation and Full Verification

**Files:**
- Modify: `docs/IMPLEMENTATION_STATUS.md`

**Interfaces:**
- Consumes: completed Tasks 1–3.
- Produces: current implementation/verification record with local-only deployment boundary.

- [ ] Update implementation status with the five-entry sidebar, account menu, page merges and unchanged production boundary.
- [ ] Run `npm test` and require zero failures, recording any environment skip.
- [ ] Run `npm run vet` and `git diff --check`.
- [ ] Restart `com.reladock.local` and verify `/` plus `/app.css` return HTTP 200.
- [ ] Request independent code review focused on route reachability, accessibility, responsive behavior and accidental business-logic changes; fix all Critical/Important findings and reverify.
