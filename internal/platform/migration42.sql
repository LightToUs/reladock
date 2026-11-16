ALTER TABLE relay_rules ADD COLUMN IF NOT EXISTS listen_port_end integer;
UPDATE relay_rules SET listen_port_end=listen_port WHERE listen_port_end IS NULL;
ALTER TABLE relay_rules ALTER COLUMN listen_port_end SET NOT NULL;
ALTER TABLE relay_rules DROP CONSTRAINT IF EXISTS relay_rules_listen_port_check;
ALTER TABLE relay_rules DROP CONSTRAINT IF EXISTS relay_rules_listen_port_range_check;
ALTER TABLE relay_rules
 ADD CONSTRAINT relay_rules_listen_port_check CHECK (listen_port BETWEEN 1 AND 65535),
 ADD CONSTRAINT relay_rules_listen_port_range_check CHECK (listen_port_end BETWEEN listen_port AND 65535 AND listen_port_end-listen_port+1<=4096);

-- Keep one rolling-deploy compatibility window for an older controller that
-- still inserts only listen_port. New code always writes both bounds.
CREATE OR REPLACE FUNCTION default_rule_listen_port_end() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.listen_port_end IS NULL THEN
  NEW.listen_port_end := NEW.listen_port;
 END IF;
 RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS default_rule_listen_port_end ON relay_rules;
CREATE TRIGGER default_rule_listen_port_end
 BEFORE INSERT OR UPDATE OF listen_port ON relay_rules
 FOR EACH ROW EXECUTE FUNCTION default_rule_listen_port_end();

ALTER TABLE endpoint_allocations DROP CONSTRAINT IF EXISTS endpoint_allocations_listen_port_check;
ALTER TABLE endpoint_allocations
 ADD CONSTRAINT endpoint_allocations_listen_port_check CHECK (listen_port BETWEEN 1 AND 65535);

ALTER TABLE endpoint_retirements DROP CONSTRAINT IF EXISTS endpoint_retirements_listen_port_check;
ALTER TABLE endpoint_retirements
 ADD CONSTRAINT endpoint_retirements_listen_port_check CHECK (listen_port BETWEEN 1 AND 65535);

ALTER TABLE nodes ADD COLUMN IF NOT EXISTS agent_port_range_supported boolean;
UPDATE nodes SET agent_port_range_supported=false WHERE agent_port_range_supported IS NULL;
ALTER TABLE nodes ALTER COLUMN agent_port_range_supported SET DEFAULT false;
ALTER TABLE nodes ALTER COLUMN agent_port_range_supported SET NOT NULL;

-- Capabilities belong to one authenticated certificate identity and must not
-- survive enrollment, rotation, manual replacement, or retirement.
CREATE OR REPLACE FUNCTION clear_node_certificate_renewal() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.certificate_fingerprint IS DISTINCT FROM OLD.certificate_fingerprint THEN
  NEW.certificate_renewal_fingerprint := NULL;
  NEW.agent_certificate_rotation_supported := false;
  NEW.agent_source_flow_supported := false;
  NEW.agent_port_range_supported := false;
 END IF;
 RETURN NEW;
END;
$$;

CREATE TABLE IF NOT EXISTS node_customer_port_pools (
 node_id text NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
 protocol text NOT NULL,
 port_start integer NOT NULL,
 port_end integer NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(node_id,protocol,port_start,port_end)
);

ALTER TABLE node_customer_port_pools DROP CONSTRAINT IF EXISTS node_customer_port_pools_check;
ALTER TABLE node_customer_port_pools DROP CONSTRAINT IF EXISTS node_customer_port_pools_protocol_check;
ALTER TABLE node_customer_port_pools DROP CONSTRAINT IF EXISTS node_customer_port_pools_port_start_check;
ALTER TABLE node_customer_port_pools DROP CONSTRAINT IF EXISTS node_customer_port_pools_port_end_check;
ALTER TABLE node_customer_port_pools DROP CONSTRAINT IF EXISTS node_customer_port_pools_port_range_check;
ALTER TABLE node_customer_port_pools
 ADD CONSTRAINT node_customer_port_pools_protocol_check CHECK (protocol IN ('tcp','udp')),
 ADD CONSTRAINT node_customer_port_pools_port_range_check CHECK (port_start BETWEEN 1 AND 65535 AND port_end BETWEEN port_start AND 65535);

CREATE OR REPLACE FUNCTION seed_node_customer_port_pools() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 INSERT INTO node_customer_port_pools(node_id,protocol,port_start,port_end)
 VALUES(NEW.id,'tcp',20000,60000),(NEW.id,'udp',20000,60000);
 RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS seed_node_customer_port_pools ON nodes;
CREATE TRIGGER seed_node_customer_port_pools
 AFTER INSERT ON nodes
 FOR EACH ROW EXECUTE FUNCTION seed_node_customer_port_pools();

INSERT INTO node_customer_port_pools(node_id,protocol,port_start,port_end)
SELECT n.id,p.protocol,20000,60000
FROM nodes n CROSS JOIN (VALUES('tcp'),('udp')) AS p(protocol)
ON CONFLICT DO NOTHING;
