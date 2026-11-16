# Google Login Implementation Plan

> **2026-11-16 security amendment:** Independent review found that Google `email_verified` alone cannot prove ownership of a pre-existing RelaDock account, especially because RelaDock registration does not separately verify mailbox ownership. The approved implementation therefore supersedes the direct-email-binding steps below: an unbound callback creates only a 10-minute `google_link_challenges` record with a hashed random token and a Strict, endpoint-scoped Cookie. `POST /api/auth/google/link` must then verify the same existing email and password before binding, creating a session, consuming the challenge, and auditing in one transaction. The Nginx callback location also disables access logging so code/state query values are not persisted. The original task sequence remains as implementation history; this amendment is authoritative wherever it conflicts.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add optional Google OpenID Connect login for existing RelaDock members without changing tenant, role, platform-admin, password-login, Agent, data-plane, or billing boundaries.

**Architecture:** A new `internal/googleauth` adapter owns Google OAuth2/OIDC protocol validation behind a small provider interface. PostgreSQL schema v41 stores one-time login attempts, hashed first-link challenges, and durable `(provider, subject)` member bindings; the existing web server orchestrates same-origin start, cross-site callback, password-confirmed first binding, session creation, and fixed redirects. Static enterprise/admin pages share a small Google-login controller while all authorization continues to come from current RelaDock database state.

**Tech Stack:** Go 1.24, PostgreSQL, `github.com/coreos/go-oidc/v3` v3.17.0, `golang.org/x/oauth2` v0.35.0, static ES modules, Node.js test runner.

**Spec:** `docs/superpowers/specs/2026-11-16-google-login-design.md`

## Global Constraints

- Google login serves existing `members` only; it never creates a tenant/member or grants a role/platform permission.
- First authorization never links or creates a session by email alone; it requires the existing same-email RelaDock password. Later login resolves only the stable Google `sub`.
- Request only `openid email`; never persist Google authorization codes/tokens/raw claims, picture, or name. Persist only hashed state plus nonce/PKCE verifier in the bounded 10-minute login-attempt table, and never log or return those attempt secrets outside the required opaque state Cookie and Google authorization URL.
- Google auth is disabled when both settings are empty and startup fails closed on partial/invalid configuration.
- Preserve password registration/login, invitations, password changes, session revocation, tenant suspension, and API-key behavior.
- Preserve all pre-existing dirty-worktree changes, especially current edits in `README.md`, `docs/*`, `public/index.html`, and `public/app.css`; use surgical patches and inspect the final diff.
- Do not connect a real Google project, add real credentials, deploy production, or change Agent/data-plane/payment state.

## Review Focus

- Concurrent password-confirmed first links for one Google subject must create one binding and separate valid RelaDock sessions without rebinding; pinned by `TestCompleteGoogleLoginConcurrentFirstBinding` in Task 2.
- A Google subject already linked to member A must never move to member B after its asserted email changes; pinned by `TestCompleteGoogleLoginUsesSubjectAfterBindingAndRejectsConflicts` in Task 2.
- Callback replay, expired attempts, mismatched cookie/query state, and nonce mismatch must create neither session nor binding; pinned by `TestGoogleCallbackRejectsReplayExpiryStateAndNonce` in Task 3.
- A suspended tenant or changed platform role must be re-read at login time, never inherited from claims or stale state; pinned by `TestCompleteGoogleLoginRechecksTenantAndPrivileges` in Task 2.
- Provider/network errors and failed first-link credentials must expose only an enumerated generic browser error and no secret material; an unknown email may receive a challenge but can never bind without the existing password.

---

### Task 1: Google OIDC configuration and provider adapter

**Files:**
- Create: `internal/googleauth/config.go`
- Create: `internal/googleauth/provider.go`
- Create: `internal/googleauth/config_test.go`
- Create: `internal/googleauth/provider_test.go`
- Modify: `internal/runtimeconfig/config.go`
- Modify: `internal/runtimeconfig/config_test.go`
- Modify: `go.mod`
- Modify: `go.sum`

