# Node Resource Observability and Drift Design

## Intent

Give platform operators a bounded, truthful view of whether an enrolled entry
node is under resource pressure or its owned nftables/tc state no longer matches
the last durably applied configuration. This supplements Agent contact,
configuration ACKs and target probes; it never turns them into an end-to-end
health claim.

## Goals

- Collect a fixed Linux resource sample on each 30-second Agent heartbeat:
  CPU, memory, root filesystem and conntrack utilization.
- Report Agent clock time and classify sustained clock skew at the control plane.
- In apply mode, run the existing read-only composed dataplane verifier and
  report `in-sync`, `drifted` or `unknown`; validation mode reports
  `not-applicable`.
- Persist only the latest authenticated sample and server-maintained condition
  start times. Do not retain a high-cardinality time series in PostgreSQL.
- Open incidents only after a condition is sustained, and resolve them when the
  latest fresh sample recovers.
- Show the latest bounded facts in the platform node list and expose aggregate,
  low-cardinality Prometheus counts.

## Non-goals

- No automatic repair, service restart, firewall modification or node fencing.
- No per-node Prometheus labels, raw command output, process lists, interface
  names, IP addresses, paths or customer/rule identifiers.
- No capacity promise, client-view health assertion or UDP end-to-end claim.
- No long-term telemetry warehouse, charts or external monitoring deployment.

## Agent sample

`ResourceObservation` is embedded in the existing authenticated heartbeat. It
contains `sampledAt`, nullable integer percentages for CPU, memory, root disk and
conntrack, and a fixed `dataplaneState` enum. Percentage values are whole numbers
from 0 through 100. Missing Linux counters remain null and do not fail the
heartbeat. The sample contains no free-form error string.

CPU uses the delta between consecutive `/proc/stat` reads held in memory; the
first sample is unknown. Memory uses `MemAvailable` against `MemTotal`. Root disk
uses `statfs("/")`. Conntrack uses the kernel count and maximum files when both
are available. Collection uses direct files/syscalls, not a shell command.

In apply mode the Agent calls the existing read-only `Adapter.Stable` check with
a short timeout. A successful check is `in-sync`; a completed verification error
is `drifted`; timeout/cancellation is `unknown`. The check never repairs state.

## Persistence and validation

Migration 24 creates one `node_resource_latest` row per node. The control plane
validates the enum, percentages and sample time (not zero, no more than one
minute in the future and no more than seven days old). It computes absolute
clock skew from the database clock and never trusts an Agent-provided skew.

The row stores latest values, receive time and condition-since timestamps. A
condition timestamp is preserved while continuously unhealthy and cleared on a
healthy or unknown value. Re-enrollment, certificate revocation and final
retirement delete the row so a previous identity cannot contribute current
facts.

## Thresholds and incidents

- CPU: at least 90% for 5 minutes (`resource-cpu-high`).
- Memory: at least 90% for 5 minutes (`resource-memory-high`).
- Root disk: at least 85% for 5 minutes (`resource-disk-high`).
- Conntrack: at least 80% for 5 minutes (`resource-conntrack-high`).
- Absolute clock skew: more than 120 seconds for 2 minutes
  (`resource-clock-skew`).
- Dataplane state `drifted` for 2 minutes (`dataplane-drift`).

Only enrolled, non-retired nodes with a latest sample received within 90 seconds
are eligible for these incidents. Unknown values resolve a resource-specific
incident because the condition is no longer established; stale contact is
handled independently by `contact-stale`. Maintenance does not hide resource or
drift incidents because existing traffic may still run there. Reconciliation
uses the existing per-node transaction, audit and notification-outbox path.

## Presentation and metrics

The admin node API returns the latest observation, its receive time, absolute
clock skew and a derived `resourceStatus` of `normal`, `pressure`, `drifted`,
`unknown` or `stale`. The UI shows this as a separate “节点资源” column with
compact percentages and explicitly says it is not end-to-end health.

Operational metrics add aggregate node-resource status counts and the six new
fixed incident kinds. No node ID is emitted as a metric label.

## Security and failure behavior

The existing mTLS identity, body limit, JSON unknown-field rejection and request
rate limit protect the heartbeat endpoint. Database writes remain atomic with
the heartbeat. Invalid observations reject the request without changing either
heartbeat or resource state. Collection failures produce null/unknown fields,
not secrets or diagnostic text. A database or verifier failure cannot be
reported as healthy.

## Verification boundary

Unit tests cover collectors and drift classification with controlled inputs.
PostgreSQL tests cover validation, identity reset, sustained thresholds,
recovery and incident idempotency. HTTP tests cover authenticated ingestion.
Admin and metrics tests cover bounded output. Real Linux pressure, real
nftables/tc drift and Prometheus alert delivery remain production acceptance
work and must be reported as unverified until exercised on isolated VPS hosts.
