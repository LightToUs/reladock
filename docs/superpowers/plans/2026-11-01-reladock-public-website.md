# RelaDock Public Website Implementation Plan

> For agentic workers: REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Replace the misleading legacy marketing page with a truthful, polished public website for RelaDock's application-level public connectivity product, then prepare and, when authenticated server access is available, deploy it on `reladock.com` without changing the security boundary of `console.reladock.com`.

**Architecture:** Keep the public website as a dependency-free static artifact in `website/`. Put all business-scope decisions in a small pure JavaScript module so the interactive scope checker is testable without a browser. The production design keeps the marketing host and console origin distinct: the public site may be served for `reladock.com` and `www.reladock.com`; the existing control application remains authoritative only for `console.reladock.com`, with its existing Host/origin and session rules unchanged.

**Tech Stack:** Semantic HTML, modern CSS, ES modules, Node's built-in test runner, design-taste inspection scripts, static HTTP serving for local acceptance.

**Spec:** `docs/REQUIREMENTS.md`, especially “应用级连接业务边界”; `AGENTS.md`; existing production boundary in `docs/IMPLEMENTATION_STATUS.md`.

**Design direction:** `one-motif`. The repeated motif is a single dock/connector shape derived from the product's public-entry-to-fixed-target mapping. Palette is soot, white, and pale lime; no gradients, unverified metrics, fake customer logos, fake pricing, or fabricated form success. Product proof uses four truthful HTML interface fragments marked `data-ui`.

**Global constraints:**

- Use the exact brand spelling `RelaDock`.
- Describe only specified public IP/domain, TCP/UDP, and port connectivity. Do not imply private ERP, SD-WAN, consumer VPN, generic proxy, risk-control evasion, protocol termination, or subscription management.
- Do not claim SLA, latency, node count, customer count, bandwidth guarantees, end-to-end health, automatic failover, or production readiness.
- Keep per-rule bandwidth, quota, term, metering, billing, delivery state, and alert language truthful.
- Primary conversion must be real. Use a prefilled email draft and the existing console link; never show fake submission success.
- Do not expose or weaken the console origin, CSRF, session, CSP, Host, or TLS boundary.
- Do not publish or modify the live server until there is authenticated access and a rollback copy.
- Preserve all unrelated uncommitted repository changes.

**Review focus:** Copy accuracy against product boundaries, mobile hero wrapping, keyboard accessibility, reduced motion, truthful UI states, no fake conversion, console isolation, and production rollback.

---

### Task 1: Build the tested scope-checker model

**Files:**

- Create: `website/js/scope-checker.js`
- Create: `website/test/scope-checker.test.mjs`

**Steps:**

1. Write failing tests for accepted public-domain/public-IP TCP/UDP requests, rejected private/internal targets, rejected unsupported protocols, and deterministic mail-draft generation.
2. Run `node --test website/test/scope-checker.test.mjs` and verify RED because the module is absent.
3. Implement the minimal pure functions and run the test again for GREEN.
4. Refactor names only while the suite stays green.

**Completion:** All scope-checker tests pass, and rejected cases never create language implying that RelaDock provides internal networking or generic Internet egress.

### Task 2: Build the truthful public page

**Files:**

- Create: `website/index.html`
- Create: `website/css/style.css`
- Create: `website/js/main.js`

**Steps:**

1. Create a semantic, accessible page with a two-line-or-less hero, explicit target customers, product boundary, delivery flow, four `data-ui` proof fragments, interactive scope checker, FAQ, real console link, and email CTA.
2. Implement the single connector motif and responsive soot/lime visual system without gradients, rest-state shadows, tiny body text, repeated card grids, or fake imagery.
3. Wire the scope checker, mail draft, mobile navigation, details behavior, focus states, and reduced-motion-safe entrance/preview motion.
4. Add a static-content regression test that verifies prohibited claims are absent, required boundary copy and links are present, and no fake form-success behavior exists.

**Completion:** `node --test website/test/*.test.mjs` passes and the page is usable without JavaScript except for the optional checker.

### Task 3: Local visual and interaction acceptance

**Files:**

- Modify as needed: `website/index.html`, `website/css/style.css`, `website/js/main.js`

**Steps:**

1. Serve `website/` locally and verify the HTML, stylesheet, module, favicon, console link, and mail CTA load without 4xx/5xx responses.
2. Run design-taste lint and browser inspection at desktop, tablet, and mobile widths.
3. Review screenshots, name the three largest issues, fix them, and repeat for at most three rounds.
4. Verify keyboard navigation, contrast, mobile overflow, no horizontal page scroll, and `prefers-reduced-motion` behavior.

**Completion:** No design-taste FAIL remains; any WARN is either fixed or explicitly justified, and screenshots demonstrate a coherent public page at all target widths.

### Task 4: Replace the legacy desktop website artifact safely

**Files:**

- Source: `website/`
- Destination: `/Users/evan/Desktop/code/reladockweb/`

**Steps:**

1. Save a timestamped recoverable copy of the three legacy files.
2. Synchronize the verified static artifact to the existing desktop website directory without deleting unrelated files.
3. Re-run the static test and local HTTP smoke test against the destination directory.

**Completion:** The desktop project contains the verified site, and the exact legacy files remain recoverable.

### Task 5: Deploy with a separate public-host boundary

**Files/hosts:**

- Production server: `103.30.78.5`
- Public hosts: `reladock.com`, `www.reladock.com`
- Existing console host: `console.reladock.com`

**Steps:**

1. Obtain authenticated server access, inventory the live service, certificate layout, listeners, and current rollback path without changing state.
2. Back up every file/configuration that will be changed, including permissions and service metadata.
3. Install the static artifact under a dedicated path and configure host-based HTTPS for root/www with a certificate covering those hosts. Preserve the existing console origin and proxy/request semantics exactly.
4. Validate configuration before reload; reload rather than stop/start where supported.
5. Verify HTTPS certificate names, status, headers, page assets, console login page/API availability, Host rejection behavior, and a rollback rehearsal or command.
6. Update `docs/IMPLEMENTATION_STATUS.md` with exact production evidence and remaining limits only after successful validation.

**Completion:** `https://reladock.com` and `https://www.reladock.com` serve the new site with valid TLS; `https://console.reladock.com` remains operational and isolated. If authenticated access is unavailable, the task remains explicitly blocked and no production claim is made.

### Task 6: Final verification and handoff

**Steps:**

1. Run the website test suite, design lint/inspection, destination smoke test, and `git diff --check`.
2. Review the changed artifact against `AGENTS.md` and the requirements boundary.
3. Report what is locally complete, what is live, the exact verification evidence, rollback location, and any unverified production condition.

**Completion:** Every completion claim is backed by fresh command output and production is clearly distinguished from local readiness.