**Interfaces:**
- Produces: `googleauth.Config{ClientID, ClientSecret, RedirectURL string}`, `Config.Enabled() bool`, and `LoadConfig(get func(string) string, origin string) (Config, error)`.
- Produces: `googleauth.Claims{Subject, Email string}`, `googleauth.Provider` with `AuthCodeURL(state, nonce, verifier string) string` and `Exchange(ctx context.Context, code, verifier, nonce string) (Claims, error)`.
- Produces: `googleauth.New(Config) (Provider, error)` using fixed Google issuer/endpoints, PKCE S256, ID-token signature/issuer/audience/expiry/nonce validation, and a hardened bounded HTTP client.
- Produces: `runtimeconfig.Config.GoogleOIDC googleauth.Config`; config loading performs no network access, including `--check-config`.

- [ ] **Step 1: Write failing configuration tests**

Add `TestLoadConfigDisabledCompleteAndPartial`, `TestLoadConfigDerivesExactCallback`, and runtime-config integration assertions:

```go
cfg, err := LoadConfig(mapGet(map[string]string{}), "https://console.example.com")
if err != nil || cfg.Enabled() { t.Fatalf("disabled: %+v %v", cfg, err) }

cfg, err = LoadConfig(mapGet(map[string]string{
    "GOOGLE_OIDC_CLIENT_ID": "fixture.apps.googleusercontent.com",
    "GOOGLE_OIDC_CLIENT_SECRET": "fixture-secret",
}), "https://console.example.com")
if err != nil || cfg.RedirectURL != "https://console.example.com/api/auth/google/callback" { t.Fatal(...) }
```

Assert either single variable, whitespace/control characters, a non-`.apps.googleusercontent.com` client ID, or a non-exact origin returns an error that never contains the secret.

- [ ] **Step 2: Run configuration tests to verify RED**

Run: `node scripts/start.mjs test ./internal/googleauth ./internal/runtimeconfig -run 'TestLoadConfig|TestGoogleOIDC'`

Expected: FAIL because `internal/googleauth` and `Config.GoogleOIDC` do not exist.

- [ ] **Step 3: Write failing provider protocol tests**

In `provider_test.go`, use an `httptest` token/JWKS server and a test RSA key to exercise real OAuth exchange and ID-token verification. Assert the authorization URL contains `response_type=code`, exact client/redirect, `scope=openid email`, state, nonce, and `code_challenge_method=S256`; assert exchange sends the verifier and rejects wrong audience, issuer, expiry, nonce, missing subject, unverified email, oversized response, redirect, or non-200 response. Assert no URL/error includes the client secret or returned tokens.

- [ ] **Step 4: Run provider tests to verify RED**

Run: `node scripts/start.mjs test ./internal/googleauth -run 'TestProvider'`

Expected: FAIL because the provider implementation is absent.

- [ ] **Step 5: Add compatible OIDC dependencies and implement the adapter**

Run: `go get github.com/coreos/go-oidc/v3@v3.17.0 golang.org/x/oauth2@v0.35.0`

Implement the interfaces above. Construct the provider from fixed Google metadata without startup discovery; use `oidc.ProviderConfig`, `oidc.NewRemoteKeySet`, `oauth2.Config`, `oauth2.S256ChallengeOption`, and `oauth2.VerifierOption`. Pin issuer/audience/expiry/signature checks, compare nonce with constant-time semantics, require verified email, and use a no-environment-proxy client with short timeout, bounded responses, and redirect rejection.

- [ ] **Step 6: Integrate config loading after `PUBLIC_ORIGIN` validation**

Populate `Config.GoogleOIDC` only after `runtimeconfig.Load` has derived and validated the exact origin. Ensure normal startup and `--check-config` share the same field validation and neither contacts Google.

- [ ] **Step 7: Run Task 1 tests and dependency hygiene**

Run: `node scripts/start.mjs test -race ./internal/googleauth ./internal/runtimeconfig`

Run: `go mod tidy`

Expected: PASS; `go.mod` remains `go 1.24.0`, with OIDC v3.17.0 and OAuth2 v0.35.0.

- [ ] **Step 8: Commit Task 1 when isolated**

