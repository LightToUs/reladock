# Stripe Production Release Design

## Intent

Turn RelaDock's existing optional Stripe wallet-top-up connector into a
production-release implementation with an auditable customer flow, durable
webhook processing, explicit test/live separation, and an operator workflow for
refund and dispute reconciliation.

This is a production-hardening and acceptance project, not an instruction to
insert live credentials or take real money immediately. Code completion,
sandbox acceptance, live merchant activation, and real-money acceptance remain
four separate facts.

## Current State

The repository already has:

- server-created Stripe Checkout Sessions with fixed API host, request timeout,
  provider idempotency, and hosted-URL validation;
- a local tenant-scoped idempotent payment order created before contacting
  Stripe;
- raw-body webhook signature verification, live/test and Connect rejection,
  amount/currency/session/metadata binding, event deduplication, and immutable
  wallet ledger credits;
- full-refund reversal when the wallet still contains the funds;
- durable review incidents plus a wallet spending freeze for ambiguous refunds
  and every matched dispute;
- an enterprise checkout page which correctly treats Stripe as wallet funding,
  not as proof that a package has been purchased;
- a configured Stripe sandbox endpoint on the production control host, without
  a signed end-to-end credit test or live money movement.

The connector is not ready to be described as production-released because the
webhook still performs accounting synchronously, package funding trusts a
browser-submitted amount, test/live mode is not explicit to operators or
customers, Checkout expiration/failure has no terminal local state, and there
is no platform payment-review or safe unfreeze workflow.

## Success Definition

The code is release-ready when all of the following are true:

1. Package funding is priced by the server from an immutable price-version ID;
   changing a browser amount cannot change either the Stripe charge or wallet
   credit.
2. A verified webhook is durably accepted before returning 2xx, then applied by
   a retrying worker. Duplicate and out-of-order events remain safe.
3. Customers can see the truthful local state of their own Stripe orders after
   returning from Checkout; the redirect never means paid.
4. Platform administrators can see connector mode, queued/failed webhook work,
   orders, and review incidents, and can resolve incidents through an audited
   action without editing ledger rows or using SQL.
5. A wallet can only be unfrozen by an explicit platform-admin reconciliation
   action after all matched incidents for that tenant are resolved. It is never
   automatically unfrozen by a later Stripe event.
6. Runtime configuration explicitly selects `test` or `live`, and keys,
   webhook events, UI labels, and readiness reporting must agree with that
   selection.
7. Automated tests pass against isolated PostgreSQL schemas, and the sandbox
   acceptance run proves Checkout creation, signed webhook delivery, one wallet
   credit, duplicate delivery, a refund/review path, and operator resolution.

Live merchant activation is complete only after a separately authorized run
with live credentials and a documented real-money payment/refund/dispute
exercise. Until then documentation must say "code release-ready" or "sandbox
accepted", not "live payments active".

## Product and Accounting Boundaries

- Stripe continues to fund the enterprise USD wallet. It does not directly
  create or renew a package period.
- After webhook-confirmed credit, the customer uses the existing atomic wallet
  purchase transaction to buy the package.
- RelaDock does not add Stripe Billing subscriptions, saved cards, automatic
  external-card renewal, Connect accounts, multiple currencies, promotion
  codes, taxes, partial-refund automation, or automatic dispute resolution.
- Existing arbitrary wallet top-ups remain supported within the current
  USD 0.50–10,000 safety bound. Package funding is a distinct purpose whose
  amount is always derived on the server.
- Ledger entries remain append-only integer cents. Incident resolution never
  edits or deletes ledger history. Any authorized balance correction continues
  to use the existing acknowledged wallet-adjustment operation.

## Customer Flow

### Package funding

1. The customer selects a published package price version in the existing
   package flow.
2. The browser submits `priceVersionId` and an idempotency key. It does not
   submit an authoritative amount.
3. In a transaction, the server reauthorizes the member, locks and reloads the
   immutable price version and its line offering/node sales gates, derives the
   USD price, and creates or replays a `plan-funding` payment order.
4. The server creates one Stripe Checkout Session for the local order. Session
   and PaymentIntent metadata bind order, tenant, purpose, price version,
   integer amount, and USD currency.
5. The customer is redirected to Stripe. Success/cancel return URLs only bring
   the customer back to the checkout page.
