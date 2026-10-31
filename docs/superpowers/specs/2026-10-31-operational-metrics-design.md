# RelaDock operational metrics design

## Intent

RelaDock already persists Agent contact, delivery, incident, upgrade and retirement facts, but an operator cannot scrape them with standard infrastructure. Add a bounded Prometheus text endpoint for control-plane alerting without turning those facts into a claim that customer traffic or VPN protocols are healthy.

## Decisions

- A separate `METRICS_LISTEN_ADDRESS` listener is disabled by default and accepts only a literal loopback IP and non-zero port. It cannot equal the browser or Agent listener. It has no browser session, write route, payment route or public TLS mode; operators expose it only through their local collector.
- `GET /metrics` and `HEAD /metrics` are the only routes. Scrapes have a short database timeout and bounded headers. Database failure returns a valid Prometheus body with `reladock_database_up 0`, never stale cached success.
- Metrics have only fixed, low-cardinality labels. Tenant, member, rule, node, target, certificate, IP, error text and token values never become labels or output.
- Database snapshot families count node lifecycle state, Agent contact state, rule state, latest desired-generation delivery state, open incident kind, notification state, Agent upgrade state, endpoint-retirement state and frozen wallets. Contact uses the same five-minute threshold as incident reconciliation.
- Process counters record only fixed worker names and success/error outcomes for subscription/incident reconciliation and notification delivery. A counter does not prove a job's external effect; durable database state remains authoritative.
- Metric names avoid `healthy` and `up` except `reladock_database_up`. Agent recent contact, applied configuration and reachable target remain separate observations.
- No external Prometheus client dependency is added. Encoding is deterministic Prometheus 0.0.4 text using fixed names and escaped fixed labels.

## Failure behavior

- Invalid or public metrics addresses fail configuration validation before a database connection or listener opens.
- Unknown database enum values fail the snapshot instead of creating an unbounded label.
- A scrape query error emits database-down plus process counters and a scrape-error counter; it does not expose the SQL error.
- Metrics listener failure participates in normal process shutdown/failure handling. It does not silently disappear while the main service continues.

## Verification and documentation

Tests cover disabled/default configuration, loopback validation, listener conflicts, exact HTTP methods/paths/content type, secret/high-cardinality absence, deterministic database counts, database failure output, worker counter concurrency and graceful shutdown wiring. Deployment docs include scrape examples and conservative alert suggestions. `docs/PROBES.md` is corrected to reflect the existing Agent/platform integration while preserving its health boundaries.

Real Prometheus/Grafana deployment, alert routing, retention, capacity thresholds and customer protocol health remain environment acceptance work.
