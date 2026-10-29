# Optional Stripe payment connector

Status: optional integration verified with deterministic provider substitutes and
real local PostgreSQL tests. No Stripe merchant account, live payment, production
domain, public webhook, or deployment has been activated. Configuration enables
the connector; it does not prove merchant onboarding or payment acceptance.
Browser success/cancel redirects never credit a wallet.

## Parent integration contract

The payment implementation owns only `internal/payments/*`,
`internal/platform/payments.go`, `payments_test.go`, `migration9.sql`,
`migration12.sql`, `internal/web/payments.go`, `payments_http_test.go`, and this document. The parent
owns migration registration, server/options/main changes, status/UI integration.

1. Migration 9 is registered in the locked full migration sequence, followed by
   period-attribution migration 10 and entitlement-snapshot migration 11. SQL is also exported as
   `platform.PaymentMigrationSQL` for isolated connector tests.
   Migration 9 preserves immutable ledger triggers and the exact v7
   `local-adjustment` and `subscription-debit` constraints, adding positive
   `payment-credit` and negative `payment-refund` entries with no rule/price.
   Also register `platform.PaymentFreezeMigrationSQL` as migration 12, after the
   existing sequence. It adds `billing_wallets.spending_frozen boolean NOT NULL
   DEFAULT false` and backfills holds for existing matched review incidents or
   review orders, including zero-balance wallets not previously initialized.
   Reapplying it preserves holds; it never clears a freeze.
2. Read `payments.ConfigFromEnv() (payments.Config, bool, error)`. All four
   variables absent means disabled. Partial/invalid configuration fails closed.
   Never log the returned config. When enabled, construct
   `payments.NewStripe(config) (*payments.Stripe, error)`, then
   `web.NewPayments(store, provider, config.WebhookSecret, cfg.Origin)`.
3. The exported web type is `*web.Payments`; its methods are
   `Enabled() bool`, `Checkout(http.ResponseWriter, *http.Request, platform.Identity)`
   and `Webhook(http.ResponseWriter, *http.Request)`. `Enabled` is nil-safe.
   `NewPayments(store, provider, secret, origin ...string)` retains the three
   argument form; a configured origin is recommended and required behind TLS
   termination. No additional mode argument is needed: `*payments.Stripe`
   provides `LiveMode() bool` from its secret-key prefix. Other injected providers
   default to test mode, or can implement `LiveMode() bool`.
4. Dispatch `POST /api/payments/stripe/webhook` immediately after strict Host
   validation, before browser CSRF/session middleware. Leave the raw body intact.
   `Webhook` owns its independent 256 KiB body bound and direct-peer-IP budget:
   120 requests/minute/IP, 10,000 retained IP windows, 429 + Retry-After on
   exhaustion. Do not also charge the shared browser API budget. Forwarded IP
   headers are ignored. This is process-local protection; any future proxy must
   have its own appropriate limits.
5. Dispatch `POST /api/payments/checkout` after existing authentication, configured
   Host/origin checks, and API rate limiting. Pass the authenticated identity and
   original context (including `platform.WithAPIKey` for bearer authentication).
   Checkout independently requires exact Origin, matching Host,
   `X-Requested-With: RelaDock`, and a non-cross-site fetch; it rechecks current
   active tenant/member/key write permission inside both database transactions.
6. An authenticated `GET /api/payments/status` may return
   `{"enabled": payments.Enabled()}`. This means configured, not merchant activated.
   Parent UI should retain truthful pending/needs-review status and never infer
   wallet credit from the redirect or the checkout response.

Environment variables (no defaults, no supplied credentials):

| Variable | Requirement |
| --- | --- |
| `STRIPE_SECRET_KEY` | Server-only `sk_test_` or `sk_live_` credential |
| `STRIPE_WEBHOOK_SECRET` | Matching endpoint `whsec_` signing secret |
| `STRIPE_SUCCESS_URL` | Server-configured absolute HTTPS return URL |
| `STRIPE_CANCEL_URL` | Server-configured absolute HTTPS cancel URL |

Test and live webhooks must use their matching endpoint secret. Signed events
whose `livemode` differs from the provider key are rejected with 400. Connect
events with nonempty `event.account` are rejected with 400; this connector has no
Connect support. The server never accepts a Stripe API host or redirect URL from
the customer. Keys are not returned to the browser and no publishable key is used.

## Checkout and order behavior

