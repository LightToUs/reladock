# Direct Plan Purchase Design

## Intent

Make plan purchase the primary customer flow. A customer selects a plan and
confirms one purchase; RelaDock uses available wallet balance first and, when
that balance is insufficient, asks Stripe only for the remaining amount. A
verified Stripe credit and the plan debit are then attempted in one PostgreSQL
transaction so the browser never has to perform a second "use wallet" action.

This design supersedes the older UI boundary that treated Stripe plan funding
as a wallet top-up followed by an explicit second plan-purchase click. Arbitrary
wallet top-up remains available as a separate enterprise prepayment action.

## Goals

- Present one customer action: buy the selected immutable plan version.
- Use available wallet balance before external payment.
- Derive the external charge on the server from the selected price version and
  current wallet balance; the browser cannot choose the amount.
- After a verified Stripe callback, credit the wallet and purchase the selected
  plan in the same database transaction when all current sales, authorization,
  wallet, and entitlement gates still pass.
- Keep checkout, callback, wallet credit, and plan purchase idempotent under
  retries, duplicate events, and concurrent requests.
- Preserve truthful failure states: payment return URLs never prove payment or
  plan activation, and a paid plan is not evidence of Agent application or
  end-to-end health.

## Non-goals

- No Stripe Billing subscription, saved-card flow, external-card auto-renewal,
  installment payment, invoice credit, or additional currency.
- No reservation or hold against the customer's existing wallet balance while
  Stripe Checkout is open.
- No automatic refund when a paid callback can no longer purchase the plan.
- No automatic retry of a failed post-credit plan purchase.
- No change to arbitrary wallet top-up, refund/dispute review, wallet freeze,
  rule delivery, Agent, or data-plane semantics.
- No real payment, production deployment, or merchant activation in this task.

## Customer Flow

1. The customer selects a published plan and confirms purchase.
2. The checkout page shows plan price, current wallet balance, the wallet amount
   that will be used, and the external amount still required.
3. If the wallet covers the full price, the existing atomic wallet purchase is
   used immediately; Stripe is not contacted.
4. If the wallet is short and Stripe is enabled, the browser submits only the
   selected `priceVersionId` and optional `ruleId`. The server rechecks the
   authenticated tenant, plan, line, node, and wallet, then calculates the
   Stripe amount.
5. The server commits a tenant-scoped local `plan-purchase` payment order before
   contacting Stripe. Retries of the same idempotency key and selection recover
   the same order and Stripe session.
6. Stripe redirects only return the browser to RelaDock. The browser reads the
   local order state; it never infers payment or activation from URL parameters.
7. A verified paid webhook credits the wallet and attempts the stored plan
   purchase. On success the response state becomes `purchased`; on a business
   gate failure the external funds remain wallet balance and the order records a
   stable purchase-failure reason.

## External Amount

For a plan price `P` and current wallet balance `B`, the shortfall is
`max(P - B, 0)`. Stripe's existing USD minimum is 50 cents, so the external
amount is `max(shortfall, 50)` whenever external payment is needed. If the
shortfall is less than 50 cents, the excess remains ordinary wallet balance
after the plan debit. The UI must show that remainder before redirecting.

The amount is a checkout-time quote, not a reservation. Other authorized wallet
spending may change `B` before the webhook. The webhook always rechecks the
actual wallet balance and never makes it negative.

## Persistence

Add a new migration after the current schema version. Extend `payment_orders`
without rewriting historical orders:

- `purpose`: `wallet-topup` for existing/arbitrary orders or `plan-purchase`;
- optional `rule_id` scoped to the same tenant;
- optional immutable `price_version_id`;
- `purchase_state`: `none`, `pending`, `purchased`, or `failed`;
- `purchase_error`: empty unless `purchase_state='failed'`;
- optional `subscription_id` scoped to the same tenant and present only for a
  purchased plan order.

At most one `pending` plan-purchase order may exist for the same tenant,
optional rule (including the unbound case), and price version. A partial unique
index enforces this selection-level guard. A concurrent request with a different
browser idempotency key recovers that same pending order instead of creating a
second Stripe Session. Terminal paid, failed, expired, refunded, or review
results do not permanently prevent a later legitimate purchase; wallet and
entitlement gates still apply.

Existing rows backfill as `wallet-topup` plus `purchase_state='none'`. Database
constraints enforce valid field combinations. No mutable price, quota,
bandwidth, period, tenant, or arbitrary JSON snapshot is stored on the order;
the immutable price-version foreign key is the source of the selected product.

`PaymentOrder` may return the non-sensitive purpose, price-version ID,
purchase state, purchase error code, and subscription ID. It continues to hide
tenant ID, Stripe session ID, PaymentIntent, provider credentials, and raw
events.

## Server Interfaces

- Keep `POST /api/payments/checkout` for arbitrary wallet top-up with a
  server-bounded integer `amountCents`.
- Add `POST /api/payments/plan-checkout` with strict JSON
  `{ruleId, priceVersionId}` and the existing `Idempotency-Key` contract.
  `ruleId` may be empty for the current buy-before-rule flow.
- Add authenticated tenant-scoped `GET /api/payments/orders/:id` so the browser
  can read the local result after a Stripe return without recreating or
  redirecting to the Checkout Session.
- Add authenticated tenant-scoped `GET /api/payments/orders?purpose=plan-purchase`
  returning a bounded recent list. This is the recovery path when session
  storage is missing, a payment completed in another tab/device, or the fixed
  Stripe return URL does not contain a local order ID.
