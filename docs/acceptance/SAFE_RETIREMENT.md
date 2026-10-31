# Safe retirement acceptance

This checklist is the production-like Linux acceptance gate for allocation and node retirement. It is not satisfied by unit tests, a successful build, an Agent heartbeat, or an applied configuration alone. Run it only on explicitly authorized disposable VPS resources with a dedicated ingress/SNAT address and shaping interface. Never use a production database as the test database.

## Preconditions and evidence

- Record release version and archive SHA256, control/Agent host identities, Linux/kernel/nftables/conntrack-tools/iproute2 versions, interface name/index/namespace, and the approved test window.
- Back up PostgreSQL and the node PKI, Agent control journal, dataplane/shaping state, updater state and usage WAL. Keep database and node-state recovery material separate.
- Confirm `/usr/sbin/conntrack --version`, the apply unit preflight, dedicated egress lease, IPv4 forwarding, disabled GRO/GSO/TSO/LRO, and absence of flowtable/hardware bypass.
- Keep validation and apply units mutually exclusive. The validation unit must have no CAP_NET_ADMIN and must not claim retirement jobs.
- Capture control audit records, outbox generations, retirement IDs/allocation IDs, Agent logs, relevant nft readback, conntrack query results and state-file checksums. Do not capture private keys or bootstrap/lease tokens.

## Allocation retirement

1. Apply one TCP and one UDP forwarding rule on distinct allocation identities and prove multi-packet forwarding and per-rule metering.
2. Maintain a TCP connection and create UDP state, then delete the rules. Confirm the newer schema-2 configuration is applied before any cleanup job is claimable.
3. Confirm the raw-priority exact quarantine and allocation-ID forward guard exist. New packets for the retired tuples must be blocked; unrelated tuples and unrelated firewall tables must continue working.
4. Interrupt each boundary separately: before authenticated begin, after begin/before purge, after purge/before proof persistence, after proof/before report, and after report/before ACK. Restart the Agent after every interruption.
5. Verify replay uses the same retirement/allocation identity, does not repeat a known completed purge, never removes quarantine on an unknown result, and never releases the database allocation before authenticated ACK.
6. Restore the control connection. Confirm report replay completes, the allocation becomes released, and local state moves from `purged-pending-ack` to `reusable` without another kernel purge.
7. Create a replacement only with the exact predecessor retirement ID. Confirm the matching quarantine is consumed atomically; a wrong predecessor, paused replacement, protocol mismatch, address mismatch or port mismatch must fail closed. The independent TCP/UDP sibling quarantine must remain.

## Node draining and final retirement

1. Put the node in maintenance, then enter draining. Confirm the transition is irreversible and installation credentials are revoked. New rules, rule resume, ordinary node edits and new/advancing Agent upgrades must be rejected; rule deletion and required configuration retry remain available.
2. Remove every rule and let allocation retirement finish. Confirm the admin page reports each remaining gate truthfully: rules, allocations, retirements, configuration generation, active configuration, upgrade, rotation and attestation.
3. Confirm the Agent cannot attest while its retirement journal, usage WAL, dataplane transition or updater state is non-empty/unstable.
4. Obtain a drain attestation, then independently change each binding: certificate identity, kernel boot ID, Agent process start, Agent version, journal sequence, node revision/generation and reusable quarantine set. Every change must consume or invalidate the old attestation.
5. Obtain a fresh attestation and wait beyond the freshness window; finalization must fail. Obtain another fresh attestation and finalize while all database locks/gates are stable.
6. Confirm finalization revokes Agent authentication, clears no historical retirement/quarantine/audit evidence, exposes no force-release action, and cannot transition the node back to ready or maintenance.

## Recovery and pass criteria

- Restore the control process and Agent from their documented backups on isolated replacement hosts. Verify wallet/ledger consistency separately; retirement evidence is not a billing reset.
- A test fails if any unknown state is cleared by timeout, an allocation is reused without exact predecessor proof, a purge is repeated after a durable result, an old runtime attestation finalizes a node, or unrelated firewall/qdisc state changes.
- Record every command outcome and artifact checksum in the change ticket. Redact credentials. One complete pass is required per supported architecture/kernel/toolchain combination before declaring that combination accepted.

## Current execution record

As of 2026-10-31, automated PostgreSQL, Agent, dataplane, HTTP, migration, release-package and static safety tests cover the state machines and failure boundaries. The opt-in isolated Docker dataplane scenario covers real nftables quarantine, conntrack cleanup and exact predecessor reuse when explicitly enabled. This task did **not** execute the complete checklist on an authorized production-like VPS, did not perform off-host disaster recovery, and did not establish end-to-end customer protocol health or capacity. Those items remain open production gates.
