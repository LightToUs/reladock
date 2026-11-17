# Source IP Blocking and Retention Design

## Intent

Give platform operators a safe way to block abusive client source addresses at a RelaDock entrance without installing software on the customer-operated target. Keep optional source-flow metadata bounded by the configured retention period through an explicit recurring worker.

## Decisions

- Blocking is platform-admin-only because raw source addresses are currently visible only to platform administrators.
- A block belongs to one relay rule. It never silently affects another tenant, rule, node, or port.
- Version one accepts canonical IPv4 CIDR only. A single address is stored as `/32`; `/0`, multicast, unspecified, loopback, and non-canonical network strings are rejected. The current data plane is IPv4-only, so IPv6 must not be presented as supported.
- Each rule is limited to 256 active CIDRs. Duplicate and overlapping CIDRs are allowed only when they are different exact canonical values; exact duplicates are idempotent.
- Operators provide a bounded reason. Creation and removal use the rule's current desired version, take the existing node/rule locks, increment the rule version, enqueue a complete node snapshot, and write platform audit in the same transaction.
- Blocks are removed explicitly in version one. Temporary block expiry is deferred; this avoids pretending that a control-plane outage can guarantee an exact unblock time.
- Configuration schema 5 carries per-rule blocked CIDRs. A certificate-bound Agent capability gates schema-5 leasing, so an older Agent cannot ignore the new field and report an unsafe applied result.
- The Agent compiles blocks into a dedicated raw-prerouting nftables chain before DNAT. Rules match the entrance address, transport, entrance port/range, and source CIDR, then drop. Paused rules retain their configured block list in the snapshot but emit no forwarding or block rule.
- The readback verifier checks the new chain, exact comments, source/destination/transport/port expressions, and rule counts. Applied configuration remains distinct from end-to-end health.
- The source-flow page offers a manual CIDR form and a one-click `/32` action on a recorded event, plus a list/removal surface for the selected rule.

## Source-flow retention

- Existing `SOURCE_FLOW_RETENTION_DAYS=30..365` remains authoritative; production currently uses 180 days.
- The controller runs one bounded cleanup immediately at startup and every 30 seconds thereafter, deleting at most 1,000 eligible rows per pass.
- Events and closed gaps use controller `received_at`; open gaps are never deleted. Superseded manifests are deleted only after the retention period when they are not current and have no event reference.
- Billing usage, wallet, ledger, rule audit, and platform audit are outside this cleanup.
- Cleanup failures are observable through the existing worker metrics path and logs, and retry on the next interval. The admin source-flow response exposes the configured retention days so the UI states the actual policy.

## Non-goals

- No HY2 payload inspection, VPN account blocking, ASN/country/reputation feeds, automatic behavioral bans, IPv6 forwarding, or target-machine changes.
- No global account-wide deny list in this increment.
- No claim that an empty source-flow result proves no traffic or that a block is active before Agent applied ACK.

## Acceptance

- A platform admin can add/remove an IPv4 CIDR for one rule and see requested versus applied versions.
- Cross-tenant, viewer, stale-version, deleted-rule, malformed/broad CIDR, duplicate, capacity, audit failure, and Agent-capability paths are covered.
- Schema-5 compilation and readback reject near-miss expressions and preserve unrelated firewall tables.
- Retention tests prove old events/closed gaps/superseded manifests are removed while open gaps/current manifests and non-source-flow facts remain.
- No production deployment or live firewall mutation occurs in this implementation task.
