CREATE TABLE platform_node_visibility (
 node_id text PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
 hidden boolean NOT NULL,
 revision bigint NOT NULL CHECK (revision>=2),
 updated_at timestamptz NOT NULL DEFAULT now()
);
