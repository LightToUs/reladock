# Lightsail Script Nodes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a disabled-by-default platform-admin module that creates one Amazon Lightsail Linux instance with one static IP, executes one pasted Bash script once over verified SSH, stops it, and safely destroys it.

**Architecture:** Keep AWS calls and SSH execution in independent `internal/cloud` packages, persist authorization and lifecycle facts in PostgreSQL, and drive external side effects through a leased restart-safe worker. Expose only strict platform-admin HTTP endpoints and a separate admin route; never mix generic Lightsail machines with RelaDock relay nodes.

**Tech Stack:** Go 1.24, PostgreSQL 17 integration tests, AWS SDK for Go v2 Lightsail/config packages, `golang.org/x/crypto/ssh`, static ES modules and Node tests.

**Spec:** `docs/superpowers/specs/2026-11-15-lightsail-script-nodes-design.md`

## Global Constraints

- Preserve every pre-existing tracked and untracked workspace change; do not reset, stash, clean, or overwrite unrelated work.
- Follow `AGENTS.md`: do not purchase servers, call live AWS, open real SSH sessions, deploy production, or use customer credentials without a later explicit authorization.
- The connector defaults disabled and partial enabled configuration fails startup.
- Only platform administrators may read or mutate Lightsail records; tenant API keys are forbidden.
- One static IPv4 maps to one immutable script identity; the script may cross the remote execution boundary at most once.
- AWS `running`, SSH exit zero, and local job success never mean RelaDock forwarding or end-to-end health.
- Use TDD for every behavior: observe a focused failing test before writing its production implementation.
- PostgreSQL remains the durable source of state; external calls happen only after the requesting local transaction and platform audit commit.
- Automated verification uses provider/SSH substitutes only and must not require network credentials.

## Review Focus

- An uncertain `CreateInstances` response must reconcile by deterministic name/tag and never purchase a duplicate; Task 4 pins this with a crash/retry worker test.
- A process death at the one-time SSH boundary must become terminal `unknown` and never execute again; Task 4 pins both sides of the boundary.
- Destroy must reject an instance/static-IP ownership mismatch and provide no force path; Tasks 3 and 4 pin this with PostgreSQL and worker tests.
- SSH must reject every key not present in AWS-witnessed host keys; Task 2 pins match/mismatch and certificate authentication.
- Script/output secrets, oversized content, ANSI controls, and unbounded logs must not escape through API/audit/logs; Tasks 2, 3, and 5 pin these boundaries.

---

### Task 1: Runtime Configuration and Provider Contracts

**Files:**
- Create: `internal/cloud/lightsail/config.go`
- Create: `internal/cloud/lightsail/config_test.go`
- Create: `internal/cloud/lightsail/types.go`
- Modify: `internal/runtimeconfig/config.go`
- Modify: `deploy/control.env.example`

**Interfaces:**
- Produces: `lightsail.ConfigFromEnv(get func(string) string) (Config, error)`.
- Produces: `lightsail.Provider` with catalog, instance, operation, static-IP, firewall, temporary-access, stop, and delete methods used by Task 4.
- Produces: provider-neutral `Catalog`, `Instance`, `Operation`, `StaticIP`, `AccessDetails`, and create input types; no AWS SDK types cross the package boundary.
- Produces: `runtimeconfig.Config.Lightsail lightsail.Config`.

- [ ] **Step 1: Write failing configuration tests**

  Add table tests for disabled defaults, exact `true|false`, required region allowlist, canonical de-duplicated region codes, literal `/32` or `/128` controller CIDR, bounded duration overrides, and rejection of partial/unknown values. Assert errors contain no environment values.

- [ ] **Step 2: Run the focused tests and verify RED**

  Run: `go test ./internal/cloud/lightsail ./internal/runtimeconfig`

  Expected: FAIL because the package, config, and `runtimeconfig.Config.Lightsail` do not exist.