6. The page restores the tenant-scoped pending order from session storage and
   reads its server state. It shows pending, credited, review required,
   expired/cancelled, or refunded; it never infers payment from the query string.
7. After a credited state appears, the customer explicitly completes the
   existing wallet package purchase. If the offering became unavailable in the
   meantime, the money remains truthful wallet balance and no package is
   fabricated.

### Arbitrary wallet top-up

The existing top-up form submits `amountCents` with purpose `wallet-topup`.
`amountCents` and `priceVersionId` are mutually exclusive. The server applies
the current min/max bound and stores the purpose so operators can distinguish a
free top-up from package funding.

### Order visibility

Authenticated enterprise APIs expose only the caller's tenant orders, newest
first, with local order ID, purpose, optional package name/version, integer
amount/currency, financial state, review state, and timestamps. They never
expose Stripe secrets, webhook payloads, Checkout Session IDs, PaymentIntent
IDs, tenant IDs, or another tenant's records.

## Runtime Mode and Configuration

Add required `STRIPE_MODE=test|live` whenever the four existing Stripe values
are configured.

- `test` requires `sk_test_`; `live` requires `sk_live_`.
- A signed event's `livemode` must match the configured mode.
- Missing all Stripe configuration still disables the connector.
- Partial configuration, a missing mode, or a key/mode mismatch fails startup
  and `--check-config` without logging secrets.
- The authenticated customer status response returns `enabled` plus
  `mode: disabled|test|live`. Test mode is visibly labelled "Stripe 测试模式"
  and cannot be presented as a live payment method.
- Platform readiness shows configuration mode separately from processing
  health. It must not perform a charge, refund, or hidden network mutation.

The current production-host sandbox remains valid only after its environment is
updated explicitly with `STRIPE_MODE=test`; this is an intentional deployment
step, not an automatic compatibility guess.

## Data Model

Create the next schema migration after v33.

### `payment_orders`

Add:

- `purpose`: `wallet-topup` or `plan-funding`;
- nullable `price_version_id`, required exactly for `plan-funding`;
- `review_state`: `none`, `needs-review`, or `resolved`;
- `review_revision`: starts at 1 and increments on every review-state or
  reconciliation change;
- `updated_at`.

Keep the existing `state` column as the financial/Checkout lifecycle and change
its allowed values to `pending`, `credited`, `refunded`, `expired`, and
`cancelled`. Existing `needs-review` rows are migrated without losing evidence:
`review_state` becomes `needs-review`, while financial state is derived from
refund ledger, credit ledger, or otherwise pending. New review incidents never
overwrite financial state.

### `payment_incidents`

Allow `state` values `needs-review` and `resolved`, and add nullable:

- `resolved_at`, `resolved_by`, `resolution`, and `resolution_note`;
- `tenant_id`, populated for matched incidents so tenant-level freeze gates can
  be evaluated without guessing through a mutable join;
- `revision`, initialized to 1 and incremented by every resolution transition.

Existing incidents are backfilled from their matched orders. Unmatched
incidents remain visible but cannot freeze or unfreeze an unidentified tenant.

### `billing_wallets`

Add `payment_review_revision bigint NOT NULL DEFAULT 1`. Every transition that
freezes or releases wallet spending increments it. The value is an optimistic
concurrency token for the tenant-level release action and is not a ledger
sequence or balance version.

### `payment_webhook_inbox`

Persist the minimum normalized, already-signature-verified event fields needed
by accounting, not the full Stripe snapshot or customer details. Store event
ID, payload hash, event type, normalized event JSON, configured mode,
processing state, attempts, next attempt, lease token/expiry, last bounded
error code, received/applied timestamps, and a uniqueness constraint on Stripe
event ID.

The immutable `payment_provider_events` table remains the accounting record of
events successfully applied. The inbox is operational state and may update only
through the worker lifecycle.

## Webhook Intake and Worker

The public webhook route continues to validate Host, body size, direct-peer
rate budget, raw signature, Connect account absence, and live/test mode.

After verification it performs only a bounded database enqueue:

- a new event is inserted as queued;
- an identical event ID and payload hash returns 200 without another row;
- the same ID with different bytes returns 409 and creates no accounting
  mutation;
- a durable enqueue failure returns non-2xx so Stripe retries.

