-- A request targets one current identity, not all future certificates.
ALTER TABLE nodes ADD COLUMN certificate_renewal_fingerprint text;
ALTER TABLE nodes ADD COLUMN agent_certificate_rotation_supported boolean NOT NULL DEFAULT false;
ALTER TABLE nodes ADD CONSTRAINT nodes_certificate_renewal_current CHECK (
 certificate_renewal_fingerprint IS NULL OR
 (certificate_fingerprint IS NOT NULL AND certificate_renewal_fingerprint = certificate_fingerprint
  AND certificate_renewal_fingerprint ~ '^[a-f0-9]{64}$')
);

-- Cover every identity replacement, including bootstrap, local enrollment and
-- retirement, without allowing an old request to follow a replacement identity.
CREATE FUNCTION clear_node_certificate_renewal() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.certificate_fingerprint IS DISTINCT FROM OLD.certificate_fingerprint THEN
  NEW.certificate_renewal_fingerprint := NULL;
  NEW.agent_certificate_rotation_supported := false;
 END IF;
 RETURN NEW;
END;
$$;
CREATE TRIGGER clear_node_certificate_renewal
 BEFORE UPDATE OF certificate_fingerprint ON nodes
 FOR EACH ROW EXECUTE FUNCTION clear_node_certificate_renewal();
