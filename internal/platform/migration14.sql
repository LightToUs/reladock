-- Latest generation-bound observations, not end-to-end health assertions.
CREATE TABLE probe_latest (
 rule_id uuid NOT NULL,
 tenant_id uuid NOT NULL,
 node_id text NOT NULL REFERENCES nodes(id),
 generation bigint NOT NULL CHECK (generation>0),
 probe_type text NOT NULL CHECK (probe_type IN ('tcp','udp')),
 status text NOT NULL,
 observed_at timestamptz NOT NULL,
 observed_ns bigint NOT NULL CHECK (observed_ns>0),
 latency_ms double precision,
 report_hash text NOT NULL CHECK (length(report_hash)=64),
 received_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(rule_id,probe_type),
 FOREIGN KEY(rule_id,tenant_id) REFERENCES relay_rules(id,tenant_id),
 FOREIGN KEY(node_id,generation) REFERENCES config_outbox(node_id,generation),
 CHECK (
  (probe_type='udp' AND status='unknown' AND latency_ms IS NULL) OR
  (probe_type='tcp' AND status IN ('targetReachable','targetUnreachable')
   AND latency_ms IS NOT NULL AND latency_ms BETWEEN 0 AND 11000)
 )
);
CREATE INDEX probe_latest_tenant_rule ON probe_latest(tenant_id,rule_id);