The route then returns 200. It does not wait for wallet, incident, or audit
processing.

A control-service worker claims queued/retry rows with a lease, calls the
existing transactionally safe event application, and records applied or a
bounded retry with exponential backoff. Process restarts recover expired
leases. Exhausted rows remain visible to platform administrators and can be
explicitly retried; deleting or silently marking them successful is not
allowed.

The accounting transaction continues to lock provider event identity, order,
wallet, ledger, incident, and platform audit together. Event ordering is never
assumed. Required event handling includes:

- credit: `checkout.session.completed` and
  `checkout.session.async_payment_succeeded`;
- terminal non-credit: `checkout.session.expired` and
  `checkout.session.async_payment_failed`;
- refunds: `charge.refunded`, `refund.created`, `refund.updated`,
  `refund.failed`, and `charge.refund.updated`;
- every `charge.dispute.*` lifecycle event.

Only an exact paid session can credit. Expiration/failure only closes a pending
order after exact session/order metadata matching. Refund/dispute behavior
continues to fail safe into review and wallet freeze.

`checkout.session.expired` changes an exact pending order to `expired`.
`checkout.session.async_payment_failed` changes an exact pending order to
`cancelled` with a bounded internal reason. If any exact paid event arrives for
an order already in `expired` or `cancelled`, the worker creates a review
incident and freezes the matched wallet; it never silently ignores or
automatically credits the late event. This rule is deliberately conservative
because Stripe does not guarantee event delivery order.

Sales eligibility is fixed only for creation of a new `plan-funding` local
order. Once that order commits, idempotent replay and attachment of the same
Stripe Session continue with the saved tenant, purpose, price version and
amount even if the offering or node later stops selling. This preserves
recovery from an uncertain provider call. It still does not promise a package:
after credit, the existing wallet purchase transaction rechecks current sales
gates and may leave the funds as wallet balance.

## Platform Payment Operations

Add a dedicated "支付与对账" platform-admin page and protected APIs using the
existing platform permission, same-origin, strict JSON, rate-limit, transaction,
revision/idempotency, and platform-audit conventions.

The page shows:

- connector `disabled/test/live` mode and whether new Checkout is available;
- webhook queued/retry/exhausted counts and oldest pending age;
- recent payment orders with tenant, purpose/package, amount, financial state,
  review state, and timestamps;
- unresolved and recently resolved incidents, including bounded reason/event
  facts without raw payloads or payment credentials.

Incident resolution requires an operator note and one disposition:

- `keep-frozen`: resolve the selected incident but leave the tenant wallet
  frozen;
- `resume-pending`: only for a pending matched order; allow a future valid paid
  event to credit after all relevant incidents are resolved;
- `cancel-pending`: only for a pending matched order; make it terminal so a
  later completion cannot credit.

Every resolution is protected by the incident's expected revision, rechecks
platform permission in the same transaction, records operator identity,
disposition and note, and writes platform audit atomically. A stale page gets
409. A tenant with any other unresolved incident remains frozen. Later Stripe
events may create a new incident and freeze the wallet again.

Wallet release is a separate tenant-level administrator action, so choosing a
resolution that intentionally keeps a hold can never make the wallet
permanently unmanageable. It requires a bounded operator note, the expected
wallet review revision, and an explicit acknowledgement that Stripe state and
any required wallet adjustment were checked. In one transaction it rechecks
platform permission, locks the wallet, proves that the tenant has no unresolved
matched incidents, sets `spending_frozen=false`, increments the wallet review
revision, and writes platform audit. It does not alter balance or ledger rows.
Freezing on a later incident also increments the wallet review revision, so a
stale release request returns 409.

No resolution action submits dispute evidence or issues a Stripe refund. Those
remain explicit Stripe Dashboard/API operator actions followed by RelaDock
reconciliation.

## Enterprise UI

- The payment method card labels test and live modes truthfully.
- Package Checkout submits only `priceVersionId`; arbitrary top-up continues to
  submit `amountCents`.
- On return, the page refreshes the pending local order and wallet. It shows
  "等待 Stripe 回调", "已到账，可用余额购买套餐", "需要人工对账",
  "支付会话已结束", or "已退款" from server facts.
- Wallet freeze blocks wallet spending and new Checkout as today, but links the
  customer to platform support rather than implying an automatic recovery.
