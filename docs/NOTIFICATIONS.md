# Optional operator incident notifications

This connector sends metadata-only incident transitions to one explicitly
configured generic HTTPS webhook. No endpoint is configured by default and no
external delivery has been activated. Tests use local `httptest` TLS servers
and isolated PostgreSQL schemas only. A successful webhook ACK means its server
accepted the event, not that a person read it or that forwarding is healthy.

## Parent integration

Register `platform.NotificationMigrationSQL` from `migration15.sql` as migration
15 in the existing locked migration transaction. The parent owns store migration
registration, command/worker wiring, environment loading and status UI. This
implementation does not change Agent, runtime, delivery or store migration code.

Construct `notify.New(notify.Config{URL: operatorURL, Secret: operatorSecret})`.
It returns `(*notify.Sender, error)`; empty URL and secret produce a disabled
sender, while partial/invalid configuration returns an error. The secret must
contain at least 32 bytes and is excluded from JSON. Never log the full config.
The parent may use `NOTIFY_WEBHOOK_URL` and `NOTIFY_WEBHOOK_SECRET` environment
variables; this package does not read or supply credentials itself.

Worker API:

```go
store.EnqueueIncidentNotifications(ctx, limit) (int, error)
store.ClaimNotification(ctx) (*platform.Notification, error)
store.ReportNotification(ctx, eventID, leaseToken string, delivered bool, errorCode string) error
sender.Enabled() bool
sender.Send(ctx, notify.Event) error
sender.Close()
```

`EnqueueIncidentNotifications` accepts limits 1–1000 and repairs missing CURRENT
snapshots only. It is not the reliability mechanism. `ClaimNotification` returns
`nil, nil` when nothing is ready. `Notification` contains `Event`, `LeaseToken`,
`LeaseUntil` and `Attempts`. Send only `job.Event`; tokens are excluded from JSON.

When `sender.Enabled()` is false, do not claim events and do not make network
requests. Incident transitions still persist, retaining their backlog. When
enabled, claim one event, call `sender.Send(ctx, job.Event)`, then report
`delivered=true, errorCode=""` only if Send returns nil. On any Send error use
`delivered=false, errorCode="delivery-failed"`. Report failures must not be
treated as success; lease expiry permits a later process to retry the same ID.
Bound each worker iteration and stop gracefully on cancellation. Send has its
own ten-second timeout, leaving room inside the thirty-second database lease.

The Store methods are private operator-worker services, not tenant HTTP APIs.
Neither an API caller nor a notification event can supply a URL or secret.

## Durable event capture

Migration 15 installs an `AFTER INSERT OR UPDATE` trigger on
`operations_incidents`. It captures state/occurrences changes in the SAME
transaction as the incident and its audit, including short open→resolved changes
between worker polls and even two changes in one transaction. Outbox failure
rolls back the incident transaction; audit failure also rolls back the outbox.
Future probe incident writers using this table are covered automatically, without
adding notification calls to each writer. Writers must increment `occurrences`
when reopening the same incident.

The database unique key is `(incident_id, occurrences, incident_state)`. Repeated
`observed_at` updates do not create events. Migration backfills the current state
of existing incidents, without inventing unavailable historical transitions.
Once queued, the event identity, payload and timestamps cannot be edited by an
UPDATE. Delivery retry fields remain mutable.

Event JSON contains exactly:

```json
{
  "eventId": "incident:<incident UUID>:<occurrences>:<open|resolved>",
  "incidentId": "<incident UUID>",
  "nodeId": "hk-01",
  "kind": "contact-stale",
  "state": "open",
  "occurrences": 1,
  "occurredAt": "2026-10-29T00:00:00Z"
}
```

For open events the occurrence timestamp is `opened_at`; resolved events use
`resolved_at`. It is not the continuously changing observation timestamp. There
are no customer names, email addresses, tenant/rule lists, targets, payloads,
support tickets/messages, credentials or arbitrary error text in an event.

## Delivery and receiver contract

This is an **at-least-once delivery attempt protocol**, not exactly once. An
accepted request whose response/report is lost can be resent. The receiver MUST
deduplicate `eventId` durably before causing external side effects and return
2xx for already accepted duplicates. Automatic attempts are bounded; exhausted
events are not a claim of eventual successful delivery.

Each lease lasts 30 seconds. Claims use row locks with `SKIP LOCKED` and issue
fresh 43-character random lease tokens. Expired workers cannot acknowledge a new
lease. Reports are idempotent for the same token/result; conflicting/stale reports
return `platform.ErrConflict`. Process restart needs no in-memory queue: expired
claims are reclaimed from PostgreSQL.

