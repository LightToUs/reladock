ALTER TABLE nodes ADD COLUMN region text NOT NULL DEFAULT 'Hong Kong';
ALTER TABLE nodes ADD COLUMN desired_generation bigint NOT NULL DEFAULT 0;
ALTER TABLE nodes ADD COLUMN validated_generation bigint NOT NULL DEFAULT 0;
ALTER TABLE nodes ADD COLUMN last_seen_at timestamptz;
ALTER TABLE nodes ADD COLUMN agent_version text NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN certificate_fingerprint text UNIQUE;
ALTER TABLE relay_rules ADD COLUMN delivery_generation bigint NOT NULL DEFAULT 0;
CREATE TABLE config_outbox (
 id uuid PRIMARY KEY,
 node_id text NOT NULL REFERENCES nodes(id),
 generation bigint NOT NULL CHECK (generation>0),
 payload bytea NOT NULL,
 checksum text NOT NULL,
 state text NOT NULL CHECK (state IN ('queued','leased','validated','failed','superseded')),
 attempts integer NOT NULL DEFAULT 0,
 lease_token text,
 lease_until timestamptz,
 next_attempt_at timestamptz NOT NULL DEFAULT now(),
 error_code text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT now(),
 reported_at timestamptz,
 UNIQUE(node_id,generation)
);
CREATE INDEX outbox_node_state ON config_outbox(node_id,state,generation DESC);
CREATE TABLE node_operator_events (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 node_id text NOT NULL REFERENCES nodes(id),
 action text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now()
);
