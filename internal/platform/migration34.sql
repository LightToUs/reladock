ALTER TABLE relay_rules
 ADD COLUMN source_logging_enabled boolean NOT NULL DEFAULT false,
 ADD COLUMN source_logging_applied_enabled boolean NOT NULL DEFAULT false,
 ADD COLUMN source_logging_applied_version bigint NOT NULL DEFAULT 0 CHECK (source_logging_applied_version>=0),
 ADD COLUMN source_logging_applied_generation bigint NOT NULL DEFAULT 0 CHECK (source_logging_applied_generation>=0);

ALTER TABLE nodes
 ADD COLUMN agent_source_flow_supported boolean NOT NULL DEFAULT false;

-- Agent capabilities are assertions by one authenticated certificate identity.
-- Never carry them across enrollment, certificate replacement, or retirement.
CREATE OR REPLACE FUNCTION clear_node_certificate_renewal() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.certificate_fingerprint IS DISTINCT FROM OLD.certificate_fingerprint THEN
  NEW.certificate_renewal_fingerprint := NULL;
  NEW.agent_certificate_rotation_supported := false;
  NEW.agent_source_flow_supported := false;
 END IF;
 RETURN NEW;
END;
$$;

CREATE TABLE node_source_flow_latest (
 node_id text PRIMARY KEY REFERENCES nodes(id),
 collector_status text NOT NULL CHECK (collector_status IN ('normal','degraded','unavailable')),
 subscription_alive boolean NOT NULL,
 backlog_count bigint NOT NULL CHECK (backlog_count>=0),
 oldest_backlog_at timestamptz,
 wal_bytes_used bigint NOT NULL CHECK (wal_bytes_used>=0),
 wal_bytes_limit bigint NOT NULL CHECK (wal_bytes_limit>0),
 dropped_total bigint NOT NULL CHECK (dropped_total>=0),
 open_gap boolean NOT NULL,
 collector_epoch uuid NOT NULL,
 collector_sequence bigint NOT NULL CHECK (collector_sequence>=0),
 observed_at timestamptz NOT NULL,
 received_at timestamptz NOT NULL DEFAULT now(),
 CHECK ((backlog_count=0) = (oldest_backlog_at IS NULL))
);

CREATE TABLE applied_source_flow_rules (
 node_id text NOT NULL REFERENCES nodes(id),
 config_generation bigint NOT NULL CHECK (config_generation>0),
 tenant_id uuid NOT NULL REFERENCES tenants(id),
 rule_id uuid NOT NULL,
 allocation_id uuid NOT NULL,
 rule_version bigint NOT NULL CHECK (rule_version>0),
 source_logging_enabled boolean NOT NULL,
 protocol text NOT NULL CHECK (protocol IN ('tcp','udp')),
 relay_ip inet NOT NULL,
 relay_port integer NOT NULL CHECK (relay_port BETWEEN 1 AND 65535),
 target_ip inet NOT NULL,
 target_port integer NOT NULL CHECK (target_port BETWEEN 1 AND 65535),
 manifest_digest text NOT NULL CHECK (manifest_digest ~ '^[a-f0-9]{64}$'),
 applied_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(node_id,config_generation,protocol,relay_ip,relay_port),
 UNIQUE(node_id,config_generation,allocation_id),
 FOREIGN KEY(rule_id,tenant_id) REFERENCES relay_rules(id,tenant_id)
);
CREATE INDEX applied_source_flow_rules_rule_generation
 ON applied_source_flow_rules(rule_id,config_generation DESC);

CREATE TABLE source_flow_events (
 node_id text NOT NULL REFERENCES nodes(id),
 event_id uuid NOT NULL,
 tenant_id uuid NOT NULL REFERENCES tenants(id),
 rule_id uuid NOT NULL,
 allocation_id uuid NOT NULL,
 config_generation bigint NOT NULL CHECK (config_generation>0),
 rule_version bigint NOT NULL CHECK (rule_version>0),
 protocol text NOT NULL CHECK (protocol IN ('tcp','udp')),
 source_ip inet NOT NULL,
 source_port integer NOT NULL CHECK (source_port BETWEEN 1 AND 65535),
 relay_ip inet NOT NULL,
 relay_port integer NOT NULL CHECK (relay_port BETWEEN 1 AND 65535),
 target_ip inet NOT NULL,
 target_port integer NOT NULL CHECK (target_port BETWEEN 1 AND 65535),
 observed_at timestamptz NOT NULL,
 received_at timestamptz NOT NULL DEFAULT now(),
 kernel_boot_id uuid NOT NULL,
 conntrack_zone integer NOT NULL CHECK (conntrack_zone BETWEEN 0 AND 65535),
 conntrack_id bigint NOT NULL CHECK (conntrack_id BETWEEN 0 AND 4294967295),
 PRIMARY KEY(node_id,event_id),
 FOREIGN KEY(rule_id,tenant_id) REFERENCES relay_rules(id,tenant_id)
);
CREATE INDEX source_flow_events_rule_observed
 ON source_flow_events(rule_id,observed_at DESC,event_id DESC);
CREATE INDEX source_flow_events_source_observed
 ON source_flow_events(source_ip,observed_at DESC,event_id DESC);
CREATE INDEX source_flow_events_node_observed
 ON source_flow_events(node_id,observed_at DESC,event_id DESC);
CREATE INDEX source_flow_events_received
 ON source_flow_events(received_at,event_id);

CREATE TABLE source_flow_gaps (
 node_id text NOT NULL REFERENCES nodes(id),
 collector_epoch uuid NOT NULL,
 sequence bigint NOT NULL CHECK (sequence>=0),
 reason text NOT NULL CHECK (reason IN (
  'netlink-overrun','collector-restart','collector-error',
  'queue-full','wal-full','wal-unavailable'
 )),
 dropped_count bigint NOT NULL CHECK (dropped_count>=0),
 opened_at timestamptz NOT NULL,
 closed_at timestamptz,
 received_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(node_id,collector_epoch,sequence),
 CHECK (closed_at IS NULL OR closed_at>=opened_at)
);
CREATE INDEX source_flow_gaps_node_window
 ON source_flow_gaps(node_id,opened_at DESC,closed_at);
CREATE INDEX source_flow_gaps_received
 ON source_flow_gaps(received_at,node_id,collector_epoch,sequence);
