# Operations and support (migration v8)

This subsystem persists control-channel incidents and support conversations. It does not probe forwarding, send email, or measure revenue, costs, or profit. A recent Agent contact or validated configuration never establishes data-plane health.

## Parent integration

Register `internal/platform/migration8.sql` as version 8 in the existing transactional migration runner, after version 7. There are no new dependencies. The operations files do not start workers, expose listeners, or modify network rules.

Call `serveOperations(w, r, who) bool` after shared Host/origin/request-header/rate-limit and identity middleware, **before** generic `/api/admin/*` routing. It handles `/api/operations/*` and `/api/admin/operations/*`. For bearer authentication, preserve the `WithAPIKey` context. Platform operations reject key contexts at the store boundary even when a key belongs to a platform administrator.

The control process schedules `Store.ReconcileIncidents(ctx, staleAfter time.Duration) error` every 30 seconds with a bounded context. The default display and worker threshold is `platform.DefaultContactStaleAfter` (5 minutes); valid thresholds are 1 second through 30 days. Each node uses a separate bounded transaction and node lock. Busy node locks are skipped for the next cycle, so one blocked node does not stall other nodes or hold the global authorization lock. Multi-worker deduplication and audit remain transactional per node.

## Contact observations and incident lifecycle

`ListOperationsNodes(ctx, who, admin, staleAfter, page)` returns the actual latest control-channel contact, desired/validated generations, latest desired configuration's delivery state, and affected rule IDs. Its contact status is:

| Value | Meaning |
| --- | --- |
| `unknown` | No certificate is enrolled; there is no authenticated Agent observation to monitor. |
| `never-seen` | Certificate enrolled, but no `last_seen_at` exists. No invented enrollment timeout or stale incident. |
| `recent` | Last recorded control contact is within the configured threshold. |
| `stale` | An enrolled Agent's recorded control contact exceeds that threshold. |
| `maintenance` | Node is in maintenance; automatic incidents are suppressed. |

`dataPlaneStatus` is always `unknown`. `deliveryState` is the persisted outbox state for the node's desired generation, or `unknown` if there is no corresponding delivery. Queries use the PostgreSQL clock rather than the browser clock.

`ReconcileIncidents` monitors enrolled nodes outside maintenance. It opens `contact-stale` only when a non-null recorded contact is old; it opens `config-failed` only when the desired generation's latest delivery is failed. Historical failed configurations cannot keep an incident open after a newer configuration is queued or validated. Missing contact alone never opens a stale incident.

An incident has one persistent ID per `(node_id, kind)`. Repeated observations refresh `observedAt` without duplicating the incident or audit event. Recovery, certificate revocation, or maintenance resolves an open incident. Recurrence reopens the same ID and increments `occurrences`; `openedAt` becomes the latest opening time. A resolution means the trigger ceased to be actionable, not that traffic recovered. Open/resolve transitions and `platform_audit_logs` records (`operations-worker`, `incident.open` / `incident.resolve`) commit in the same transaction.

Tenant queries include only nodes with the tenant's non-deleted rules, and only those rule IDs. Paused rules remain configured and are included. Visibility follows current rule association; it is not a historical ownership archive. Platform administrators can query all nodes/incidents. No payloads, certificate fingerprints, lease tokens, customer targets, or other tenants' rule IDs appear in tenant monitoring results.

## Support workflow and authorization

Owner/admin sessions and write-scoped tenant keys may create tickets and reply. Viewer/read-scoped keys may read their own tenant's tickets. Tenant writes recheck the current member role, tenant status, key expiry, scope and revocation in their transaction. Platform replies require a current platform-admin session and are audited as that operator, without impersonating a tenant member. Platform keys are forbidden.

Ticket subjects are trimmed, valid UTF-8, 1–160 characters. Messages are trimmed, valid UTF-8, 1–4000 characters; null bytes and blank values are rejected. The shared HTTP 8 KiB request limit may further constrain multi-byte content. Messages are plain text, not HTML. Bodies are stored only in the conversation, not copied to audit logs.

Tickets start `open`, revision 1. Every reply requires the current `revision`; stale replies return 409 without adding a message. An admin reply explicitly sets `open`, `waiting-customer`, or `resolved`. A tenant reply omits `status` and reopens the ticket. Message insert, status/revision update, and audit (`support.create` / `support.reply`) are one transaction. Cross-tenant reads and replies return 404.

## HTTP contracts

All requests use the parent's shared authentication/security middleware. JSON bodies use strict decoding; unknown fields, multiple JSON values, invalid types, and oversized requests are rejected.

| Method and tenant path | Input / response |
| --- | --- |
| `GET /api/operations/nodes` | Optional `page` and `staleAfterSeconds`; `Page[OperationNode]`. Display threshold does not change worker policy. |
| `GET /api/operations/incidents` | Optional `page`; `Page[Incident]`, including open and resolved incidents. |
| `GET /api/operations/tickets` | Optional `page`; `Page[SupportTicket]`. |
| `POST /api/operations/tickets` | `{ "subject": "...", "body": "..." }`; 201 `SupportTicket`. |
| `GET /api/operations/tickets/{id}` | `SupportTicketThread` with ticket and latest 100 messages in chronological order. |
| `POST /api/operations/tickets/{id}/replies` | `{ "body": "...", "revision": 1 }`; updated `SupportTicket`. |

Admin queries and replies use the same suffix under `/api/admin/operations/`. Admin reply JSON additionally requires `status`. Admin ticket creation is not exposed. Lists return at most 100 rows per page (`page` 1–100000); each node/incident includes at most 100 matching rule IDs. Thread history currently exposes only the latest 100 messages, without an older-message cursor. List responses include `total`, `page`, and `pageSize`. Responses are `Cache-Control: no-store`.

Store interfaces additionally include `ListIncidents(ctx, who, admin, page)`, `CreateSupportTicket(ctx, who, subject, body)`, `ListSupportTickets(ctx, who, admin, page)`, `SupportThread(ctx, who, id, admin)`, and `ReplySupportTicket(ctx, who, id, body, status, revision, admin)`. The `admin` flag selects the namespace; store authorization independently rechecks platform access.

## Verification and limits

PostgreSQL tests create random schemas and never migrate the business schema. Coverage includes concurrent incident deduplication, recovery, unknown contact and maintenance, tenant visibility, cross-tenant conversation access, revision conflicts, role/key boundaries, and audit failure rollback. Run `npm test` and `npm run vet` after parent integration.

No external notifications, SLA claims, endpoint probes, attachments, staff assignment, message delivery receipts, ticket retention policies, or cost/profit summaries are implemented. Migration 8, the periodic worker, tenant routes and admin routes are registered in the control process.