- [ ] **Step 3: Implement the minimal config and provider-neutral contracts**

  Implement disabled defaults and enabled validation. Use defaults of 5 seconds worker cadence, 15 minutes AWS-operation timeout, 30 seconds SSH-connect timeout, and 20 minutes script timeout; accept only bounded overrides described in tests. Do not resolve AWS credentials during config parsing.

- [ ] **Step 4: Document the environment contract**

  Add only non-secret examples and state that credentials come from the AWS SDK default chain. Do not add access-key placeholders that invite committing secrets.

- [ ] **Step 5: Run focused tests and verify GREEN**

  Run: `go test ./internal/cloud/lightsail ./internal/runtimeconfig`

  Expected: PASS.

- [ ] **Step 6: Commit**

  ```bash
  git add internal/cloud/lightsail/config.go internal/cloud/lightsail/config_test.go internal/cloud/lightsail/types.go internal/runtimeconfig/config.go deploy/control.env.example
  git commit -m "feat: define Lightsail connector configuration"
  ```

### Task 2: AWS Adapter and Verified SSH Runner

**Files:**
- Create: `internal/cloud/lightsail/aws.go`
- Create: `internal/cloud/lightsail/aws_test.go`
- Create: `internal/cloud/sshdeploy/runner.go`
- Create: `internal/cloud/sshdeploy/runner_test.go`
- Modify: `go.mod`
- Modify: `go.sum`

**Interfaces:**
- Consumes: `lightsail.Provider` and provider-neutral types from Task 1.
- Produces: `lightsail.NewAWSProvider(ctx context.Context, cfg Config) (Provider, error)` using an internal per-region SDK-client factory.
- Produces: `sshdeploy.Runner` with `Run(context.Context, Request) (Result, error)`.
- Produces: `sshdeploy.Request{Address, Username, PrivateKeyPEM, Certificate, HostPublicKeys, Script, ConnectTimeout, RunTimeout}` and `Result{Started, ExitCode, Output, HostKeySHA256}`.

- [ ] **Step 1: Write failing AWS mapping tests**

  Use a narrow fake SDK client to assert allowed-region enforcement, active Linux catalog filtering, deterministic create tags, exact operation/status mapping, exact TCP/22 source CIDR replacement, static-IP operations, temporary access material mapping, sanitized provider errors, and context deadlines.

- [ ] **Step 2: Run adapter tests and verify RED**

  Run: `go test ./internal/cloud/lightsail`

  Expected: FAIL because `NewAWSProvider` and the SDK adapter are absent.

- [ ] **Step 3: Add official AWS SDK dependencies and implement the adapter**

  Add only AWS SDK v2 `config` and `service/lightsail` dependencies. Cache clients by allowed region, keep explicit HTTP timeouts, rely on the SDK credential chain, and never return raw SDK error bodies.

- [ ] **Step 4: Run adapter tests and verify GREEN**

  Run: `go test ./internal/cloud/lightsail`

  Expected: PASS.

- [ ] **Step 5: Write failing SSH tests**

  Use an in-process SSH server with generated keys. Assert exact AWS-witnessed host-key acceptance, mismatch rejection before command execution, OpenSSH certificate/private-key authentication parsing, the exact `/usr/bin/sudo -n /bin/bash -seu` command, single stdin delivery, connect/run timeout distinction, non-zero exit capture, final-32-KiB output bounding, UTF-8 normalization, ANSI/control stripping, and masking of common password/token/secret/authorization assignments.

- [ ] **Step 6: Run SSH tests and verify RED**

  Run: `go test ./internal/cloud/sshdeploy`

  Expected: FAIL because the runner package is absent.

- [ ] **Step 7: Add `golang.org/x/crypto/ssh` and implement the runner**

  Build an exact host-public-key set from AWS data; do not use `InsecureIgnoreHostKey`, TOFU, shell command concatenation, or temporary key files. Combine bounded stdout/stderr and return whether the remote command crossed `Start`.