- Logout, tenant change, 401, leaving checkout, and stale async responses keep
  their current generation/isolation protections.

## Security and Privacy

- Never log or return Stripe keys, signing secrets, raw webhook bodies,
  Checkout Session IDs, PaymentIntent IDs, or unbounded provider errors.
- Keep the fixed Stripe API host, disabled environment proxy, redirect refusal,
  HTTPS hosted-URL allowlist, request/body limits, and integer-cent bounds.
- The normalized inbox stores only fields used by the accounting contract.
- Customer and platform queries are paginated and tenant/platform scoped.
- Resolution notes are bounded and treated as operator audit text; they must not
  accept secrets or raw card data.
- A browser redirect, order creation, webhook enqueue, or operator page status
  is never wallet credit. Only the ledger transaction is wallet credit.

## Verification and Acceptance

### Automated

Use strict TDD with real isolated PostgreSQL schemas for:

- mode/key mismatch and secret-safe startup errors;
- server-derived package price, forged amount rejection, changed selection
  idempotency conflict, stopped-sale concurrency, and tenant isolation;
- inbox duplicate/conflicting identity, lease recovery, retry/exhaustion,
  out-of-order credit/refund/dispute, and audit rollback;
- paid, async paid, expired, async failed, partial/full/unfunded refund, every
  dispute prefix, and later duplicate events;
- incident resolution authorization, revision conflict, disposition rules,
  multiple-incident freeze, the separate tenant wallet release gate, re-freeze,
  and immutable ledger;
- enterprise order visibility and admin page/API scoping;
- UI test/live labels, return-state rendering, stale response isolation, and
  no redirect-based credit claim.

Run `npm test`, `npm run vet`, `git diff --check`, and the release build. Record
the real test database, skipped tests, and unverified environment items in
`docs/IMPLEMENTATION_STATUS.md`.

### Stripe sandbox acceptance

Using a dedicated sandbox endpoint and a new disposable enterprise/package:

1. verify runtime mode, endpoint event selection, HTTPS, and signing secret;
2. create Checkout from the package page and complete a Stripe test payment;
3. observe one inbox event, one immutable provider event, one wallet credit,
   and no package purchase until the customer confirms wallet payment;
4. resend the event and prove no second credit;
5. exercise expiry/async-failure with supported Stripe test mechanisms;
6. issue a full refund and a dispute-style test event, verify reversal/review
   and freeze, then exercise the audited operator resolution;
7. restart the service with queued work and prove lease recovery;
8. preserve event IDs, local order IDs, ledger IDs, timestamps, and redacted
   screenshots/logs in a payment acceptance record.

### Live activation gate

Live activation is a later, separately authorized operation. It requires an
activated Stripe merchant, live keys and endpoint secret supplied through the
production secret store, explicit `STRIPE_MODE=live`, approved return URLs,
verified business/legal/refund policy, an on-call reconciliation owner, backup,
rollback, and a capped real transaction/refund/dispute exercise. No credentials
or real charge are created by this design phase.

## Documentation and Release Status

Update `README.md`, `docs/REQUIREMENTS.md`, `docs/ARCHITECTURE.md`,
`docs/ROADMAP.md`, `docs/PAYMENTS.md`, `docs/DEPLOYMENT.md`, and
`docs/IMPLEMENTATION_STATUS.md` as implementation and acceptance facts change.

The repository-wide release manifest remains `productionReady=false` until its
unrelated network, capacity, recovery, and protocol gates are also satisfied.
Stripe may independently reach `code-ready`, `sandbox-accepted`, or
`live-accepted`; do not use one status to imply the others.

## Calls Made in This Design

- Keep wallet funding followed by an explicit wallet package purchase, because
  that is the repository's latest confirmed payment boundary.
- Keep arbitrary top-up as a separate purpose rather than removing it.
- Add a durable webhook inbox because Stripe documents retries, duplicate
  events, non-guaranteed ordering, and quick 2xx responses as production
  integration requirements.
- Require explicit test/live mode instead of inferring production readiness from
  a key prefix.
- Provide audited manual reconciliation but do not automate refunds, dispute
  evidence, balance corrections, or unfreezing.
- Treat live activation as a separate authorization and acceptance operation,
  not as a side effect of merging code.
