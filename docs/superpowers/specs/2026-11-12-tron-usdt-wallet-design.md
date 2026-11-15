# TRON USDT Wallet Funding Design

## Intent

Add an optional, default-disabled TRON mainnet USDT funding channel to RelaDock.
Customers may fund their enterprise USD wallet or fund a specific immutable plan
price for an optional owned rule. Confirmed funds always enter the existing USD
wallet ledger first; a plan funding order may then attempt the existing wallet
purchase transaction. The browser, a submitted transaction hash, an RPC response
from a non-solidified node, and a block-explorer page are never sufficient proof
of wallet credit.

The configured collection address is
`TU4x4apSvfu1NLx6ee11rM5PjtVCBbWssw`. Its Base58Check checksum and TRON mainnet
`0x41` payload prefix were locally validated on 2026-11-12. The address is public
configuration, not a credential. RelaDock must never request, store, log, or use
its private key or mnemonic and must never originate a chain transaction.

## Product Decisions

- v1 supports only TRON mainnet and the official TRC-20 USD₮ contract
  `TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t`.
- The enterprise wallet remains denominated in USD integer cents. v1 applies the
  explicit commercial policy `1 USDT = 1 USD`. The server allocates each active
  order an exact six-decimal quote of `cents * 10,000 + suffix` USDT base units,
  where the globally unique suffix is `0..9,999`. The suffix is an anti-collision
  verification tail smaller than one USD cent; only the integer-cent portion is
  credited to the wallet. The UI must disclose and copy the full exact amount.
  Allocation and conversion use checked integer arithmetic only.
- v1 uses the single configured collection address and requires the customer to
  submit the transaction hash for an order. It does not derive deposit addresses,
  retain deposit private keys, sweep funds, estimate Energy, or send refunds.
  Unique per-order addresses require a separately approved key-custody design.
- An arbitrary wallet top-up accepts a server-bounded `amountCents`. A plan
  funding order accepts `priceVersionId` and optional `ruleId`; the server loads
  the immutable price and ignores browser-supplied amounts.
- An active quote reserves its exact base-unit amount globally until the claim
  grace ends. Unverified transaction hashes are not unique reservations: only an
  accepted immutable receipt owns a transaction hash globally, preventing one
  tenant from blocking another tenant's public transaction before verification.
- A plan funding order credits the wallet first. It then makes one idempotent
  attempt to call the existing `PurchasePlan` path. If authorization, sales,
  node, rule, balance, or other gates reject the purchase, the truthful wallet
  credit remains and the order reports that the customer must purchase from the
  balance manually. Funding never fabricates a subscription.
- Orders expire for automatic matching 30 minutes after creation. A solidified
  transfer whose block timestamp is outside the order window is held for review;
  it is not silently discarded or automatically credited.
- The channel is not production-ready until the operator has separately accepted
  the collection-address custody, RPC availability, address-risk review, legal,
  tax, reconciliation, and incident-response procedures.

## Customer Flow

1. An authenticated owner/admin creates either a wallet top-up order or plan
   funding order with an `Idempotency-Key`.
2. The API returns the local order ID, TRON network, official token contract,
   collection address, exact USDT decimal display amount, integer USD cents,
   expiry, and `pending` state. It returns no key material.
3. The customer sends the exact amount through TRON and submits the 64-hex
   transaction hash to the order.
4. The order becomes `confirming`. A worker queries the configured HTTPS TRON
   SolidityNode endpoint and retries temporary/not-yet-solidified results.
5. The verifier requires a solidified receipt with successful contract execution
   and exactly one matching official-USDT `Transfer` log to the collection
   address for the order's exact base-unit amount. It records the sender, block,
   transaction hash, event index and observed block timestamp.
6. The store transaction locks the order and wallet, inserts a unique immutable
   chain receipt, appends one positive `payment-credit` ledger entry, updates the
   wallet balance, marks the order `credited`, and writes platform audit. Duplicate
   workers and duplicate customer submissions cannot credit twice.
7. For plan funding, the worker calls `PurchasePlan` with an order-derived
   idempotency key. Success records the subscription ID; failure records a bounded
   purchase error while leaving the wallet credit spendable unless another
   existing review freeze applies.

## State Model

Financial states are `pending`, `confirming`, `credited`, `needs-review`, and
`expired`. Purchase states are `not-requested`, `pending`, `purchased`, and
`failed` and do not overwrite the financial state.