- [ ] **Step 8: Run package tests and verify GREEN**

  Run: `go test ./internal/cloud/lightsail ./internal/cloud/sshdeploy`

  Expected: PASS with no network access.

- [ ] **Step 9: Commit**

  ```bash
  git add internal/cloud/lightsail internal/cloud/sshdeploy go.mod go.sum
  git commit -m "feat: add Lightsail and verified SSH adapters"
  ```

### Task 3: PostgreSQL Lifecycle Facts and Admin Commands

**Files:**
- Create: `internal/platform/migration39.sql`
- Create: `internal/platform/lightsail.go`
- Create: `internal/platform/lightsail_test.go`
- Modify: `internal/platform/store.go`
- Modify: `internal/platform/migration_regression_test.go`

**Interfaces:**
- Produces: `platform.LightsailInstance`, `LightsailJob`, `LightsailCreateInput`, and `LightsailCatalogSelection` JSON-safe types.
- Produces: `AdminCreateLightsailInstance(ctx, who, idempotencyKey, input) (LightsailInstance, error)`.
- Produces: `AdminLightsailInstances(ctx, who) ([]LightsailInstance, error)`.
- Produces: `AdminRequestLightsailStop(ctx, who, id string, revision int64) (LightsailJob, error)`.
- Produces: `AdminRequestLightsailDestroy(ctx, who, id string, revision int64, confirmation string) (LightsailJob, error)`.
- Produces: leased worker methods `ClaimLightsailJob`, `RetryLightsailJob`, `AdvanceLightsailJob`, `BeginLightsailExecution`, `CompleteLightsailExecution`, and `CompleteLightsailJob`, all requiring exact job/lease/stage identity.

- [ ] **Step 1: Write the migration and state tests first**

  Add real PostgreSQL tests for v39 schema constraints, one active job per instance, unique AWS/static-IP names, immutable create request identity, nullable script clearing only when `execution_started_at` is set, bounded fields, and a v38-to-current migration that preserves existing billing/node/source-flow/TRON facts.

- [ ] **Step 2: Run migration tests and verify RED**

  Run: `go test -race ./internal/platform -run 'TestMigration39|TestLightsail'`

  Expected: FAIL because migration v39 and Lightsail store types are absent.

- [ ] **Step 3: Implement migration v39 and register it**

  Add the two tables, checks, indexes, mutation guard for script reintroduction, embedded SQL, `SchemaVersion = 39`, and the ordered migration block. Keep script body out of every view/list projection.

- [ ] **Step 4: Write failing admin-command tests**

  Cover platform permission, tenant/API-key denial, 64-KiB UTF-8 script bounds, deterministic provider names, idempotent replay, changed-payload conflict, audit rollback, stop revision conflict, stop replay, destroy-running refusal, exact confirmation, ownership evidence requirements, and absence of script/AWS secrets in audit details.

- [ ] **Step 5: Run command tests and verify RED**

  Run: `go test -race ./internal/platform -run 'TestLightsail'`

  Expected: FAIL on missing admin/store methods.

- [ ] **Step 6: Implement admin commands and leased transitions**

  Follow existing `adminTx`, `platformLog`, UUID, revision, advisory-lock, and lease patterns. `BeginLightsailExecution` must atomically return the script to the claiming worker, set the boundary, and clear the stored body; a later claim with a crossed non-terminal boundary must finish `unknown` without returning a script.

- [ ] **Step 7: Run platform tests and verify GREEN**

  Run: `go test -race ./internal/platform -run 'TestMigration39|TestLightsail'`

  Expected: PASS.

- [ ] **Step 8: Commit**

  ```bash
  git add internal/platform/migration39.sql internal/platform/lightsail.go internal/platform/lightsail_test.go internal/platform/store.go internal/platform/migration_regression_test.go
  git commit -m "feat: persist Lightsail lifecycle jobs"
  ```

### Task 4: Restart-safe Lightsail Worker

