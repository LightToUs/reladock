# Node-bound Plan Catalog Design

## Intent

RelaDock already sells immutable, per-rule plan periods and supports shared,
tenant-dedicated, and rule-dedicated node resources. The missing commercial
boundary is a concrete line product. A shared plan version currently matches
every shared node, so a Hong Kong package could also be offered for a future
Singapore node.

This increment introduces a line offering bound to one manually prepared entry
node. Plan price versions belong to that offering, and a customer can buy a
version only for a rule already assigned to the same node. It uses the existing
Hong Kong node without provisioning, deploying, purchasing, or modifying any
production server.

## Confirmed product behavior

- A line offering represents one sellable route on one concrete entry node.
- The offering carries the customer-facing name, source market, destination
  market, node binding, resource mode, and sale state.
- One offering may have several immutable price versions with different price,
  quota, bandwidth, and duration.
- One rule still owns one independent package period. Rules sharing a node do
  not share bytes, bandwidth limits, duration, or billing state.
- Only a published offering on the rule's exact node is purchasable.
- Pausing, selling out, or retiring an offering blocks new purchases and
  renewals but does not rewrite an existing subscription or ledger record.
- Existing unbound price versions remain readable historical facts. They are
  not automatically guessed onto a node and are not offered for new purchase.
- No package, price, quota, or bandwidth is seeded for the Hong Kong node;
  those are explicit administrator decisions.

## Data model

`line_offerings` stores a mutable catalog record:

- immutable identity: UUID and unique lowercase code;
- node binding: one existing node ID;
- presentation: name, source market, destination market;
- derived resource mode copied from the bound node for API presentation;
- sale state: `draft`, `published`, `paused`, `sold-out`, or `retired`;
- revision and timestamps for optimistic administrator updates.

`plan_price_versions.offering_id` is a nullable foreign key. It is nullable only
so existing immutable versions can survive migration without an invented node
binding. Every newly created version must reference an offering. Its resource
mode is derived from the offering's bound node and stored on the immutable
version as it is today.

Offering code and node binding do not change after creation. Presentation is
also fixed in this increment; an operator creates a replacement offering if the
commercial identity changes. Only the sale state changes in place. `retired` is
terminal.

## Administration

The existing rule-plan administration page gains two sections:

1. Create and manage line offerings. The administrator chooses an existing
   node and supplies code, name, source market, and destination market. New
   offerings begin as `draft`.
2. Create immutable price versions under an offering. The resource mode is
   shown from the offering and is no longer independently selected.

Publishing requires the bound node to be in the control-plane `active` state.
This is an admission check, not a claim of capacity, Agent application, or
end-to-end health. A published offering may be changed to paused, sold out, or
retired. A paused or sold-out offering may be published again if the node is
active. A retired offering cannot be reopened.

Every create or state transition requires platform administrator permission and
writes a platform audit record in the same transaction.

## Customer catalog and purchase

The customer selects a rule first. The catalog then returns only versions that:

- belong to a `published` offering;
- are bound to the exact node used by the selected, owned, nondeleted rule;
- have the same immutable resource mode as that node.

The purchase transaction repeats all checks while holding the existing node
lock. It also locks the offering row against concurrent state changes before
wallet debit. A stale or forged request for another node, a legacy unbound
version, or a non-published offering fails atomically without changing the
wallet, ledger, subscription, rule, or configuration outbox.

Customer copy displays the line name and source-to-destination direction with
the existing price, quota, bandwidth, duration, and resource mode. Purchase
still means configuration requested; it does not mean Agent-applied or healthy.

## Migration and compatibility

Migration v27 creates the offering table and adds the nullable offering foreign
key to immutable price versions. No existing subscription, usage row, ledger
entry, or price version is changed. Existing unbound versions remain visible to
platform administrators with a legacy/unbound label and remain usable by their
already purchased periods, but are excluded from customer catalogs and new
purchases.

## Non-goals

- No cloud-provider API, automatic node purchase, release, or spare-node logic.
- No pending dedicated-node order or delayed billing activation.
- No automatic package or price creation for the existing Hong Kong node.
- No aggregate shared-node oversubscription policy or SLA claim.
- No early replacement, queued renewal, rollover, proration, or overage.
- No production deployment or production database mutation in this increment.

## Verification boundary

Automated tests cover migration preservation, administrator authorization,
state transitions and revision conflicts, exact-node catalog filtering,
resource-mode derivation, legacy version exclusion, forged purchase rollback,
concurrent pause-versus-purchase serialization, strict HTTP input, and customer
and administrator presentation. Real Hong Kong route quality, capacity, Agent
application, production catalog creation, and customer protocol health remain
environment work and are not claimed by this increment.
