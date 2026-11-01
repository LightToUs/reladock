# Console Page Navigation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give both RelaDock consoles independent URL-backed pages and collapsible responsive sidebars.

**Architecture:** Add one framework-free navigation controller shared by both static consoles. Existing DOM sections become named panes; app-specific code supplies titles and lazy-load callbacks while the controller owns hash routing, history synchronization, mobile drawer state and persisted desktop collapse.

**Tech Stack:** HTML, CSS, browser ES modules, Node test runner.

**Spec:** `docs/superpowers/specs/2026-11-01-console-page-navigation-design.md`

## Global Constraints

- Preserve all API, authentication, dialog, telemetry and Agent behavior.
- Use hash routes so the existing Go static routes need no server changes.
- Unknown routes resolve to overview without displaying multiple panes.
- Keep unrelated untracked website and deployment files untouched.

## Review Focus

- Direct opening and browser history must keep route, title and pane aligned.
- Dynamic admin menu items must be routable even though they are appended at runtime.
- A collapsed sidebar must remain usable by keyboard and screen-reader users.
- Mobile drawer close behavior must not steal focus on desktop.
- Authentication transitions must not expose hidden console panes.

---

### Task 1: Shared navigation controller

**Files:**
- Create: `public/console-navigation.js`
- Create: `test/console-navigation.test.mjs`
- Modify: `public/assets.go`

**Interfaces:**
- Produces: `bindConsoleNavigation(options)` with `select(view)`, `sync()`, `closeMobile()`, and `destroy()`.
- Consumes: named menu items and panes supplied as DOM collections.

- [ ] Write tests for initial hash, unknown route fallback, click navigation, history changes, mobile close and persisted collapse.
- [ ] Run the focused test and verify RED because the module does not exist.
- [ ] Implement the controller and embed it as a static asset.
- [ ] Run the focused test and verify GREEN.
- [ ] Commit the task.

### Task 2: Enterprise page layout

**Files:**
- Modify: `public/index.html`
- Modify: `public/app.js`
- Modify: `public/app.css`
- Modify: `internal/web/static_assets_test.go`

**Interfaces:**
- Consumes: `bindConsoleNavigation(options)` from Task 1.
- Produces: eight enterprise panes with URL-backed navigation and responsive sidebar controls.

- [ ] Add failing static behavior tests for pane grouping, route links and collapse controls.
- [ ] Run focused tests and verify RED.
- [ ] Group existing content into named panes, bind navigation, and add desktop/mobile sidebar CSS.
- [ ] Run focused and navigation tests and verify GREEN.
- [ ] Commit the task.

### Task 3: Platform page routing and sidebar collapse

**Files:**
- Modify: `public/admin.html`
- Modify: `public/admin.js`
- Modify: `public/admin.css`
- Modify: `test/admin-navigation.test.mjs`
- Modify: `internal/web/static_assets_test.go`

**Interfaces:**
- Consumes: `bindConsoleNavigation(options)` from Task 1.
- Produces: URL-backed existing platform panes plus responsive collapse controls.

- [ ] Add failing tests for admin URL routing, dynamic menu items and desktop collapse controls.
- [ ] Run focused tests and verify RED.
- [ ] Replace the page-only selection binding with the shared controller while preserving lazy loaders.
- [ ] Run focused tests and verify GREEN.
- [ ] Commit the task.

### Task 4: Verification and local handoff

**Files:**
- Modify: `docs/IMPLEMENTATION_STATUS.md`

- [ ] Run `npm test` and `npm run vet`.
- [ ] Perform desktop, collapsed sidebar and mobile drawer browser checks for both consoles.
- [ ] Update implementation status with results and remaining limits.
- [ ] Start the local stack and provide the test URL without deploying production.
