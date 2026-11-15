# Rule Source Flow Logging Design

## Intent

RelaDock operators need an auditable way to associate a relay rule with the
public source IP address and source port that opened a connection. The feature
is for abuse investigation and network-security operations. It must remain
optional because high connection rates create kernel event, disk, upload, and
database costs.

This feature records connection metadata only. It does not inspect packet
payloads, terminate customer VPN or proxy protocols, identify a person behind
NAT/CGNAT, or reveal the final destination inside an encrypted tunnel.

## Product behavior

- Source-flow logging is configured independently on every non-deleted relay
  rule and defaults to disabled for existing and newly created rules.
- Only a platform administrator may enable or disable it. Tenant users can see
  the requested and applied state but cannot change it.
- Changing the setting requires the current `desiredVersion`, takes the node
  lock, increments the rule desired version, enqueues a complete configuration
  snapshot, and writes platform audit in the same transaction.
- The UI distinguishes requested state from applied state. A successful HTTP
  request means "waiting for Agent application", not "recording is active".
- Disabling stops capture of new flows after the disabling configuration is
  applied. Existing records remain until retention cleanup.
- Only Linux apply-mode Agents that have advertised the source-flow capability
  may receive an enable request. Validation-only, offline-without-capability,
  and older Agents cannot be represented as actively logging.

## Captured fact

The Agent observes the conntrack `NEW` event for an applied rule and uses the
original conntrack tuple, before SNAT, as the source fact. Each accepted record
contains:

- an Agent-generated event ID and authenticated node ID;
- tenant, rule, allocation, rule version, and applied config generation;
- protocol (`tcp` or `udp`);
- original source IP and source port;
- relay listen IP and listen port;
- resolved target IP and target port from the applied configuration;
- kernel observation time and control-plane receive time.

The record represents a newly observed conntrack flow, not an application
session. UDP is explicitly labeled as a conntrack pseudo-flow. The first
release does not add per-flow final bytes, end time, DNS/SNI, URL, payload,
PCAP, automatic classification, or automatic blocking. Existing per-rule
metering remains the only traffic-usage and billing path.

`observed_at` is the Agent wall-clock time, truncated to UTC microseconds, when
the netlink `NEW` message is accepted. It is not represented as a kernel event
timestamp. The record also carries the kernel boot ID, conntrack zone, and
conntrack ID as diagnostic evidence. Event identity is a fresh cryptographic
128-bit Agent ID persisted before upload; replay deduplication uses that ID.
The kernel `NEW` stream is not assumed to replay after reconnect, so collector
restart is represented as a gap rather than reconstructed events. The first
release does not attempt to collapse distinct raw `NEW` messages by five-tuple;
legitimate five-tuple reuse must remain visible.

## Control-plane model

Migration v34 adds:

1. `relay_rules.source_logging_enabled boolean NOT NULL DEFAULT false` for the
   requested state.
2. `relay_rules.source_logging_applied_enabled boolean NOT NULL DEFAULT false`,
   `source_logging_applied_version bigint NOT NULL DEFAULT 0`, and
   `source_logging_applied_generation bigint NOT NULL DEFAULT 0` so the UI can
   prove the last applied state and the exact rule/config versions that carried
   it, including enable/disable/enable requests superseded before an ACK.
3. `nodes.agent_source_flow_supported boolean NOT NULL DEFAULT false`, set
   only by an authenticated heartbeat capability header. The existing
   certificate-identity replacement trigger clears it.
4. `node_source_flow_latest`, one row per node, holding the most recent
   authenticated collector status, durable backlog count, dropped total,
   collector epoch/sequence, observation time, and receive time.
5. `source_flow_events`, append-only and tenant-bound, with unique
   `(node_id,event_id)`, strict address/port/protocol checks, the fields listed
   above, and indexes for `(rule_id,observed_at DESC)`,
   `(source_ip,observed_at DESC)`, and `(node_id,observed_at DESC)`.
6. `source_flow_gaps`, append-only node-wide gap intervals keyed by node and
   collector epoch/sequence, with bounded reason codes, dropped count, open
   time, and optional close time. A gap conservatively applies to every enabled
   rule on that node whose query window overlaps it. It is a first-class fact
   and must never be rendered as complete logging.
7. `applied_source_flow_rules`, immutable per node/config generation, storing
   the exact allocation, rule/version, logging state, resolved target, and
   relay tuple reported with a successful schema-3 applied ACK. It is retained
   for at least the source-event retention horizon so delayed WAL events can be
   validated after edits, disablement, or retirement.