**Files:**
- Create: `cmd/reladock/lightsail_worker.go`
- Create: `cmd/reladock/lightsail_worker_test.go`

**Interfaces:**
- Consumes: Task 1 `lightsail.Provider`, Task 2 `sshdeploy.Runner`, and Task 3 leased store methods.
- Produces: `processLightsailJob(ctx context.Context, store lightsailJobStore, provider lightsail.Provider, runner sshdeploy.Runner, cfg lightsail.Config) error`.
- Produces: `runLightsailWorker(ctx context.Context, ...)` with configured cadence and per-cycle deadline.

- [ ] **Step 1: Write failing create/deploy worker tests**

  Use deterministic fakes to cover every design stage: reconcile existing instance before create, uncertain create response then name/tag recovery without a second create, exact operation polling, firewall before static IP, allocation/attach recovery, temporary access readiness, host-key handoff, success, clean non-zero exit, connect failure before boundary retry, process loss after boundary terminal unknown, timeout/lost connection after `Started=true` terminal unknown, and no second runner call.

- [ ] **Step 2: Run worker tests and verify RED**

  Run: `go test ./cmd/reladock -run 'TestLightsailWorker'`

  Expected: FAIL because the worker is absent.

- [ ] **Step 3: Implement the minimal create/deploy state machine**

  Process one leased stage per cycle, persist every external operation identity before polling, classify only known provider/readiness failures for bounded retry, and never log provider bodies or script material.

- [ ] **Step 4: Run create/deploy tests and verify GREEN**

  Run: `go test ./cmd/reladock -run 'TestLightsailWorkerCreate|TestLightsailWorkerDeploy|TestLightsailWorkerExecution'`

  Expected: PASS.

- [ ] **Step 5: Write failing stop/destroy worker tests**

  Cover already-stopped reconciliation, stop operation replay, refusal to overlap an active job, destroy ownership/tag mismatch to `manual-review`, exact staged detach/delete/release recovery after a crash at every boundary, expected provider absence, unexpected absence, and no force-delete branch.

- [ ] **Step 6: Run stop/destroy tests and verify RED**

  Run: `go test ./cmd/reladock -run 'TestLightsailWorkerStop|TestLightsailWorkerDestroy'`

  Expected: FAIL on unimplemented job kinds/stages.

- [ ] **Step 7: Implement stop/destroy state machines and the run loop**

  Preserve static IP on stop. Require reconciled `stopped` before destroy. Verify deterministic name, static-IP name, and RelaDock local-ID tag before every destructive call.

- [ ] **Step 8: Run worker tests and verify GREEN**

  Run: `go test ./cmd/reladock -run 'TestLightsailWorker'`

  Expected: PASS.

- [ ] **Step 9: Commit**

  ```bash
  git add cmd/reladock/lightsail_worker.go cmd/reladock/lightsail_worker_test.go
  git commit -m "feat: orchestrate Lightsail node lifecycle"
  ```

### Task 5: Strict Platform-admin HTTP API

**Files:**
- Create: `internal/web/lightsail.go`
- Create: `internal/web/lightsail_http_test.go`
- Modify: `internal/web/server.go`

**Interfaces:**
- Consumes: Task 1 catalog provider and Task 3 platform commands.
- Produces: `web.LightsailOptions{Config lightsail.Config, Provider lightsail.Provider}` in `web.Options`.
- Produces: `GET /api/admin/lightsail/status`, `GET /catalog?region=`, `GET /instances`, `POST /instances`, `POST /instances/:id/stop`, and `POST /instances/:id/destroy`.

- [ ] **Step 1: Write failing HTTP tests**

  Assert disabled status without provider calls, non-admin/API-key denial, strict query/body and method handling, 96-KiB create body limit, required `Idempotency-Key`, region allowlist, active Linux catalog filtering, create `202`, stop/destroy revision and confirmation handling, script never returned by list/create, sanitized provider errors, and no state mutation when provider/catalog validation fails.

