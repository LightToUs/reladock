-- Generic Lightsail script nodes are intentionally separate from relay nodes.
CREATE TABLE lightsail_instances (
 id uuid PRIMARY KEY,
 created_by uuid NOT NULL REFERENCES members(id),
 idempotency_key text NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 200),
 request_hash text NOT NULL CHECK (request_hash ~ '^[a-f0-9]{64}$'),
 name text NOT NULL CHECK (length(name) BETWEEN 1 AND 64),
 region text NOT NULL CHECK (region ~ '^[a-z]{2}(-[a-z]+)+-[0-9]+$'),
 availability_zone text NOT NULL CHECK (length(availability_zone) BETWEEN 3 AND 32),
 blueprint_id text NOT NULL CHECK (length(blueprint_id) BETWEEN 1 AND 255),
 bundle_id text NOT NULL CHECK (length(bundle_id) BETWEEN 1 AND 255),
 aws_instance_name text NOT NULL UNIQUE CHECK (aws_instance_name ~ '^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$'),
 aws_arn text NOT NULL DEFAULT '' CHECK (length(aws_arn)<=2048),
 static_ip_name text NOT NULL UNIQUE CHECK (static_ip_name ~ '^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$'),
 static_ip_address inet,
 aws_state text NOT NULL DEFAULT '' CHECK (length(aws_state)<=40),
 lifecycle_state text NOT NULL CHECK (lifecycle_state IN ('requested','provisioning','running','stopping','stopped','destroying','destroyed','manual-review','error')),
 deployment_state text NOT NULL CHECK (deployment_state IN ('pending','running','succeeded','failed','unknown')),
 script_sha256 text NOT NULL CHECK (script_sha256 ~ '^[a-f0-9]{64}$'),
 script_size integer NOT NULL CHECK (script_size BETWEEN 1 AND 65536),
 script_body text,
 execution_started_at timestamptz,
 create_operation_id text NOT NULL DEFAULT '' CHECK (length(create_operation_id)<=255),
 firewall_operation_id text NOT NULL DEFAULT '' CHECK (length(firewall_operation_id)<=255),
 static_ip_operation_id text NOT NULL DEFAULT '' CHECK (length(static_ip_operation_id)<=255),
 stop_operation_id text NOT NULL DEFAULT '' CHECK (length(stop_operation_id)<=255),
 delete_operation_id text NOT NULL DEFAULT '' CHECK (length(delete_operation_id)<=255),
 host_key_sha256 text[] NOT NULL DEFAULT '{}' CHECK (cardinality(host_key_sha256)<=16),
 execution_output text NOT NULL DEFAULT '' CHECK (octet_length(execution_output)<=32768),
 exit_code integer,
 error_code text NOT NULL DEFAULT '' CHECK (length(error_code)<=120),
 revision bigint NOT NULL DEFAULT 1 CHECK (revision>=1),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 destroyed_at timestamptz,
 UNIQUE(created_by,idempotency_key),
 CHECK ((execution_started_at IS NULL AND script_body IS NOT NULL) OR (execution_started_at IS NOT NULL AND script_body IS NULL)),
 CHECK (script_body IS NULL OR octet_length(script_body)=script_size),
 CHECK (deployment_state<>'running' OR execution_started_at IS NOT NULL),
 CHECK ((lifecycle_state='destroyed')=(destroyed_at IS NOT NULL))
);

CREATE FUNCTION protect_lightsail_create_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.created_by<>OLD.created_by OR NEW.idempotency_key<>OLD.idempotency_key OR NEW.request_hash<>OLD.request_hash
    OR NEW.name<>OLD.name OR NEW.region<>OLD.region OR NEW.availability_zone<>OLD.availability_zone
    OR NEW.blueprint_id<>OLD.blueprint_id OR NEW.bundle_id<>OLD.bundle_id
    OR NEW.aws_instance_name<>OLD.aws_instance_name OR NEW.static_ip_name<>OLD.static_ip_name
    OR NEW.script_sha256<>OLD.script_sha256 OR NEW.script_size<>OLD.script_size THEN
  RAISE EXCEPTION 'Lightsail create identity is immutable';
 END IF;
 IF OLD.execution_started_at IS NOT NULL AND (NEW.execution_started_at IS DISTINCT FROM OLD.execution_started_at OR NEW.script_body IS NOT NULL) THEN
  RAISE EXCEPTION 'Lightsail execution boundary is immutable';
 END IF;
 RETURN NEW;
END;
$$;

CREATE TRIGGER protect_lightsail_create_identity
 BEFORE UPDATE ON lightsail_instances
 FOR EACH ROW EXECUTE FUNCTION protect_lightsail_create_identity();

CREATE TABLE lightsail_jobs (
 id uuid PRIMARY KEY,
 instance_id uuid NOT NULL REFERENCES lightsail_instances(id),
 kind text NOT NULL CHECK (kind IN ('create','stop','destroy')),
 state text NOT NULL CHECK (state IN ('queued','leased','retrying','succeeded','failed','unknown','manual-review')),
 stage text NOT NULL CHECK (length(stage) BETWEEN 1 AND 80),
 lease_token uuid,
 lease_expires_at timestamptz,
 attempts integer NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 100),
 next_attempt_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 provider_operation_id text NOT NULL DEFAULT '' CHECK (length(provider_operation_id)<=255),
 error_code text NOT NULL DEFAULT '' CHECK (length(error_code)<=120),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 completed_at timestamptz,
 CHECK ((lease_token IS NULL)=(lease_expires_at IS NULL)),
 CHECK ((state='leased')=(lease_token IS NOT NULL)),
 CHECK ((state IN ('succeeded','failed','unknown','manual-review'))=(completed_at IS NOT NULL))
);

CREATE UNIQUE INDEX lightsail_jobs_one_active_per_instance
 ON lightsail_jobs(instance_id)
 WHERE state IN ('queued','leased','retrying');

CREATE INDEX lightsail_jobs_queue
 ON lightsail_jobs(next_attempt_at,created_at,id)
 WHERE state IN ('queued','leased','retrying');
