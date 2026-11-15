# Hybrid Resource Plans Design

## Intent

RelaDock must sell one independent package per relay rule while allowing the
underlying node resource to be shared or dedicated. Sharing applies only to the
server, public address, and upstream line. It never creates a tenant-wide or
node-wide traffic pool.

This increment keeps node procurement manual. It does not call a cloud provider
API, automatically purchase a server, or automatically release a server. Those
actions remain explicit operator work until a later provider-adapter project.

## Confirmed product rules

- One relay rule is one billable line and has at most one current package.
- Upload and download bytes are summed for that rule's package quota.
- Quota and bandwidth are independent per rule and cannot be transferred,
  borrowed, merged, or shared with another rule.
- Reaching quota stops that rule; no overage billing is enabled.
- After a package is exhausted or expired, buying another package creates a new
  period with zero package usage. Historical usage and immutable ledger facts
  remain intact.
- Multiple rules may share one server, public IP, Agent, and upstream line when
  the node is configured for shared use.
- Different package types may select different resource modes. Price, quota,
  bandwidth, and duration remain immutable price-version facts.

## Resource modes

Nodes and plan price versions use the same three modes:

- `shared`: multiple tenants and rules may use the node.
- `dedicated-tenant`: only one configured tenant may create rules on the node;
  that tenant may create multiple rules and every rule still purchases its own
  package.
- `dedicated-rule`: only one configured tenant may use the node and the node may
  contain at most one undeleted rule.

Every existing node and plan price version migrates to `shared`, preserving
existing behavior and data. A dedicated node stores its allowed tenant ID.
Shared nodes must not store a dedicated tenant ID. Dedicated nodes must store a
valid tenant ID.

Node admission is enforced transactionally while the existing node advisory
lock is held. A dedicated-rule count check therefore cannot be defeated by two
concurrent rule creations. An operator may change a node's resource mode or
dedicated tenant only while the node has no undeleted rules; normal name,
region, address, and capacity rules remain unchanged.

Tenant node inventory shows all shared nodes plus dedicated nodes assigned to
the current tenant. An occupied dedicated-rule node remains visible to its owner
for factual status, but the backend rejects a second rule regardless of UI
state. Other tenants do not receive that dedicated node from the tenant-facing
inventory.

## Package catalog and purchase

`plan_price_versions` gains immutable `resourceMode`. Plan code and name remain
the flexible product identifiers for standard, optimized, or custom line
families; no fixed marketing tiers are hard-coded.

The customer package picker filters by the selected rule. Only plan versions
whose resource mode matches the rule's node are offered. The purchase
transaction repeats this compatibility check under the node lock, so a forged
or stale browser request cannot buy an incompatible package.

The existing one-rule accounting model remains authoritative. A package
purchase after expiry or quota exhaustion closes the old period, captures the
new physical baseline, creates a fresh subscription and usage row, updates the
rule bandwidth, and requests a new node configuration. The purchase response
continues to mean configuration requested, not Agent-applied or end-to-end
healthy.

For this first increment, an active package cannot be replaced early. There is
no queued renewal, rollover, proration, refund automation, or early reset. This
keeps the already-tested late-report attribution and wallet semantics intact;
those policies require a separate explicit product decision.

## Administration and presentation

The node dialog exposes resource mode and, for dedicated modes, the dedicated
tenant ID. The node list identifies shared, tenant-dedicated, and rule-dedicated
resources. The tenant list exposes the tenant ID so an operator can assign a
dedicated node without guessing.

The immutable plan form exposes resource mode, and plan tables/catalog labels
show it. Customer copy continues to say that quota and bandwidth belong only to
the selected rule. Buying a replacement after an exhausted/expired period is
described as starting a new period rather than deleting or clearing historical
usage.

## Safety and non-goals

- No cloud-provider credentials, API calls, server purchasing, or automatic
  release are added.
- No VPN users, protocol credentials, subscription links, or protocol
  termination are added.
- No aggregate shared-node bandwidth guarantee or oversubscription policy is
  invented. Existing per-rule and per-node maximum validation remains.
- Resource mode is a control-plane allocation constraint. It is not a health,
  isolation, performance, or SLA claim.
- No Agent or Linux data-plane schema change is required; packet-level quota,
  expiry protection, rule-specific shaping, and period-bound usage reporting
  remain unchanged.

## Verification boundary

Tests must cover migration defaults, invalid mode/tenant combinations,
concurrent dedicated-rule allocation, cross-tenant dedicated-node rejection,
mode-change conflicts, plan immutability, catalog filtering, forged incompatible
purchases, and renewal after quota exhaustion without quota sharing. Full Go
race tests, Node tests, and static analysis remain required. Real cloud API
provisioning and production network behavior are explicitly unverified because
they are outside this increment.