- Keep `POST /api/subscriptions/purchase` for wallet-only purchase and existing
  API clients.

The plan-checkout endpoint derives the amount and rejects forged tenants,
foreign rules/plans, unavailable sales, frozen wallets, sufficient-wallet
requests that should use the wallet path, changed idempotent selections, and
malformed input before a provider request.

## Transaction and Locking

Extract the current `PurchasePlan` transaction body into reusable transaction
helpers while preserving its observable API. Plan preparation retains the
existing lock order: tenant/member authorization, idempotency, node/rule,
line/plan, subscription state, then wallet. The webhook must not lock the wallet
and then wait for a node lock, because that would deadlock against manual
purchase or auto-renewal.

For a paid `plan-purchase` order, webhook handling:

1. holds the existing global provider-event advisory lock and exact order row;
2. prepares and locks the stored plan purchase using the original member and a
   deterministic purchase idempotency key derived from the order ID;
3. locks/creates the wallet;
4. appends the positive `payment-credit` entry;
5. when preparation and current wallet gates pass, appends the negative
   `subscription-debit`, creates the subscription, and marks the order
   `purchased` in the same transaction;
6. commits the immutable provider event, both ledger entries, subscription,
   audits, order state, and any configuration Outbox together.

If a known business gate prevents purchase, the transaction still commits the
verified credit and records `purchase_state='failed'` with a bounded enum-like
reason (`unavailable`, `insufficient-balance`, `wallet-frozen`, `unauthorized`,
or `conflict`). Unknown database, audit, or storage errors roll the whole event
back and return non-2xx so Stripe can retry; they are never converted into a
successful wallet credit with a vague failure.

## Idempotency and Concurrency

- A local order is unique by `(tenant_id, idempotency_key)`.
- Reuse with a changed purpose, rule, or price version returns conflict.
- Stripe continues to use the local order ID as its provider idempotency key.
- Provider event ID/hash replay rules remain unchanged.
- The deterministic subscription purchase key is stable for the order, so a
  distinct duplicate paid event cannot create a second subscription.
- A second idempotency key for the same selection recovers the existing pending
  order. After a verified `checkout.session.expired` or
  `checkout.session.async_payment_failed` event, that order is terminal and a
  new key may create a replacement checkout.
- Concurrent plan checkout, manual wallet purchase, auto-renewal, wallet
  adjustment, refund, and dispute paths retain non-negative integer balances and
  existing lock ordering.

## Failure and Refund Behavior

- If the line or node becomes unavailable, the member loses write permission,
  the tenant is disabled, the wallet becomes frozen, an active entitlement now
  conflicts, or concurrent spending leaves insufficient balance, no plan is
  opened. The verified external amount remains in the wallet and the order
  exposes the bounded failure reason.
- The UI tells the customer that funds are available in the wallet and that a
  new plan purchase can be attempted after the underlying issue is resolved.
- Existing full-refund logic is unchanged. If credited funds have already been
  consumed by a plan and the wallet cannot fund the refund debit, the existing
  review incident and spending freeze apply. RelaDock does not silently revoke
  a subscription or make the wallet negative.
- Disputes continue to create review incidents and freeze spending; later
  events never automatically unfreeze a wallet.
- `checkout.session.expired` and `checkout.session.async_payment_failed` do not
  credit the wallet. They mark the exact pending order terminal with
  `payment-expired` or `payment-failed`, release the selection-level duplicate
  guard, and remain visible in recent-order history. A browser cancel alone is
  not proof of terminal provider state; the order stays pending until Stripe
  reports a terminal event.

## Frontend

- Keep the independent wallet top-up form in the overview for enterprise
  prepayment.
- Make the package checkout copy and primary action describe direct purchase,
  not "recharge and come back".
- When balance is sufficient, show one wallet purchase action and the remaining
  balance.
- When balance is insufficient, show wallet contribution, Stripe charge,
  minimum-charge excess if any, and one action such as "支付差额并购买套餐".
- Persist only the tenant-scoped idempotency key, local order ID, and selected
  non-sensitive plan/rule details in session storage. Logout, tenant change,
  401, leaving the purchase flow, and stale asynchronous responses retain the
  existing isolation gates.
- After redirect/refresh, read the local order. Show waiting, purchased,
  needs-review, or credited-but-plan-failed truthfully. Never say the plan is
  active before the local subscription exists.
- If the exact browser entry is unavailable, reconcile the bounded recent
  plan-order list with the current plan catalog and purchased subscriptions.
  This supports truthful cross-tab/device recovery without putting a tenant or
  order identifier into Stripe return URLs.

## Verification

Use real isolated PostgreSQL schemas and provider substitutes. Cover:

- exact server-derived shortfall, the 50-cent minimum, and excess remainder;
- sufficient wallet avoiding Stripe;
- changed/foreign selections and frozen-wallet rejection before provider calls;
- credit plus purchase in one transaction and exact two-entry ledger balances;
- duplicate/concurrent callbacks producing one credit and one subscription;
- changed sales status, revoked member/tenant, concurrent wallet spending, and
  audit/storage rollback behavior;
- ordinary top-up and existing refund/dispute regressions;
- tenant-scoped order reads and strict HTTP input;
- frontend amount/copy/state handling, tenant/logout/stale-response isolation;
- migration upgrade/reapply constraints and historical order preservation.

Run focused red/green tests, then `npm test`, `npm run vet`, and
`git diff --check`. Update README, requirements, architecture, roadmap,
payments documentation, and implementation status. Report that real Stripe,
production deployment, and merchant acceptance remain unverified.