```bash
git add go.mod go.sum internal/googleauth internal/runtimeconfig/config.go internal/runtimeconfig/config_test.go
git commit -m "feat: add Google OIDC provider"
```

If execution remains in the current dirty checkout, do not stage overlapping user changes; record the checkpoint and defer commits.

---

### Task 2: Schema v41 and transactional member binding

**Files:**
- Create: `internal/platform/migration41.sql`
- Create: `internal/platform/google_auth.go`
- Create: `internal/platform/google_auth_test.go`
- Modify: `internal/platform/store.go`
- Modify: `internal/platform/store_test.go`
- Modify: `internal/platform/migration_regression_test.go`

**Interfaces:**
- Consumes: normalized member emails, existing `Identity`, session token hashing, `platformLock`, and tenant/member audit conventions.
- Produces: `SchemaVersion = 41` with `member_external_identities`, `oauth_login_attempts`, and `google_link_challenges`.
- Produces: `OAuthLoginAttempt{Verifier, Nonce, ReturnTo string}` and `GoogleIdentity{Subject, Email string; EmailVerified bool}`.
- Produces: `Store.StartGoogleLogin(ctx, state, verifier, nonce, returnTo string) error`.
- Produces: `Store.ConsumeGoogleLogin(ctx, state string) (OAuthLoginAttempt, error)`.
- Produces: `Store.CompleteGoogleLogin(...) (GoogleLoginResult, error)` for bound login or a pending challenge, plus `Store.CompleteGoogleLink(...)` for password-confirmed binding.

- [ ] **Step 1: Write failing migration tests**

Add `TestMigration41GoogleIdentityConstraints` and extend the schema-version assertion from 40 to 41. Verify:

```go
// one subject maps to one member; one member has at most one Google subject
// provider is exactly "google"; state hashes are 64 hex chars
// verifier/nonce/return target/expiry constraints reject malformed rows
// rerunning Migrate preserves members, sessions, audits, and existing v40 facts
```

- [ ] **Step 2: Run migration tests to verify RED**

Run: `node scripts/start.mjs test -race ./internal/platform -run 'TestMigration41|TestMigration40IsCurrentSchemaVersion'`

Expected: FAIL because schema v41 and its tables are absent.

- [ ] **Step 3: Implement migration41 and register it atomically**

Create both tables with foreign keys to `members`, uniqueness on `(provider, subject)` and `(provider, member_id)`, fixed return targets `/` and `/admin`, bounded field checks, expiry index, and no token columns. Embed and apply migration 41 in `Store.Migrate`, recording version 41 in the same transaction.

- [ ] **Step 4: Write failing login-attempt tests**

Add `TestGoogleLoginAttemptIsHashedSingleUseAndExpires`: start an attempt, assert plaintext state is absent from the row, consume once, reject replay, reject expiry, and verify invalid return targets/verifier/nonce write nothing.

- [ ] **Step 5: Run attempt tests to verify RED**

Run: `node scripts/start.mjs test -race ./internal/platform -run TestGoogleLoginAttempt`

Expected: FAIL because the Store methods are absent.

- [ ] **Step 6: Implement bounded one-time attempts**

`StartGoogleLogin` validates token lengths and the fixed return target, stores `tokenHash(state)`, verifier, nonce, and `now()+10m`, and deletes at most a bounded batch of already expired/consumed rows. `ConsumeGoogleLogin` atomically marks exactly one unexpired row consumed and returns only verifier/nonce/return target; replay and expiry return `ErrUnauthorized`.

- [ ] **Step 7: Write failing member binding tests**

Add these real-PostgreSQL tests:

- `TestCompleteGoogleLoginLinksExistingVerifiedEmailAndCreatesSession`
- `TestCompleteGoogleLoginRejectsUnknownOrUnverifiedEmailWithoutWrites`
- `TestCompleteGoogleLoginUsesSubjectAfterBindingAndRejectsConflicts`
- `TestCompleteGoogleLoginConcurrentFirstBinding`
- `TestCompleteGoogleLoginRechecksTenantAndPrivileges`
- `TestCompleteGoogleLoginAuditFailureRollsBackBindingAndSession`

