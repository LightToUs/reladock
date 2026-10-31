ALTER TABLE nodes
 ADD COLUMN agent_heartbeat_at timestamptz,
 ADD COLUMN agent_mode text NOT NULL DEFAULT '' CHECK (agent_mode IN ('','validation','apply')),
 ADD COLUMN agent_started_at timestamptz,
 ADD COLUMN agent_certificate_not_after timestamptz,
 ADD COLUMN agent_rotation_state text NOT NULL DEFAULT 'idle' CHECK (agent_rotation_state IN ('idle','pending','error')),
 ADD COLUMN agent_rotation_error_code text NOT NULL DEFAULT '' CHECK (agent_rotation_error_code IN ('','state-invalid','key-mismatch','certificate-invalid','publish-failed','renew-unavailable','activate-unavailable')),
 ADD COLUMN agent_rotation_error_since timestamptz,
 ADD CONSTRAINT nodes_agent_rotation_consistent CHECK (
  (agent_rotation_state='error' AND agent_rotation_error_code<>'' AND agent_rotation_error_since IS NOT NULL) OR
  (agent_rotation_state<>'error' AND agent_rotation_error_code='' AND agent_rotation_error_since IS NULL)
 );

CREATE TABLE node_certificate_rotations (
 node_id text PRIMARY KEY REFERENCES nodes(id),
 csr_hash text NOT NULL CHECK (csr_hash ~ '^[a-f0-9]{64}$'),
 certificate bytea NOT NULL CHECK (octet_length(certificate) BETWEEN 1 AND 32768),
 fingerprint text NOT NULL UNIQUE CHECK (fingerprint ~ '^[a-f0-9]{64}$'),
 not_after timestamptz NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now()
);