- [ ] **Step 2: Run HTTP tests and verify RED**

  Run: `go test -race ./internal/web -run 'TestLightsail'`

  Expected: FAIL because the route and options are absent.

- [ ] **Step 3: Implement the route as a separate handler**

  Route it before generic `/api/admin/*`, reuse existing session/origin/Host/rate-limit protections, and recheck platform permission in every store call. The catalog provider call receives the request context and never accepts endpoint URLs from the browser.

- [ ] **Step 4: Run HTTP tests and verify GREEN**

  Run: `go test -race ./internal/web -run 'TestLightsail'`

  Expected: PASS.

- [ ] **Step 5: Commit**

  ```bash
  git add internal/web/lightsail.go internal/web/lightsail_http_test.go internal/web/server.go
  git commit -m "feat: expose Lightsail admin API"
  ```

### Task 6: Separate Lightsail Admin Page

**Files:**
- Create: `public/lightsail-admin.js`
- Create: `public/lightsail-admin.test.js`
- Modify: `public/admin.html`
- Modify: `public/admin.js`
- Modify: `public/admin.css`
- Modify: `public/assets.go`
- Modify: `public/assets_test.go`
- Modify: `internal/web/server.go`
- Modify: `internal/web/static_assets_test.go`

**Interfaces:**
- Consumes: Task 5 API.
- Produces: pure presentation/action helpers exported from `lightsail-admin.js` and `bindLightsailAdmin(options)` for the real DOM.

- [ ] **Step 1: Write failing Node behavior tests**

  Test status/catalog loading, stale catalog response isolation when region changes, deterministic create payload, unique idempotency key retention across uncertain retries, disabled presentation, one-time script warning, safe text rendering, state/action labels, stop revision payload, destroy exact-name confirmation payload, poll stop on hidden page/logout, and no script/credential rendering.

- [ ] **Step 2: Run Node tests and verify RED**

  Run: `node --test public/lightsail-admin.test.js`

  Expected: FAIL because the module is absent.

- [ ] **Step 3: Write failing embedded/static route tests**

  Require the new navigation route, pane, create/stop/destroy dialogs, 64-KiB script field, boundary copy, module import, embedded asset, GET/HEAD content type, and absence of credential fields.

- [ ] **Step 4: Run static tests and verify RED**

  Run: `go test ./public ./internal/web -run 'Test.*Lightsail.*Asset|Test.*Lightsail.*Presentation'`

  Expected: FAIL on missing markup/module/route.

- [ ] **Step 5: Implement the admin module and minimal scoped styling**

  Add `#/lightsail` without changing existing page meanings. Use dialogs rather than browser `prompt`/`confirm`, text nodes rather than HTML interpolation, visible 5-second polling and hidden 30-second polling without overlap, and retain the last successful state on transient read failure.

- [ ] **Step 6: Run frontend/static tests and verify GREEN**

  Run: `node --test public/lightsail-admin.test.js && go test ./public ./internal/web -run 'Test.*Lightsail.*Asset|Test.*Lightsail.*Presentation'`

  Expected: PASS.

- [ ] **Step 7: Commit**

  ```bash
  git add public/lightsail-admin.js public/lightsail-admin.test.js public/admin.html public/admin.js public/admin.css public/assets.go public/assets_test.go internal/web/server.go internal/web/static_assets_test.go
  git commit -m "feat: add Lightsail admin workspace"
  ```

### Task 7: Runtime Wiring and Failure-closed Startup

**Files:**
- Modify: `cmd/reladock/main.go`
- Create: `cmd/reladock/lightsail_runtime_test.go`
- Modify: `internal/observability/metrics.go`
- Modify: `internal/observability/metrics_test.go`

**Interfaces:**
- Consumes: Tasks 1–5 config/provider/runner/worker/web options.
- Produces: one connector instance shared by the admin catalog handler and worker, started only when enabled.
- Produces: fixed-cardinality worker outcome metric `lightsail_lifecycle` without account, region, IP, instance, or job labels.