Assert the first success writes one `account.google_link`, every success writes `account.login`, platform-admin state comes from the current member row, email changes after binding do not move the subject, and no provider tokens can be found in any table.

- [ ] **Step 8: Run binding tests to verify RED**

Run: `node scripts/start.mjs test -race ./internal/platform -run TestCompleteGoogleLogin`

Expected: FAIL because `CompleteGoogleLogin` is absent.

- [ ] **Step 9: Implement transactional binding and login**

Normalize/verify claims at the Store boundary. Acquire the existing platform authorization lock, resolve by subject first, otherwise lock the active member by normalized email, insert the Google binding under unique constraints, re-read current tenant/role/platform state, create a fresh 24-hour session, and audit link/login in the same transaction. Convert uniqueness races to a re-read only when the existing subject belongs to the same member; otherwise reject generically.

- [ ] **Step 10: Run Task 2 tests**

Run: `node scripts/start.mjs test -race ./internal/platform -run 'TestMigration41|TestGoogleLoginAttempt|TestCompleteGoogleLogin|TestAccountSessions'`

Expected: PASS with real PostgreSQL; no skipped database tests.

- [ ] **Step 11: Commit Task 2 when isolated**

```bash
git add internal/platform/migration41.sql internal/platform/google_auth.go internal/platform/google_auth_test.go internal/platform/store.go internal/platform/store_test.go internal/platform/migration_regression_test.go
git commit -m "feat: bind Google identities to members"
```

If execution remains in the current dirty checkout, defer the commit rather than including unrelated changes.

---

### Task 3: Secure HTTP start, callback, cookies, and fixed redirects

**Files:**
- Create: `internal/web/google_auth.go`
- Create: `internal/web/google_auth_http_test.go`
- Modify: `internal/web/server.go`
- Modify: `cmd/reladock/main.go`

**Interfaces:**
- Consumes: `googleauth.Provider`, `Store.StartGoogleLogin`, `Store.ConsumeGoogleLogin`, and `Store.CompleteGoogleLogin`.
- Produces: public `GET /api/auth/google/status` → `{enabled:boolean}`.
- Produces: same-origin `POST /api/auth/google/start` with `{returnTo:"/"|"/admin"}` → `{authorizationURL:string}` plus a 10-minute callback-scoped state Cookie.
- Produces: `GET /api/auth/google/callback` → clears the state Cookie; bound subjects receive the Strict session, while unbound subjects receive only a Strict link challenge and `googleAuth=link`. `POST /api/auth/google/link` verifies the existing password before creating the session.
- Produces: `web.Options.GoogleAuth googleauth.Provider`; `cmd/reladock` constructs the provider only when configured.

- [ ] **Step 1: Write failing status/start tests**

Using a fake `googleauth.Provider`, add `TestGoogleAuthStatusAndStartAreOptionalSameOriginAndBounded`. Assert disabled status, strict JSON, missing/cross-site header rejection, only `/` and `/admin`, auth rate-limit classification, 43-character random state/nonce/verifier, authorization URL propagation, and state Cookie attributes: HttpOnly, SameSite=Lax, callback-only Path, 600-second lifetime, Secure under HTTPS.

- [ ] **Step 2: Run start tests to verify RED**

Run: `node scripts/start.mjs test -race ./internal/web -run TestGoogleAuthStatusAndStart`

Expected: FAIL because routes/options do not exist.

- [ ] **Step 3: Implement optional status/start routing**

Add the provider to `server`, route status before session authentication, generate cryptographic URL-safe values, persist the attempt before returning the URL, and classify start/callback in the existing auth budget. Never return secret config or raw attempt values except the opaque state Cookie and Google URL required by the browser.

- [ ] **Step 4: Write failing callback tests**

Add:

- `TestGoogleCallbackLinksExistingMemberAndSetsStrictSession`
- `TestGoogleCallbackReturnsAdminToAdminWithoutGrantingPrivilege`
- `TestGoogleCallbackRejectsReplayExpiryStateAndNonce`
- `TestGoogleCallbackRejectsUnknownUnverifiedAndSubjectConflict`
- `TestGoogleCallbackErrorsAreBoundedAndSecretFree`