Source-flow events are immutable through ordinary application APIs. The Agent
upload transaction authenticates the node through the existing mTLS channel,
verifies that every event exactly matches `applied_source_flow_rules` for that
authenticated node/generation, inserts a bounded batch idempotently, and
acknowledges only after commit. Cross-tenant, cross-node, stale-generation, and
Agent-claimed target attribution without a stored manifest are rejected rather
than corrected.

The deployment configuration controls retention. There is no implicit
production value: when `SOURCE_FLOW_RETENTION_DAYS` is absent the controller
starts normally with source logging unavailable, and enable requests fail
closed with a concrete reason. An explicit value from 30 through 365 days is
required; 180 days is the documented recommendation. Retention is measured
from control-plane `received_at`, so a delayed durable upload remains available
for the configured period. A bounded worker deletes expired rows in small
batches without touching billing usage or audit records. Encrypted backups
follow their separately documented backup retention; a restore must run source
retention cleanup before serving queries. The operator must confirm the value
against the applicable jurisdiction and contracts before production rollout.

## Configuration delivery and compatibility

`ConfigRule` gains `sourceLoggingEnabled`. A snapshot containing any enabled
rule uses node-config schema 3. New Agents accept schemas 1 through 3; older
Agents reject schema 3 instead of silently ignoring the request. The control
plane refuses an enable mutation until the currently authenticated Agent has
advertised schema-3/source-flow support.

The applied configuration stores enough allocation and resolved-target data
to build an exact lookup from original destination address, protocol, and port
to rule identity. A successful schema-3 applied report includes the canonical
source-flow manifest and digest; the platform validates it against the claimed
snapshot and persists it in `applied_source_flow_rules` in the ACK transaction.
The collector changes its active lookup only after the same configuration has
been successfully applied and durably promoted to last-good. Recovery reloads
that last-good lookup before capture resumes. Candidate or failed
configurations never change flow attribution.

An applied report advances `source_logging_applied_version` to the source
logging state in that exact generation. A later applied disable proves that
new events should stop; it does not delete prior rows.

## Agent collection and WAL

The Linux apply Agent adds two independent supervisor activities:

- a long-running conntrack event collector; and
- a bounded batch uploader over `/agent/source-flows`.

The collector uses the kernel conntrack netlink event stream rather than nft
packet logging. It accepts only `NEW` events whose original tuple matches an
enabled rule in the durable last-good lookup. It ignores unrelated host flows,
reply tuples, disabled rules, retired allocations, and any event it cannot
attribute exactly.

Before upload, an event is stored in a new private, locked, checksummed source
flow WAL under the existing Agent state directory. It is separate from the
billing/metering WAL so source-log pressure cannot corrupt, reset, or block
billing usage. The WAL has persisted byte and record limits, refuses option or
identity changes after creation, and prunes only acknowledged events.

Gap metadata has reserved capacity outside event admission. Event admission
stops before that reserve is consumed, allowing WAL-full to open and later
close a durable gap. Gap reasons are a closed set:
`netlink-overrun`, `collector-restart`, `collector-error`, `queue-full`,
`wal-full`, and `wal-unavailable`. Gaps are node-wide, open at the first known
loss boundary, accumulate a monotonic dropped count, and close only after the
collector is subscribed again, the WAL can persist, and the uploader can send
the gap record. An unclean Agent start opens `collector-restart` from the last
durable collector checkpoint; a clean stop while no source-enabled rule exists
does not create a gap.

The uploader sends bounded batches and removes them only after an authenticated
durable receipt. Replays are idempotent. Network and control-plane failures
back off independently and never stop local metering.

Forwarding remains fail-open with respect to this optional log. If the event
socket, queue, or source-flow WAL is unavailable/full, the Agent must not block
or freeze customer forwarding. It increments a monotonic dropped/gap fact,
persists health when possible, reports the condition in heartbeat, and emits an
operational incident after the existing sustained-condition policy. The UI
must show the interval as incomplete. This differs intentionally from billing
WAL failure, which keeps its existing fail-closed safety behavior.

Collector health is `normal` only when the heartbeat is fresh, the event
subscription is alive, no gap is open, no drop count has advanced, and either
there is no backlog or its oldest record is at most 90 seconds old and WAL use
is below 80 percent. An open gap or new drop degrades immediately; old backlog
or WAL use at/above 80 percent degrades after 90 seconds. A collector incident
opens after two minutes of continuous degradation and resolves after two
minutes with normal status. A quiet subscription with no new customer flow is
normal, not stale. Node contact staleness remains a separate status.