Request: JSON `{"amountCents":500}` with `Idempotency-Key` matching the existing
8–128 character pattern `[a-zA-Z0-9._:-]`. Unknown fields, query parameters,
fractional values, zero/negative values, and out-of-range values are rejected.
USD is fixed. The connector's safety bound is 50–1,000,000 integer cents
($0.50–$10,000); this is an implementation limit, not a confirmed commercial price.

Response: 201 with `id`, `amountCents`, `currency: "USD"`, `state`, `createdAt`,
and (while pending) `checkoutUrl`. It excludes tenant ID, Stripe session ID,
credentials and wallet credit claims. Other errors include 400 invalid request,
401/403 authentication/authorization, 409 changed idempotent request or uncertain
old order, and 503 disabled/provider unavailable. Keep the same request key on
uncertain responses. The parent UI may navigate to the validated hosted URL.

The local order is committed before contacting the provider. Only the server
creates sessions, with one card-payment line item at the exact USD amount.
Session and PaymentIntent metadata contain `order_id`, `tenant_id`,
`amount_cents`, and `currency: usd`. The provider abstraction is:

```go
type Provider interface {
    CreateCheckout(context.Context, payments.CheckoutRequest) (payments.Session, error)
}
```

The request contains the server-generated OrderID, authenticated TenantID and
AmountCents. Session responses must agree with all four metadata fields, amount,
currency and a `https://checkout.stripe.com` URL before attachment to the order.
Outbound requests go only to `https://api.stripe.com/v1/checkout/sessions`, use
Basic authentication and API version `2024-06-20`, disable redirects and environment
proxies, and have a 12-second deadline plus response-header timeout and response
size bound. The production constructor exposes no alternate host/client option;
transport substitutes are internal to provider tests. Provider error bodies are
never surfaced or logged by this connector.

The Stripe idempotency key is always `reladock-checkout-<local-order-id>`.
Concurrent retries attach the same session; saved sessions are returned without
another provider request. If no session was saved and the order is older than
23 hours, retry returns 409 and requires reconciliation: Stripe can discard keys
after 24 hours. Do not blindly create a new order after an uncertain payment.
Checkout currently has no automated expiration/cancellation reconciliation job.

## Webhook accounting

Configure own-account **snapshot** events, with non-expanded PaymentIntent IDs.
Supported credit events are `checkout.session.completed` and
`checkout.session.async_payment_succeeded`. Credit requires the session object
to be `checkout.session`, mode `payment`, status `complete`, payment status `paid`,
matching local session/order/tenant/amount/currency metadata, exact USD total,
and a nonempty previously unused PaymentIntent ID. Unpaid/mismatched completion
events create a durable review incident and do not credit automatically.

The signature is HMAC-SHA256 over `<timestamp>.<raw body>` using the endpoint
secret, with constant-time equality, support for multiple v1 signatures, and a
five-minute past/future replay window. JSON decoding occurs only after signature
verification. Timestamp changes on Stripe redelivery are accepted; provider event
identity is the event ID, with a unique stored SHA256 of the original payload.
An identical replay is a no-op; different bytes under the same ID return 409.
Even different event IDs cannot credit/refund the same order twice.

An advisory transaction lock serializes Stripe event handling, then the order and
wallet rows are locked. Wallet update, append-only ledger entry, order state,
provider event and platform audit are one transaction. Audit/storage failures
roll everything back and return non-2xx for Stripe retry. A completion received
before session attachment returns 409 without committing the event, allowing
redelivery after attachment. The ledger uses the same wallet lock as local
adjustments and subscription debits. Overflow/insufficient balances never wrap
or become negative.

## Refunds and review

Subscribe to `charge.refunded`, `refund.created`, `refund.updated`,
`refund.failed`, and legacy `charge.refund.updated` when available. Only a
`charge.refunded` event proving the full exact USD charge was refunded, with the
already credited order's PaymentIntent and enough wallet balance, appends a
`payment-refund` debit and marks the order `refunded`. This connector observes
refunds; it does not initiate refunds at Stripe.

All `charge.dispute.*` events explicitly create review incidents, including
created, updated, closed, funds-withdrawn, funds-reinstated, and future lifecycle
types. Matching uses the stored PaymentIntent or local order metadata. Disputed
amounts can be partial, so they never trigger a wallet debit: every matched
wallet is frozen regardless of the reported amount or dispute outcome. If intent
and order metadata identify different orders, all identified tenants are frozen
and receive review records. Unmatched disputes remain durable incidents; no
unidentified tenant is guessed. A later matched completion encounters the held
intent and enters review. Nonempty Connect accounts and live/test mismatches
are rejected by the signed HTTP handler before any wallet changes.