Assert successful callbacks use 303 and clear the Lax attempt Cookie. Bound subjects set the existing Strict session Cookie; unbound subjects set only the endpoint-scoped Strict link Cookie and require a successful password-confirmation POST before `/api/auth/me` works. Assert failure paths create no session/binding, use only fixed destinations, and never echo code/state/nonce/verifier/token/provider error.

- [ ] **Step 5: Run callback tests to verify RED**

Run: `node scripts/start.mjs test -race ./internal/web -run TestGoogleCallback`

Expected: FAIL because callback orchestration is absent.

- [ ] **Step 6: Implement callback and startup wiring**

Validate query/cookie state before consuming the attempt; consume once before token exchange; handle provider denial without exchange; validate claims through the provider; call transactional completion; clear the attempt Cookie on every callback outcome. In `main`, construct `googleauth.New(cfg.GoogleOIDC)` without discovery/network and pass it through `web.Options`.

- [ ] **Step 7: Run HTTP/security regressions**

Run: `node scripts/start.mjs test -race ./internal/web -run 'TestGoogle|TestHTTPRejectsCrossSite|TestAuthenticationRateLimit|TestAuthenticatedRuleFlow'`

Expected: PASS; existing password flow and CSRF/Host/rate-limit tests remain green.

- [ ] **Step 8: Commit Task 3 when isolated**

```bash
git add internal/web/google_auth.go internal/web/google_auth_http_test.go internal/web/server.go cmd/reladock/main.go
git commit -m "feat: add Google login callback flow"
```

If execution remains in the current dirty checkout, defer the commit rather than staging unrelated hunks.

---

### Task 4: Enterprise and platform login UI

**Files:**
- Create: `public/google-login.js`
- Create: `test/google-login.test.mjs`
- Modify: `public/assets.go`
- Modify: `public/assets_test.go`
- Modify: `public/index.html`
- Modify: `public/admin.html`
- Modify: `public/app.js`
- Modify: `public/admin.js`
- Modify: `public/app.css`
- Modify: `public/admin.css`
- Modify: `internal/web/static_assets_test.go`

**Interfaces:**
- Consumes: public Google status/start endpoints and the fixed `googleAuth=failed` query marker.
- Produces: `createGoogleLoginController({api, returnTo, button, message, location, history})` with `load()`, `start()`, and `consumeCallbackResult()` for `failed`/`link` markers.
- Produces: enterprise Google button visible only in login mode when enabled; admin Google button visible when enabled.

- [ ] **Step 1: Write failing controller tests**

In `test/google-login.test.mjs`, assert disabled status keeps the button hidden, enabled status reveals it, click requests the correct fixed return target then navigates only to the returned Google HTTPS URL, duplicate clicks are suppressed, failures stay on-page, and callback error consumption displays a generic Chinese message while removing the query marker with `history.replaceState`.

- [ ] **Step 2: Run controller tests to verify RED**

Run: `node --test test/google-login.test.mjs`

Expected: FAIL because `public/google-login.js` is absent.

- [ ] **Step 3: Implement the shared controller**

Keep URL validation browser-side as defense in depth: accept only HTTPS `accounts.google.com` authorization URLs. Do not place state or provider errors in DOM text. Expose a small state callback so the enterprise page can hide the entry during registration mode.

- [ ] **Step 4: Write failing static asset/UI tests**

Extend asset and web static tests to require the embedded module, module imports, both buttons, generic error live regions, no hidden secret config, enterprise login-mode-only behavior, and no removal of current password/register/invitation controls.

- [ ] **Step 5: Run static tests to verify RED**

Run: `node scripts/start.mjs test ./public ./internal/web -run 'Test.*(Static|Asset|Google)'`

Run: `node --test test/auth-session.test.mjs test/google-login.test.mjs`

Expected: FAIL because pages and embedding do not expose the new module.

- [ ] **Step 6: Add both login entries with surgical patches**

