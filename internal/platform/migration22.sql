ALTER TABLE endpoint_allocations
 ADD COLUMN allocation_id uuid NOT NULL DEFAULT gen_random_uuid(),
 ADD CONSTRAINT endpoint_allocations_allocation_id_key UNIQUE (allocation_id);

CREATE FUNCTION endpoint_allocation_identity_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.allocation_id <> OLD.allocation_id THEN
  RAISE EXCEPTION 'allocation identity is immutable' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END;
$$;
CREATE TRIGGER endpoint_allocation_identity_immutable
 BEFORE UPDATE ON endpoint_allocations
 FOR EACH ROW EXECUTE FUNCTION endpoint_allocation_identity_immutable();

CREATE TABLE endpoint_retirements (
 id uuid PRIMARY KEY,
 allocation_id uuid NOT NULL UNIQUE,
 node_id text NOT NULL REFERENCES nodes(id),
 rule_id uuid NOT NULL,
 tenant_id uuid NOT NULL,
 listen_address inet NOT NULL,
 protocol text NOT NULL CHECK (protocol IN ('tcp','udp')),
 listen_port integer NOT NULL CHECK (listen_port BETWEEN 20000 AND 60000),
 quarantine_generation bigint NOT NULL DEFAULT 0 CHECK (quarantine_generation>=0),
 state text NOT NULL CHECK (state IN ('waiting-removal','queued','claimed','purging','released','failed')),
 attempts integer NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 5),
 lease_token_hash text CHECK (lease_token_hash IS NULL OR lease_token_hash ~ '^[a-f0-9]{64}$'),
 lease_expires_at timestamptz,
 next_attempt_at timestamptz NOT NULL DEFAULT now(),
 error_code text NOT NULL DEFAULT '' CHECK (error_code IN ('','tool-missing','tool-version','permission-denied','guard-missing','guard-mismatch','state-mismatch','output-invalid','query-nonzero','io-failed','internal-error')),
 proof jsonb,
 proof_digest text CHECK (proof_digest IS NULL OR proof_digest ~ '^[a-f0-9]{64}$'),
 created_at timestamptz NOT NULL DEFAULT now(),
 claimed_at timestamptz,
 purging_at timestamptz,
 reported_at timestamptz,
 released_at timestamptz,
 quarantine_consumed_at timestamptz,
 consumed_by_allocation_id uuid,
 FOREIGN KEY(rule_id,tenant_id) REFERENCES relay_rules(id,tenant_id),
 CHECK ((lease_token_hash IS NULL) = (lease_expires_at IS NULL)),
 CHECK (state NOT IN ('claimed','purging') OR lease_token_hash IS NOT NULL),
 CHECK ((state='released') = (proof IS NOT NULL AND proof_digest IS NOT NULL AND released_at IS NOT NULL)),
 CHECK ((quarantine_consumed_at IS NULL) = (consumed_by_allocation_id IS NULL)),
 CHECK (quarantine_consumed_at IS NULL OR state='released')
);
CREATE UNIQUE INDEX endpoint_retirements_one_executor_per_node
 ON endpoint_retirements(node_id) WHERE state IN ('claimed','purging');
CREATE INDEX endpoint_retirements_node_state
 ON endpoint_retirements(node_id,state,next_attempt_at,created_at);
CREATE INDEX endpoint_retirements_rule
 ON endpoint_retirements(rule_id,created_at);
CREATE INDEX endpoint_retirements_unconsumed
 ON endpoint_retirements(node_id,released_at) WHERE state='released' AND quarantine_consumed_at IS NULL;
CREATE UNIQUE INDEX endpoint_retirements_one_unconsumed_tuple
 ON endpoint_retirements(node_id,listen_address,protocol,listen_port)
 WHERE state='released' AND quarantine_consumed_at IS NULL;

ALTER TABLE endpoint_allocations
 ADD COLUMN predecessor_retirement_id uuid REFERENCES endpoint_retirements(id);
CREATE UNIQUE INDEX endpoint_allocations_predecessor_unique
 ON endpoint_allocations(predecessor_retirement_id) WHERE predecessor_retirement_id IS NOT NULL;

INSERT INTO endpoint_retirements(id,allocation_id,node_id,rule_id,tenant_id,listen_address,protocol,listen_port,quarantine_generation,state)
SELECT gen_random_uuid(),e.allocation_id,e.node_id,e.rule_id,e.tenant_id,e.listen_address,e.protocol,e.listen_port,GREATEST(r.delivery_generation,0),'waiting-removal'
FROM endpoint_allocations e
JOIN relay_rules r ON r.id=e.rule_id AND r.tenant_id=e.tenant_id
WHERE r.status='deleted';

ALTER TABLE nodes DROP CONSTRAINT nodes_status_check;
ALTER TABLE nodes ADD CONSTRAINT nodes_status_check
 CHECK (status IN ('unprovisioned','ready','maintenance','draining','retired'));

CREATE TABLE node_drain_attestations (
 id uuid PRIMARY KEY,
 node_id text NOT NULL REFERENCES nodes(id),
 generation bigint NOT NULL CHECK (generation>=0),
 certificate_fingerprint text NOT NULL CHECK (certificate_fingerprint ~ '^[a-f0-9]{64}$'),
 kernel_boot_id uuid NOT NULL,
 agent_started_at timestamptz NOT NULL,
 agent_version text NOT NULL CHECK (length(agent_version) BETWEEN 1 AND 80),
 journal_sequence bigint NOT NULL CHECK (journal_sequence>=0),
 good_digest text NOT NULL CHECK (good_digest ~ '^[a-f0-9]{64}$'),
 quarantine_digest text NOT NULL CHECK (quarantine_digest ~ '^[a-f0-9]{64}$'),
 observed_at timestamptz NOT NULL,
 received_at timestamptz NOT NULL DEFAULT now(),
 evidence jsonb NOT NULL CHECK (octet_length(evidence::text)<=65536),
 consumed_at timestamptz
);
CREATE INDEX node_drain_attestations_node
 ON node_drain_attestations(node_id,received_at DESC,id DESC);