- [ ] **Step 1: Write failing runtime tests**

  Assert `--check-config` validates connector fields without loading AWS credentials or connecting to AWS, disabled startup constructs no provider/runner/worker, enabled wiring shares one provider, cancellation stops the worker, provider construction errors are sanitized, and metrics contain no customer/cloud resource identifiers.

- [ ] **Step 2: Run runtime tests and verify RED**

  Run: `go test ./cmd/reladock ./internal/observability -run 'TestLightsail'`

  Expected: FAIL because runtime wiring and metric are absent.

- [ ] **Step 3: Implement failure-closed runtime wiring**

  Construct AWS/SSH implementations only after config validation and only outside `--check-config`; inject options into web; start the worker under the existing signal context; log only fixed sanitized failure messages.

- [ ] **Step 4: Run runtime tests and verify GREEN**

  Run: `go test ./cmd/reladock ./internal/observability -run 'TestLightsail'`

  Expected: PASS.

- [ ] **Step 5: Commit**

  ```bash
  git add cmd/reladock/main.go cmd/reladock/lightsail_runtime_test.go internal/observability/metrics.go internal/observability/metrics_test.go
  git commit -m "feat: wire disabled-by-default Lightsail worker"
  ```

### Task 8: Documentation, Full Verification, and Status Boundary

**Files:**
- Modify: `README.md`
- Modify: `docs/REQUIREMENTS.md`
- Modify: `docs/ARCHITECTURE.md`
- Modify: `docs/ROADMAP.md`
- Modify: `docs/DEPLOYMENT.md`
- Modify: `docs/OPERATIONS.md`
- Modify: `docs/IMPLEMENTATION_STATUS.md`

**Interfaces:**
- Consumes: all implemented behavior and exact verification evidence from Tasks 1–7.
- Produces: operator configuration/IAM guidance and an honest implemented-versus-unverified handoff.

- [ ] **Step 1: Update documentation from observed behavior**

  Document the separate generic-node boundary, required least-privilege IAM actions, standard AWS credential chain, opt-in region prerequisite, source-CIDR firewall window, script sensitivity, at-most-once unknown outcome, stop-before-destroy flow, API throttling/backoff, and the fact that no live AWS acceptance occurred.

- [ ] **Step 2: Run focused package tests**

  Run: `go test -race ./internal/cloud/lightsail ./internal/cloud/sshdeploy ./internal/platform ./internal/web ./cmd/reladock ./internal/observability`

  Expected: PASS.

- [ ] **Step 3: Run Node tests**

  Run: `node --test`

  Expected: all enabled tests pass; report the existing opt-in Linux installer skip separately.

- [ ] **Step 4: Run the repository verification commands**

  Run: `npm test`

  Expected: PASS against a real isolated PostgreSQL test database; do not claim a full pass if the database is unavailable or any package fails.

  Run: `npm run vet`

  Expected: PASS.

  Run: `git diff --check`

  Expected: PASS.

- [ ] **Step 5: Inspect the final diff for unrelated overlap and secrets**

  Run: `git status --short --branch`, scoped `git diff -- <touched paths>`, and repository secret-pattern searches that print only filenames/counts rather than candidate secret values.

  Expected: no unrelated user change modified, no AWS key/temporary SSH key/script body fixture resembling a live secret, and no production deployment performed.

- [ ] **Step 6: Record exact evidence and unverified boundaries**

  Append commands, pass/fail/skip counts, provider-substitute scope, and the absence of live AWS/SSH/purchase/destroy verification to `docs/IMPLEMENTATION_STATUS.md`.

- [ ] **Step 7: Commit**

  ```bash
  git add README.md docs/REQUIREMENTS.md docs/ARCHITECTURE.md docs/ROADMAP.md docs/DEPLOYMENT.md docs/OPERATIONS.md docs/IMPLEMENTATION_STATUS.md
  git commit -m "docs: describe Lightsail script nodes"
  ```