- A malformed hash is rejected before state mutation.
- Re-submitting the same hash for the same order is an idempotent replay.
- A different hash after a claim is a conflict and requires operator review.
- A transaction hash may belong to only one order.
- A missing/non-solidified receipt remains `confirming` until the retry horizon.
- RPC/network/JSON failures retain retryable state and a bounded non-secret error.
- Wrong contract, failed execution, wrong recipient, wrong amount, ambiguous
  matching logs, or outside-window block time produce `needs-review` without a
  wallet mutation.
- No process changes `needs-review` to credited automatically. A future operator
  reconciliation workflow must append facts; it must not edit ledger history.

## TRON Verification Boundary

The client calls only the configured HTTPS base URL plus
`/walletsolidity/gettransactioninfobyid`, with redirects and environment proxies
disabled, a bounded response body, optional server-only `TRON_API_KEY`, and a
short timeout. Configuration is all-or-nothing and invalid partial configuration
fails startup and `--check-config` without logging secrets.

The verifier does not trust token symbol/name. It compares the log contract bytes
to the decoded official USDT contract address, requires the canonical TRC-20
`Transfer(address,address,uint256)` topic, reads the indexed destination from the
last 20 bytes of topic 2, and parses the 32-byte data amount as an unsigned integer
that must fit the stored `bigint` base-unit amount.

## Data Model

Migration v36 adds (v35 is already reserved by the in-progress Stripe
production-payment work present in this checkout):

- `tron_payment_orders`: tenant/member ownership, tenant-scoped idempotency,
  purpose, optional rule/price references, USD cents, USDT base units, collection
  address, state, claimed tx hash, timestamps, bounded last error, ledger and
  optional subscription references.
- `tron_payment_receipts`: one immutable receipt per transaction hash, including
  order, sender, recipient, contract, amount, block number/time and log index.
  Mutation and truncation are rejected by the existing immutable-ledger trigger.
- indexes for tenant order listing and bounded worker claims.

The existing `billing_ledger.kind='payment-credit'` remains the accounting entry;
the ledger key namespace is `tron-payment:<order-id>`. No USDT balance is added to
`billing_wallets`.

## API and Authorization

- `GET /api/payments/status` reports Stripe and TRON availability separately while
  retaining the aggregate `enabled` flag for existing clients.
- `POST /api/payments/tron/orders` creates/replays an order. Exactly one of
  `amountCents` or `priceVersionId` is accepted; `ruleId` is only valid with a
  plan price. Current tenant write authorization is rechecked in the transaction.
- `POST /api/payments/tron/orders/:id/claim` accepts only `{ "txHash": "..." }`
  and rechecks tenant write authorization.
- `GET /api/payments/tron/orders/:id` and `GET /api/payments/tron/orders` expose
  only the authenticated tenant's bounded, non-secret order state.
- API keys retain their existing read/write scope behavior. Cross-tenant IDs
  return the existing not-found boundary.

## User Interface

The existing checkout page gains `USDT / TRON` beside wallet and Stripe. It shows
the exact network, official token, collection address, amount, expiry, copyable
values, transaction-hash submission and truthful pending/confirming/review/credited
status. It never labels a submitted hash, browser return, or detected transaction
as credited before the server reports `credited`. When plan auto-purchase fails,
it instructs the customer to use the wallet balance.

## Non-goals

- Private-key generation, mnemonic custody, HD derivation, hot-wallet signing,
  automatic sweep, TRX/Energy management, or on-chain refunds.
- Multiple TRON tokens, multiple USDT networks, exchange deposits or bridges.
- Dynamic FX/oracle conversion, accepting partial/multiple payments, overpayment
  auto-credit, or arbitrary transaction memo matching.
- Automatic risk clearance, Travel Rule exchange, or a claim of legal/licensing
  readiness.
- Production configuration, real transfers, balance inspection, or deployment.

## Verification Boundary

Tests use an isolated PostgreSQL schema and an HTTP substitute for SolidityNode.
They cover address/checksum validation, exact official-contract log parsing,
solidified success, not-found retry, wrong contract/recipient/amount/time,
ambiguous logs, tenant isolation, order and transaction idempotency, concurrent
credit, audit rollback, wallet/ledger invariants, plan-purchase fallback, strict
HTTP input, disabled/partial configuration, and UI truthfulness. No test contacts
TRON mainnet or moves funds. Production acceptance remains separately required.
