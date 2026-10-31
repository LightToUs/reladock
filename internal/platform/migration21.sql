CREATE TABLE node_agent_upgrades (
 id uuid PRIMARY KEY,
 node_id text NOT NULL REFERENCES nodes(id),
 target_version text NOT NULL CHECK (target_version ~ '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'),
 amd64_sha256 text NOT NULL CHECK (amd64_sha256 ~ '^[a-f0-9]{64}$'),
 arm64_sha256 text NOT NULL CHECK (arm64_sha256 ~ '^[a-f0-9]{64}$'),
 previous_version text NOT NULL,
 state text NOT NULL CHECK (state IN ('queued','claimed','installing','verifying','succeeded','failed','rolled-back','cancelled')),
 claimed_arch text CHECK (claimed_arch IS NULL OR claimed_arch IN ('amd64','arm64')),
 lease_token_hash text CHECK (lease_token_hash IS NULL OR lease_token_hash ~ '^[a-f0-9]{64}$'),
 lease_expires_at timestamptz,
 error_code text NOT NULL DEFAULT '' CHECK (error_code IN ('','download-unavailable','archive-invalid','checksum-mismatch','install-failed','service-failed','health-timeout','rollback-failed','state-invalid')),
 requested_at timestamptz NOT NULL DEFAULT now(),
 claimed_at timestamptz,
 reported_at timestamptz,
 updated_at timestamptz NOT NULL DEFAULT now(),
 completed_at timestamptz,
 CHECK ((state='queued' AND lease_token_hash IS NULL AND lease_expires_at IS NULL AND claimed_arch IS NULL) OR state<>'queued'),
 CHECK ((state IN ('succeeded','failed','rolled-back','cancelled')) = (completed_at IS NOT NULL))
);

CREATE UNIQUE INDEX node_agent_upgrades_one_active_global
 ON node_agent_upgrades ((true))
 WHERE state IN ('queued','claimed','installing','verifying');
CREATE UNIQUE INDEX node_agent_upgrades_one_active_node
 ON node_agent_upgrades (node_id)
 WHERE state IN ('queued','claimed','installing','verifying');
CREATE INDEX node_agent_upgrades_node_history ON node_agent_upgrades(node_id,requested_at DESC,id DESC);
