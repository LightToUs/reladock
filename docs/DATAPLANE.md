# Isolated nftables dataplane component

Implemented in `internal/dataplane`, with a diagnostic `cmd/reladock-dataplane` CLI and a composed explicit Linux mode in `cmd/reladock-agent`. The default validation Agent never applies networking. Application ACKs are generated only through the journaled Agent/controller begin/report protocol, never by this standalone CLI; no component equates them with end-to-end health.

Latest user correction: bandwidth and traffic belong to each rule, not an enterprise pool. `ShapedMeterHooks` composes the separately owned `internal/shaping` adapter and durable `internal/metering` WAL. Both directions and both transports of a rule share its HTB ceiling and quota. Standalone Compile/CLI without shaping options is diagnostic only and must not be used to claim production bandwidth enforcement.

The composed Agent freezes only the platform relay chain before final sampling and tc changes; disk or recovery uncertainty stays fenced. Kernel quota budgets subtract a fixed controller baseline plus all local physical epochs, including ACKed/pruned reports. A new incarnation is assigned on every table rebuild and rollback. Timeouts are kernel-owned, with a 35-second conservative safety margin against the bounded 30-second transition. Offload/flowtable prerequisites are checked without altering unrelated networking. Actual container tests cover this composition, quota exhaustion, no quota reset on replacement and expiry of an established TCP flow without an Agent timer. Source semantics for named quotas: [official nftables manual](https://netfilter.org/projects/nftables/manpage.html).

## Interfaces

```go
Compile(ctx context.Context, cfg platform.NodeConfig, opts Options) (Plan, error)
NewAdapter(opts Options, runner Runner, store StateStore, lock Locker, verify Verifier) (*Adapter, error)
(*Adapter).Apply(ctx context.Context, cfg platform.NodeConfig) (Plan, error)
(*Adapter).Recover(ctx context.Context) error
NewFileStore(absolutePrivateDirectory string) (*FileStore, error)
(*FileStore).Locker() Locker
NewExecRunner(absoluteNftPath string) (*ExecRunner, error) // Linux only
ReadbackVerifier(runner Runner) Verifier

type Resolver interface {
    LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}
type Runner interface {
    Run(context.Context, []string, string) ([]byte, error)
}
type StateStore interface {
    Load(context.Context) (State, error)
    Save(context.Context, State) error
}
type Locker interface {
    Lock(context.Context) (unlock func(), err error)
}
type Verifier func(context.Context, Plan) error
```

`Options` contains expected `NodeID`, concrete `SNATAddress` and injectable `Resolver`. `Plan` contains frozen/sorted `Config`, `Script`, node/generation, SNAT address, counter mappings and exact current/retired `Allocations`. Apply accepts NodeConfig, never a caller-supplied script. Standalone Compile cannot include existing state history; Apply adds it while locked.

Runner receives nft argument vectors and stdin. ExecRunner uses an absolute executable, an allowlist, bounded output and no shell. Resolver lookup uses `ip` so mixed IPv4/IPv6 answers are rejected rather than silently filtered. Go's IPv4-mapped representation of DNS A answers is normalized only at the resolver boundary; mapped config literals remain rejected. Valid IPv4 answers are sorted and the first is pinned in last-good state. DNS refresh requires a newer generation. No instant DNS change or existing connection migration is promised.

Identifiers use literal ASCII letters/digits/underscore/hyphen, length 1–80. Node identity, schema 1, positive generation/rule version, status, protocol, ports and bandwidth are validated. TCP/UDP are independent allocations; `both` expands to both. Colliding paused allocations are rejected too. Rule names never enter nft source. Only `pending-agent` rules produce NAT/counters; paused rules retain exact allocation guards.

## CLI flags

| Flag | Meaning/default |
| --- | --- |
| `--config` | Strict NodeConfig JSON regular file, at most 2 MiB |
| `--node` | Required expected node identity |
| `--snat` | Required concrete source NAT IPv4 |
| `--dry-run` | Compile/print only; default without apply |
| `--apply` | Explicit Linux-only nft execution opt-in |
| `--state-dir` | Required for apply; absolute private directory, final directory created as 0700 |
| `--nft` | Trusted absolute nft path; default `/usr/sbin/nft` |
| `--timeout` | Default 30s, positive, maximum 5m |
| `--recover` | Requires apply; restore pending last-good instead of applying config |

Safe plan example:

```sh
go run ./cmd/reladock-dataplane --config /absolute/node-config.json \
  --node hk-01 --snat 192.0.2.2 --dry-run
```

An authorized Linux operator can replace dry-run with `--apply --state-dir /absolute/private-state`. Recovery uses `--apply --recover --state-dir ... --node ... --snat ...` without config. Recovery never runs automatically. Dry-run constructs no runner and creates no state; domain names can still cause DNS resolution. Use literal targets when DNS activity is unwanted.

## Ownership and transaction boundaries

Fixed table: `ip reladock`, exact comment `reladock-dataplane-v1`. `nft -j list tables` determines existence only; ownership is read separately with `nft -j list table ip reladock`. Inventory comments are not assumed available. Read failures and missing/wrong comments fail closed. A marked existing table without last-good state is not adopted. The marker is cooperative ownership, not protection from another privileged writer forging/modifying it.

The batch idempotently adds, deletes and recreates **only that table in one nft transaction**. No global flush, sysctl, IP-forwarding enablement, route, other firewall table, conntrack deletion, interface or qdisc modification occurs. Forwarding and compatible firewall/routing are operator prerequisites. Other tables may still drop traffic after this table accepts it.

Forward handling matches only exact original `(ingress IPv4, protocol, port)` allocations from the snapshot or retained last-good history. There is no blanket 20000–60000 guard. Retired tuples stay guarded; reassigning them to other software requires operator reconciliation. No retirement garbage collector is supplied. Changing ingress/SNAT on existing state is rejected pending separate conntrack migration design.

NAT is IPv4 TCP/UDP DNAT plus explicit SNAT; targets see the relay's source address. A concrete unicast ingress IPv4 is required. Wildcard `0.0.0.0`, loopback, multicast, IPv6, mapped IPv6, zones, URLs, unresolved/unsupported targets and mixed DNS answers are rejected. The platform's wildcard default therefore needs explicit ingress configuration first. Targets equal to ingress/SNAT are rejected; the compiler cannot discover other local aliases.

## Persistence, serialization and rollback

Every process writing this table in one network namespace must use the **same state directory and lock**. FileStore uses 0600 temporary files, fsync, atomic rename and directory fsync. A persistent `flock` inode serializes independent Linux processes; macOS supports lock-only tests. Trusted parent directories and exclusive administrative control are required. Final symlink directories/state files are rejected. Replacing the lock inode or using a different directory defeats coordination. State is limited to 8 MiB; durability depends on the storage stack.

After locking, Apply validates persisted state, recompiles frozen Good/Candidate literals to reject corrupt scripts, rejects Pending and rejects generations at or below `HighWater` (`ErrStale`). It compiles, inspects ownership and runs `nft -c -f -`. It then durably saves high-water, Pending, the previous Good and the **complete Candidate plan/allocations** before running `nft -f -`. A required Verifier runs before the candidate becomes Good and Candidate is cleared. Syntax-check failures do not apply and can be retried. Runtime failures consume their generation even after rollback/restart.

Apply, verification or final commit failures restore the old business configuration **recompiled with the union of Good and Candidate allocations**. Newly introduced candidate tuples remain exact drop guards because their conntrack entries can survive table replacement. On initial failure there is no prior business configuration: recovery leaves a verified **guard-only table**, rather than deleting the protection. The resulting protective plan becomes Good only after local verification; this is not a successful application of the candidate's business rules or a platform ACK.

State additionally retains `Tombstones []Allocation`: failed candidate active tuples that the previous Good did not permit. They survive recovery, restart and newer snapshots, and Apply rejects attempts to reuse them as active rules. There is **no automatic removal or removal API**; explicit conntrack reconciliation is required before any future retirement/reuse implementation. Good's active rules continue forwarding; the failed new-port rules have no NAT or allow/counter entries. The guards are exact address/protocol/port tuples, never a blanket range. Older clean states can be read; an older Pending state lacking its Candidate cannot safely reconstruct these tuples and requires manual reconciliation rather than automatic recovery.

Recovery uses a separate bounded 30s context, unaffected by caller cancellation. Runtime/apply failures already have durable pending intent. A failed final state commit may have exposed a candidate after rename, so the Adapter best-effort reasserts the old baseline and Candidate/Pending before kernel recovery. **A failed reassertion never suppresses the kernel restoration attempt.** It checks, atomically restores and verifies first, then attempts to persist the protective Good/Tombstones and clear Pending. Any unresolved storage or restore failure returns `ErrRecoveryRequired`, including explicit Recover failures; errors are never reported as success. The first durable intent remains pending when subsequent writes fail before replacement. If all storage writes fail after an uncertain replacement, durable pending cannot be guaranteed; the error requires operator intervention even if a state file appears clean.

`ErrStateUncertain` identifies FileStore errors after rename (opening/fsyncing the state directory). Pre-rename failures are deterministic with respect to replacing that file. Unknown errors from injected StateStores are conservatively treated as potentially committed when final commit was attempted. Validation/check failures never execute the candidate; runtime failure and commit uncertainty always attempt recovery. Regression tests cover both a visible post-rename candidate and a store that permanently fails every Save after its first durable intent.

Disk/kernel state cannot share a transaction. Interrupted intent requires explicit recovery; the candidate is not assumed healthy. This component does not reconcile conntrack or implement a billing epoch.

## Counters and unverified behavior

Named counters execute in the **forward filter path**, separately for original/reply direction. `Plan.Counters` maps names to rule, tenant, protocol and direction. NAT first-packet counters are never used for traffic totals. These counters count hook traversal, including retransmissions, not successful delivery. Apply/rollback recreate and reset counters. No counter-reader API, usage WAL, billing epoch, deduplication, quotas or billing boundary is implemented.

Readback checks ownership, configuration/generation token, chain types/hooks/priorities, exact counter identities and rule counts. It checks local structure, not complete expression equivalence or end-to-end health. Parents can inject a stronger Verifier; platform ACK and healthy status still require separately verified runtime behavior.

Pause/delete removes NAT/counters and leaves exact allocation guards intended to drop matching old forwarded conntrack packets. **Established connection pause/delete has not been verified here.** Flowtable/hardware offload, conntrack zones, ICMP-related error delivery, fragmentation, reset accounting, restart and allocation reuse remain unverified. Offload may bypass guards/counters. NAT mappings can outlive table replacement; failed candidate mappings are fenced by exact guards, not purged. The new Docker regression specifically exercises an existing TCP socket on a failed candidate's newly introduced port, but awaits a parent rerun. Do not promise immediate stop, migration, universal UDP health or billing completeness. IP-forwarding/sysctl/other firewall tables are untouched; no production path-health claim follows from a successful command.

## Focused tests

```sh
RELADOCK_DATAPLANE_INTEGRATION=0 go test -race -count=1 ./internal/dataplane ./cmd/reladock-dataplane
go vet ./internal/dataplane ./cmd/reladock-dataplane
```

Default tests inject DNS/nft/verification and use private temporary state files. They cover injection, IP families, DNS pinning, both protocols, paused/retired allocations, ownership, stale versions, corrupt state/restart, locks, check/apply order, rollback and uncertain commits. They never execute real nft/tc. Focused checks do not certify the PostgreSQL suite.

## Opt-in Docker integration harness

`TestDockerIntegration` skips unless `RELADOCK_DATAPLANE_INTEGRATION=1`. Supply an **existing local image** and pre-authorized **internal Docker bridge**. The harness never builds/pulls images or creates/deletes networks. The network must have name prefix `reladock-test-`, driver `bridge`, `Internal=true`, label `reladock-test=true`. The parent/operator provisions these resources separately with explicit approval. Docker creates container endpoints on that approved network; nft runs only inside the container namespace.

The image needs `nft`, `python3`, `sleep infinity`; `iproute2` aids diagnostics. A parent-owned `deploy/Dockerfile.dataplane-test` can use Debian 12 and install `nftables python3 iproute2 coreutils`. Authorized image builds may download packages; the harness does not. Linux Docker kernel nft/NAT support must already be available.

```sh
RELADOCK_DATAPLANE_INTEGRATION=1 \
RELADOCK_DATAPLANE_IMAGE=your-already-built-local-image:tag \
RELADOCK_DATAPLANE_NETWORK=reladock-test-your-approved-network \
go test -count=1 -timeout=5m -v ./internal/dataplane -run '^TestDockerIntegration$'
```

Four disposable containers use unique `reladock-test-<random>-{relay,target,client,initial-relay}` names, `--pull=never`, all capabilities dropped and no-new-privileges. Only the two relay namespaces add NET_ADMIN; target/client add none. The second relay covers initial-apply failure without an existing Good. Docker sets forwarding **inside those container network namespaces**. There is no privileged/host-network mode, host/socket mount or extra namespace capability. Private test files store state. Cleanup removes only exact generated names; supplied images/networks remain. Interruptions may require manual removal of recorded container names.

A client reaches the relay eth0 IPv4 and a separate target container, avoiding unsupported loopback targets. Tests exercise real nft check/apply/readback, repeated TCP/UDP echo on single sockets, forward multi-packet counters, invalid batch rejection, foreign-table preservation and rollback after injected runtime failure. Foreign DNAT port 20001 checks that platform port 20000 does not capture other ports in the numeric range. New rollback regressions first prove echo on a candidate-only TCP port, hold **that same socket** across injected verification failure, and assert its traffic and new connections are blocked after rollback and again after Adapter restart/new snapshot. They cover both existing Good and initial apply. Existing Good TCP/UDP and foreign-table forwarding are rechecked. When opted in, unavailable Docker fails rather than silently skipping.

**The earlier Docker baseline was executed by the parent on 2026-10-28 and passed.** It used local `reladock-dataplane-test:local` and a dedicated internal network. **The new candidate-new-port/live-connection regressions have not been executed by this task and require a parent rerun.** Prior baseline results do not establish the new rollback guarantees. Isolated tests do not certify tc, offload, production VPS behavior or production readiness.

## Implementation record

Executed test-first compiler, transaction, state/lock, CLI and readback work, then added the explicitly opt-in Docker harness. User review replaced broad port guards with exact persisted allocations and moved ownership verification to full-table inspection. Both have regression coverage. Reproduced the uncertain post-rename commit failure and added durable recovery-intent reassertion before rollback. No Git initialization or shared Agent/platform edits.

Verification on 2026-10-28: focused race tests and vet passed; Linux amd64 builds passed. Parent integration additionally ran real Docker tests and requested an independent review. Full-workspace verification is recorded in the production-readiness plan.

P1 review fixes: durable Candidate and permanent exact Tombstones now protect failed new allocations through rollback/restart; first failures retain guard-only tables. Recovery-intent write errors no longer bypass kernel restoration. New mock regressions were observed failing before the fixes and now pass under the focused race suite, including interrupted apply and permanent storage failure after the first intent. Focused vet and Linux amd64 CLI/test-binary compilation also passed. The extended live-socket Docker regressions were compiled but not executed here; the parent must rerun them before claiming those new runtime guarantees.