The collector never changes sysctls, offload, host logging, firewall rules, or
network prerequisites automatically. Installation and preflight verify the
required conntrack event capability and privileges before an administrator may
enable the feature.

## HTTP and authorization

- `PATCH /api/admin/rules/:id/source-logging` accepts exactly `tenantId`,
  `desiredVersion`, and `enabled`.
- The mutation uses existing platform-admin authorization, same-origin/Host
  checks, strict JSON, PostgreSQL shared rate limits, node/rule locking, and
  platform audit.
- `GET /api/admin/source-flows` supports bounded, paginated filters for rule,
  node, protocol, source IP, and a required bounded time range. It never returns
  payload because none is collected.
- The first release does not provide bulk export. Each successful administrator
  query writes a privacy-access audit entry without copying result addresses
  into audit details.
- Tenant rule responses expose requested/applied/health labels only. Raw source
  IPs and ports are platform-admin-only in the first release.

## UI

The platform global-rule row gains a source-logging control and status. The
confirmation dialog explains performance, privacy, retention, and the fact
that activation waits for Agent application. Unsupported nodes disable the
control with a concrete reason.

A platform source-flow page provides server-side pagination and filters. It
shows observed time, company/rule, node, protocol, source tuple, relay tuple,
target tuple, config generation, and whether the surrounding interval has a
known collection gap. It never labels a period complete merely because no rows
were returned.

Tenant rule rows show only one of: disabled, waiting to enable, enabled,
waiting to disable, collector degraded, or unsupported. Tenant users do not
receive raw source tuples.

## Performance and capacity boundaries

- Work is per new conntrack flow, never per forwarded packet.
- Kernel event ingestion, in-memory buffering, WAL size, upload batch size,
  request body size, database page size, query time window, and retention
  deletion batch are all bounded.
- The collector must remain responsive under short-connection and UDP floods;
  overload produces explicit gaps rather than unbounded memory/disk growth.
- Database inserts are batched and idempotent. Queries require a time bound and
  use indexed predicates.
- No throughput or maximum flows-per-second claim is made until the isolated
  Linux stress test and a production-like VPS acceptance test are completed.

## Failure and privacy boundaries

- Source IP plus port is evidence about a public network endpoint, not a
  verified natural person.
- Encrypted customer tunnels expose only the customer-to-landing connection;
  this feature cannot reveal the tunnel's final websites or application data.
- Agent downtime, kernel event loss, process restart before durability, WAL
  exhaustion, and upload backlog can create gaps. Every known gap is retained
  and displayed.
- Logs are access-controlled, query-audited, excluded from ordinary tenant
  APIs, excluded from billing, and excluded from public Prometheus labels.
- Backup and recovery documentation must classify the new table as sensitive
  operational data and preserve its retention/access rules.

## Verification

Tests must cover at least:

- v34 migration preservation, defaults, constraints, indexes, and rerun;
- admin-only enable/disable, desired-version conflict, audit rollback, node
  capability, certificate replacement, and concurrent rule/node mutations;
- schema-3 snapshot compatibility and exact applied-state advancement;
- conntrack original-tuple attribution before SNAT for TCP and UDP;
- disabled-rule filtering, unrelated host flow filtering, legitimate
  five-tuple reuse, duplicate WAL/upload events, rule replacement/retirement,
  and last-good recovery;
- source WAL corruption, identity/option mismatch, capacity exhaustion,
  replay, durable ACK, restart, and independent billing WAL survival;
- mTLS upload node/rule/tenant/generation validation, strict input, batch
  limits, duplicate upload, and transactional rollback;
- paginated/filtering access isolation, query audit, retention batching, and
  incomplete/gap rendering;
- isolated Linux NAT tests proving source tuple is captured before SNAT and
  forwarding continues when optional source logging fails;
- stress measurement for short TCP connections and UDP pseudo-flows, reported
  as measured results rather than a production capacity claim.

## Delivery boundary

Implementation and automated/isolated tests do not authorize production
deployment, do not enable any existing rule, and do not modify a production
node's network or Agent. Production rollout requires a compatible Agent
upgrade, explicit operator enablement per rule, retention confirmation,
capacity observation, and an updated acceptance record in
`docs/IMPLEMENTATION_STATUS.md`.