Add a compact divider/button/status block to the existing auth cards; import and bind the controller in `app.js` with return target `/` and `admin.js` with `/admin`. Extend `setMode` so registration hides the enterprise Google option without clearing password fields. Add focused styles to both CSS files without changing existing console/dialog/port-control rules.

- [ ] **Step 7: Run Task 4 tests**

Run: `node --test test/google-login.test.mjs test/auth-session.test.mjs`

Run: `node scripts/start.mjs test -race ./public ./internal/web -run 'Test.*(Static|Asset|Google)'`

Expected: PASS; existing password visibility and static delivery remain green.

- [ ] **Step 8: Commit Task 4 when isolated**

```bash
git add public/google-login.js test/google-login.test.mjs public/assets.go public/assets_test.go public/index.html public/admin.html public/app.js public/admin.js public/app.css public/admin.css internal/web/static_assets_test.go
git commit -m "feat: expose Google login in both consoles"
```

In the current checkout, `public/index.html` and `public/app.css` already contain user changes; do not commit whole-file overlap unless isolated.

---

### Task 5: Deployment documentation, status accounting, and full verification

**Files:**
- Modify: `deploy/control.env.example`
- Modify: `docs/DEPLOYMENT.md`
- Modify: `README.md`
- Modify: `docs/REQUIREMENTS.md`
- Modify: `docs/ARCHITECTURE.md`
- Modify: `docs/ROADMAP.md`
- Modify: `docs/IMPLEMENTATION_STATUS.md`

**Interfaces:**
- Consumes: verified code/test results from Tasks 1–4.
- Produces: exact Google Cloud redirect URI/config instructions and honest local-vs-production status.

- [ ] **Step 1: Add configuration and operational documentation**

Document:

```text
GOOGLE_OIDC_CLIENT_ID=<web client id>
GOOGLE_OIDC_CLIENT_SECRET=<server-only secret>
Authorized redirect URI: ${PUBLIC_ORIGIN}/api/auth/google/callback
Scopes: openid email
```

State that both variables absent disables the feature, partial configuration fails startup, provider secrets never enter browser/database, first-link requires the existing same-email password, callback access logging must be suppressed, and production Google consent/client/browser callback remains unverified until separately configured and authorized.

- [ ] **Step 2: Update product and implementation status surgically**

Add schema v41/authentication entries to requirements, architecture, roadmap, README, and the implementation-status feature matrix. Record no production deployment and no real Google credential/project/browser verification. Preserve every existing v39/v40/Lightsail and port-control status edit.

- [ ] **Step 3: Run focused package verification fresh**

Run: `node scripts/start.mjs test -race ./internal/googleauth ./internal/runtimeconfig ./internal/platform ./internal/web ./public`

Expected: PASS with real PostgreSQL; report exact durations/failures/skips.

- [ ] **Step 4: Run the required full suite**

Run: `npm test`

Expected: Go `-race` packages and Node tests pass; any known or new failure must be named rather than omitted.

- [ ] **Step 5: Run static and diff verification**

Run: `npm run vet`

Run: `git diff --check`

Run: `git status --short --branch`

Expected: vet and diff checks exit 0; status contains only preserved pre-existing changes plus the scoped Google-login work.

- [ ] **Step 6: Re-read the spec and inspect the final diff**

Check every design bullet against code/tests, search for credential/token leakage with `rg -n 'GOOGLE_OIDC|access_token|refresh_token|id_token|client_secret'`, and confirm only configuration names—not secret values or provider responses—appear in tracked output.

- [ ] **Step 7: Record evidence in implementation status**

Append the exact commands, pass/fail counts, skip reasons, schema v41 boundary, dependency versions, and unverified real-Google/production items. Do not claim production readiness.

- [ ] **Step 8: Commit documentation when isolated**

```bash
git add deploy/control.env.example docs/DEPLOYMENT.md README.md docs/REQUIREMENTS.md docs/ARCHITECTURE.md docs/ROADMAP.md docs/IMPLEMENTATION_STATUS.md
git commit -m "docs: document Google login boundaries"
```

In the current dirty checkout, defer this commit unless the Google hunks can be staged without including the user's unrelated documentation edits.