Partial, pending/failed, unmatched, early, inconsistent, or unfunded refunds
persist `payment_incidents` with `state='needs-review'`, and mark a matched order
`needs-review`. They return 200 only after commit, without making the wallet
negative. Review is deduplicated per order (or per unmatched event), and audit is
written only when an incident is first inserted. Disputes additionally retain
one explicit incident/audit per matched order and distinct provider event ID,
even if a refund incident already exists; identical event retries add nothing.
An unmatched refund's
PaymentIntent is retained so a later completion cannot credit over that refund.
Already refunded orders cannot be credited again. A later distinct, authoritative
full `charge.refunded` event can reverse a previously credited review order if
funds are then available; it cannot credit an early-refund order. Historical
incidents remain durable for operator reconciliation. Replaying the same event
does not retry a debit after replenishment. There is no automatic incident
resolution or partial-refund policy.

Every matched `paymentReview` holds the order row, creates/locks its wallet row
with `FOR UPDATE`, and sets `spending_frozen=true` in the same transaction as
event identity, incident, order state and audit. Partial refunds and unfunded
full refunds leave the ledger/balance unchanged but block wallet spending.
Audit failure rolls back the freeze and event together. A later full refund,
new credit, dispute win/closure, or reinstatement never unfreezes the wallet.
Only explicit authorized reconciliation may release a hold; this connector
implements no release API or automatic release worker.

The parent must reject every normal spending operation while the locked wallet
has `spending_frozen=true` (including `PurchasePlan`, returning `ErrForbidden`),
and expose `Wallet.SpendingFrozen` / JSON `spendingFrozen` to explain the hold.
Those billing/subscription/UI changes are parent-owned. The optional exported
helper is:

```go
func CheckWalletSpendingAllowed(ctx context.Context, tx pgx.Tx, tenantID string) (bool, error)
```

It locks an existing wallet until the caller commits/rolls back. Call it after
authorization and wallet creation and before a debit, using the same transaction.
`(false, nil)` means frozen; any error must also prevent spending. Missing wallets
return `ErrNotFound`; malformed tenant IDs return `ErrInvalid`. Callers already
reading `spending_frozen` under their wallet `FOR UPDATE` need not call it again.

Review incidents are durable database records; there is not yet a dedicated
payment-review UI, external alert dispatcher, or reconciliation worker. Before
merchant activation, operations must establish review monitoring, reconciliation,
and authorized resolution procedures. Do not resolve by editing immutable ledger
entries or silently dismissing incidents.

## Verification and official references

Tests use local isolated PostgreSQL schemas and signed synthetic events, never
Stripe network calls or actual payments. Coverage includes replay/concurrency,
exact amount/currency/session/tenant checks, wallet competition with refunds,
rollback on audit failure, partial/insufficient/early/unmatched refunds, disabled
configuration, fixed outbound request parameters, signature corruption/rotation/
timestamp checks, HTTP CSRF/authentication/body limits, event mode and Connect
rejection, independent webhook rate limits, and credential-safe provider errors.
Review regressions also cover persistent partial/unfunded-refund holds, all
dispute lifecycle prefixes, same/conflicting event identity, explicit dispute
incidents following refunds, ambiguous-tenant holds, audit rollback, migration
hold backfill, and refusal to automatically unfreeze after later resolution.

Run `npm test` and `npm run vet` with a local test database, never production.
No test success is evidence of an activated merchant or real Stripe settlement.

### 2026-10-28 verification snapshot during parallel integration

`npm run vet` passes. Payment-specific tests pass with `-race`, real isolated
PostgreSQL schemas and provider substitutes, including the parent's integrated
HTTP routes. The command is
`node scripts/start.mjs test -race ./internal/payments ./internal/platform ./internal/web -run 'TestPayment|TestSignature|TestStripe|TestConfig' -count=1`.

The parent registered period-attribution migration 10 and the latest snapshot
backfill as migration 11. Final complete-workspace checks are recorded in the
production-readiness plan; connector-specific tests alone do not certify other
features or merchant activation.

Implementation references are Stripe's official
[Checkout session creation](https://docs.stripe.com/api/checkout/sessions/create),
[manual webhook verification](https://docs.stripe.com/webhooks#verify-manually),
[idempotency](https://docs.stripe.com/api/idempotent_requests),
[event types](https://docs.stripe.com/api/events/types), and
[charge/refund fields](https://docs.stripe.com/api/charges/object), plus the
[dispute object](https://docs.stripe.com/api/disputes/object).
