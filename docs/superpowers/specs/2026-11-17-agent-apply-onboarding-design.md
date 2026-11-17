# Agent Apply Onboarding Design

## Goal

Make a newly enrolled Linux Agent usable without a second manual repair session, while preserving the existing rule that forwarding is never enabled silently. The bootstrap installer must offer an explicit apply choice, prepare only a narrowly supported host, persist the reviewed prerequisites across reboot, and fall back to validation-only on uncertainty. When a configuration was already validated before the mode switch, the control plane must automatically issue a new generation for the apply Agent.

## User flow

After successful enrollment the installer starts the validation unit first. On an interactive terminal it then offers:

1. Enable forwarding (apply mode)
2. Keep validation-only (default)

The apply path discovers one IPv4 default-route interface and one global IPv4 address, shows the proposed interface, address, interface index, network namespace, qdisc handoff, mark namespace and link ceiling, and requires a second literal confirmation. The link ceiling defaults to the product's current 1000 Mbps ceiling and can be entered explicitly. This value is only a shaping budget, not a bandwidth guarantee.

Non-interactive installs remain validation-only unless a future separately authenticated automation contract is designed. Existing-install upgrade/no-action behavior remains unchanged.

## Supported automatic preparation

The apply choice succeeds only when all facts are unambiguous:

- Linux with systemd; required `ip`, `tc`, `nft`, `ethtool`, `conntrack`, `sysctl` and `sha256sum` tools exist at trusted absolute paths.
- Exactly one IPv4 default-route interface and exactly one global unicast IPv4 address are selected.
- The selected SNAT/listen address must be assigned to that interface. Public-NAT mappings whose public address is not locally assigned are not automatically adopted and remain validation-only for manual review.
- The interface is not loopback, its ifindex and current network namespace are readable, and its alias is empty or already owned by this node.
- There are no existing tc classes or filters on the interface.
- The root qdisc is either an empty `noqueue`, or a single empty kernel `fq` root with handle `0:`. The latter is authorized with the exact raw JSON fingerprint required by the existing shaping lease contract.
- No nftables flowtable/offload bypass or existing fwmark policy is detected. Ordinary non-RelaDock firewall rules may coexist and are not an automatic rejection.
- The chosen mark namespace is the existing RelaDock namespace `82`, and no conflicting mark policy is present.

Preparation writes a private `/etc/reladock/shaping.json`, completes the existing private `agent.env`, and creates `/etc/reladock/network-managed` only after confirmation. A packaged root oneshot unit reads those reviewed values and idempotently enables IPv4 forwarding, disables GRO/GSO/TSO/LRO, and assigns the RelaDock interface alias. It is enabled only by this confirmed path. The apply unit starts after it, and both units are enabled across reboot.

Before switching services, the installer runs a read-only apply preflight through the installed Agent. Until the call to start the apply unit, a failure restores the previous `agent.env`, lease, marker, IPv4-forwarding value, offload values and interface alias, then keeps validation running. The apply-unit start request is the irreversible boundary because the process may immediately claim and apply work; after that boundary the installer never attempts network or state rollback. A startup failure preserves all evidence, leaves validation disabled to avoid two modes racing, and prints the documented recovery instruction. It never clears PKI, control journal, data-plane state, shaping state, WAL, or audit data.

## Control-plane redelivery

An authenticated heartbeat already serializes on the node row. During that transaction, when the reported mode changes from a non-apply value to `apply`, the store checks whether the current desired generation is exactly `validated`, is newer than the applied generation, and is still the current outbox row. If so, it enqueues one new full snapshot. The desired generation then advances, making repeated heartbeats idempotent. No reissue occurs for queued, leased, applying, failed, superseded, or already applied work.

This is configuration redelivery only. An applied ACK still does not establish end-to-end TCP/UDP health.

## Release and upgrade behavior

The network-prerequisite unit is included in the signed release allowlist, checksum manifest, manual installer, bootstrap installer and upgrade rollback set. Existing apply installations without `/etc/reladock/network-managed` are not adopted or modified by the new unit. Existing service mode continues to be restored after upgrade.

## Verification

Tests cover:

- transition heartbeat reissues exactly once from a current validated generation;
- no reissue for validation heartbeats, unchanged apply mode, non-current state, or already applied configuration;
- release/package/install scripts include the new unit;
- apply service ordering and least-privilege boundaries remain explicit;
- bootstrap defaults to validation without a terminal and requires confirmation before generating lease/network-managed state;
- installer rollback leaves identity and journals intact.

The repository test suite and static checks are required. Real network mutation, reboot persistence and forwarding remain production-VPS acceptance work unless run on an explicitly authorized test node.