Up to eight claims are allowed per event, including process crashes. Failures
wait 5, 10, 20, 40, 80, 160, then 300 seconds before the next attempt. After the
last failure or last claimed lease expiring, the event remains `exhausted` with
no delivery timestamp. There is no automatic purge or fake ACK. Operations must
monitor `notification_outbox` for exhausted events and oldest pending age; an
audited retry/reconciliation UI is not included. An endpoint/key change may
deliver the backlog to the newly configured receiver, so changes require normal
operator review.

### Administrator delivery inspection and retry

The Store also provides:

```go
AdminNotificationDeliveries(ctx context.Context, who Identity, page int) (Page[NotificationDelivery], error)
AdminRetryNotification(ctx context.Context, who Identity, eventID string) error
```

Listing is platform-admin-only, newest first, 100 entries/page (page 1–100000).
Fields are `eventId`, `nodeId`, `kind`, `incidentState`, delivery `state`,
`attempts`, `errorCode`, `createdAt` and nullable `deliveredAt`. The query does
not return full event payloads, endpoint credentials, lease tokens or lease
deadlines. Current database platform privileges are rechecked on each call;
tenant owners, revoked administrators and API-key contexts are rejected.

Retry only changes an `exhausted` event to `pending`, resets attempts to zero,
clears the previous token/error/lease and makes it immediately eligible for a
fresh claim. It keeps event identity/content unchanged, so receivers still
deduplicate a previously accepted event. The reset and `notification.retry`
platform audit commit together; audit failure rolls back the reset. Replaying
while already `pending` succeeds without resetting backoff or adding an audit.
Claimed or delivered events return `ErrConflict`; delivered events cannot be
manually resent. Missing IDs return `ErrNotFound`, malformed IDs `ErrInvalid`.
An explicit audited retry starts a new bounded attempt budget; it does not
enable a disabled sender or perform an HTTP request itself.

The parent can dispatch `serveNotifications(w,r,who) bool` after shared
Host/authentication/CSRF/rate-limit middleware and before generic admin routes:

- `GET /api/admin/notifications?page=1` returns the metadata page.
- `POST /api/admin/notifications/retry` accepts only `{"eventId":"..."}` and
  returns `{"ok":true}` on success.

Unknown/duplicate query fields and unknown JSON fields are rejected. The parent
owns route/UI registration; this helper does not modify the shared server.

Claims prefer creation order, but parallel workers and retries can deliver
events out of order. Receivers should compare occurrence counters, state
(`resolved` follows `open` within a given occurrence), and occurrence timestamps
rather than treating arrival order as authoritative.

## HTTPS endpoint and authentication

The operator supplies a complete exact HTTPS origin AND path, for example
`https://alerts.example.com/hooks/reladock`. Bare origins, userinfo, query strings
(including a bare `?`), fragments, noncanonical paths and escaped path aliases are
rejected. The sender cannot change the destination based on event contents.
The operator is responsible for selecting the intended receiver; private/local
HTTPS destinations are not silently substituted or discovered.

No environment proxy is used, redirects are never followed, system TLS
certificate verification remains enabled, and a request including response reads
has a ten-second timeout. Response headers are bounded to 16 KiB and the response
body to 4 KiB. Only a completed 2xx response within these bounds is acknowledged;
redirects, non-2xx, timeout, transport errors and oversized bodies all fail. No
response body or credential-bearing diagnostic is stored or returned in errors.

Requests use these headers:

- `Content-Type: application/json`
- `X-RelaDock-Event-ID: <stable eventId>`
- `X-RelaDock-Timestamp: <current Unix seconds>`
- `X-RelaDock-Signature: v1=<lowercase hex HMAC-SHA256>`

HMAC input is the timestamp header, a literal `.`, and the EXACT raw JSON body
bytes, using the shared secret bytes as key. Each attempt has a fresh delivery
timestamp; event ID and event contents stay stable. The receiver must verify TLS,
compute the HMAC before parsing/acting on the body, compare signatures in constant
time, enforce a clock tolerance (for example five minutes), and require the
header event ID to match the signed body. It must still deduplicate event IDs
because a fresh signature is expected on valid retries. Do not send the secret
as a header or embed it in a URL.

## Verification

Focused tests cover PostgreSQL transactional capture/audit rollback, short
transitions, repeated observation suppression, unique concurrent claims, restart
and stale-token fencing, capped retry/exhaustion, immutable snapshots and repair
backfill. Local TLS sender tests cover the exact route, independently recomputed
HMAC, stable IDs, disabled/partial configuration, no secret serialization, strict
URL validation, no redirects, response bounds, and 2xx-only acknowledgements.

Use `node scripts/start.mjs test -race ./internal/platform ./internal/notify -run
'TestNotifications|TestSender' -count=1` with a local test database. No real
operator notification, customer message or production delivery was performed.
