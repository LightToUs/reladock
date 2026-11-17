ALTER TABLE nodes ADD COLUMN IF NOT EXISTS agent_source_ip_block_supported boolean;
UPDATE nodes SET agent_source_ip_block_supported=false WHERE agent_source_ip_block_supported IS NULL;
ALTER TABLE nodes ALTER COLUMN agent_source_ip_block_supported SET DEFAULT false;
ALTER TABLE nodes ALTER COLUMN agent_source_ip_block_supported SET NOT NULL;

CREATE OR REPLACE FUNCTION clear_node_certificate_renewal() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.certificate_fingerprint IS DISTINCT FROM OLD.certificate_fingerprint THEN
  NEW.certificate_renewal_fingerprint := NULL;
  NEW.agent_certificate_rotation_supported := false;
  NEW.agent_source_flow_supported := false;
  NEW.agent_port_range_supported := false;
  NEW.agent_source_ip_block_supported := false;
 END IF;
 RETURN NEW;
END;
$$;

