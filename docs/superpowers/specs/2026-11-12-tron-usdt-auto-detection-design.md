# TRON USDT Automatic Detection and QR Payment Design

## Intent

Remove transaction-hash submission from the normal customer journey. A customer
creates a TRON USDT order, scans the collection-address QR code, transfers the
full six-decimal amount before the countdown reaches zero, and waits for the
solidified transfer to be discovered and credited automatically. Manual TxID
submission remains a recovery path for missed scans; it is not the primary call
to action.

## Product Decisions

- Automatic discovery scans sequential solidified blocks through the configured
  SolidityNode. It does not trust unconfirmed head data, provider webhooks, an
  explorer display, or a balance delta.
- Only one successful official-mainnet-USDT `Transfer` event to the configured
  collection address in a transaction is a candidate. Contract, recipient,
  amount, block time, transaction hash and log index remain fully verified.
- The candidate amount and original 30-minute transfer window select the pending
  order. The existing unique active six-decimal quote prevents two live orders
  from sharing a candidate amount. The existing confirmation worker re-queries
  the candidate TxID before credit, so discovery never bypasses receipt checks.
- A database cursor leases one solidified block at a time and advances only after
  every candidate in that block has been handled. Restart, retry and multiple
  control processes cannot skip or double-credit a block. First enablement starts
  with a bounded 1,200-block lookback; older missed transfers use manual recovery.
- The QR image is generated locally by the authenticated RelaDock server and
  encodes only the Base58Check TRON collection address. This is the interoperable
  receive-address QR format documented by TronLink. The page separately shows the
  official USDT contract and exact amount. It does not encode a URL, approval,
  signing request, WalletConnect session, or non-standard payment URI.
- The order response includes server time. The browser derives a monotonic
  `MM:SS` countdown from server time, updates once per second, never displays a
  negative duration, disables the QR/new-transfer presentation at zero, and
  refreshes authoritative state. A transfer included inside the original window
  may still be auto-verified or manually recovered afterward.

## Data and Worker Flow

Migration v38 adds one `tron_payment_scan_cursors` row per network/contract/
collection tuple. It stores `next_block`, a short lease, a retry time, a bounded
error code and timestamps. A worker reads the latest solidified height, claims
the next sequential height, fetches all TransactionInfo receipts for that block,
and parses candidate events. A candidate atomically changes a matching order from
`pending` to `confirming`, records its TxID, schedules the existing confirmation
worker, and writes a platform audit record. The cursor advances only after the
block succeeds; provider failures retain the height for retry.

The existing `ApplyTronPaymentReceipt` transaction remains the only path that
creates the immutable receipt, wallet credit, ledger entry and audit record.
Package purchase still runs after that credit commits.

## UI

The invoice places a square QR code beside the exact amount and address. Copy
buttons are provided for both values. Text explicitly tells the customer to scan
the address, select TRON official USDT, and enter the exact displayed amount.
The primary status says automatic detection is waiting for a solidified transfer.
Manual TxID submission moves into an expandable “未自动到账？” recovery section.
When the countdown reaches zero, the QR is hidden and the page tells customers
not to start a new transfer; already-sent customers can keep waiting or use the
recovery section.

## Safety and Limits

- No private key, mnemonic, signing, sweep, refund or outbound chain transaction.
- No credit from unfinalized blocks, failed receipts, wrong contracts, wrong
  recipients, wrong amounts, ambiguous transfers or out-of-window block times.
- Cursor retries are idempotent; immutable receipt uniqueness remains the final
  global transaction replay barrier.
- QR scanning fills the address only. Cross-wallet automatic token-and-amount
  prefill is explicitly out of scope because there is no common safe TRON payment
  URI accepted by the supported wallets. WalletConnect/signing is a separate
  product and requires an explicit approval design.
- Production release requires a database backup, v38 migration verification,
  SolidityNode scan-method connectivity, external UI checks and a controlled
  small-value real transfer before general payment readiness.
