CREATE TABLE node_bootstrap (
 token_hash text PRIMARY KEY,
 node_id text NOT NULL REFERENCES nodes(id),
 expires_at timestamptz NOT NULL,
 revoked boolean NOT NULL DEFAULT false,
 csr_hash text,
 certificate bytea,
 fingerprint text,
 created_at timestamptz NOT NULL DEFAULT now(),
 CHECK ((csr_hash IS NULL AND certificate IS NULL AND fingerprint IS NULL) OR
        (csr_hash IS NOT NULL AND certificate IS NOT NULL AND fingerprint IS NOT NULL))
);
CREATE INDEX node_bootstrap_node ON node_bootstrap(node_id);
CREATE TABLE node_preflight (
 node_id text PRIMARY KEY REFERENCES nodes(id),
 report jsonb NOT NULL,
 reported_at timestamptz NOT NULL DEFAULT now()
);
