CREATE TABLE tenants (
 id uuid PRIMARY KEY,
 name text NOT NULL CHECK (length(name) BETWEEN 1 AND 120),
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE members (
 id uuid PRIMARY KEY,
 tenant_id uuid NOT NULL REFERENCES tenants(id),
 email text NOT NULL UNIQUE,
 password_hash text NOT NULL,
 role text NOT NULL CHECK (role IN ('owner','admin','viewer')),
 created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(id,tenant_id)
);
CREATE TABLE sessions (
 token_hash text PRIMARY KEY,
 member_id uuid NOT NULL REFERENCES members(id),
 expires_at timestamptz NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sessions_expiry ON sessions(expires_at);
CREATE TABLE nodes (
 id text PRIMARY KEY,
 name text NOT NULL,
 status text NOT NULL CHECK (status IN ('unprovisioned','ready','maintenance')),
 listen_address inet NOT NULL
);
INSERT INTO nodes VALUES ('hk-01','香港 · HK-01','unprovisioned','0.0.0.0');
CREATE TABLE relay_rules (
 id uuid PRIMARY KEY,
 tenant_id uuid NOT NULL REFERENCES tenants(id),
 node_id text NOT NULL REFERENCES nodes(id),
 name text NOT NULL CHECK (length(name) BETWEEN 1 AND 80),
 protocol text NOT NULL CHECK (protocol IN ('tcp','udp','both')),
 target_host text NOT NULL,
 target_port integer NOT NULL CHECK (target_port BETWEEN 1 AND 65535),
 bandwidth_mbps integer NOT NULL CHECK (bandwidth_mbps BETWEEN 1 AND 1000),
 listen_port integer NOT NULL CHECK (listen_port BETWEEN 20000 AND 60000),
 status text NOT NULL CHECK (status IN ('pending-agent','paused','deleted')),
 desired_version bigint NOT NULL DEFAULT 1,
 applied_version bigint NOT NULL DEFAULT 0,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(id,tenant_id)
);
CREATE INDEX rules_tenant ON relay_rules(tenant_id,created_at);
CREATE TABLE endpoint_allocations (
 node_id text NOT NULL REFERENCES nodes(id),
 listen_address inet NOT NULL,
 protocol text NOT NULL CHECK (protocol IN ('tcp','udp')),
 listen_port integer NOT NULL CHECK (listen_port BETWEEN 20000 AND 60000),
 rule_id uuid NOT NULL,
 tenant_id uuid NOT NULL,
 PRIMARY KEY(node_id,listen_address,protocol,listen_port),
 FOREIGN KEY(rule_id,tenant_id) REFERENCES relay_rules(id,tenant_id)
);
CREATE TABLE idempotency_requests (
 tenant_id uuid NOT NULL REFERENCES tenants(id),
 key text NOT NULL CHECK (length(key) BETWEEN 8 AND 128),
 request_hash text NOT NULL,
 rule_id uuid NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id,key),
 FOREIGN KEY(rule_id,tenant_id) REFERENCES relay_rules(id,tenant_id)
);
CREATE TABLE audit_logs (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 tenant_id uuid NOT NULL REFERENCES tenants(id),
 actor_id uuid NOT NULL,
 action text NOT NULL,
 resource_id text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(actor_id,tenant_id) REFERENCES members(id,tenant_id)
);
CREATE INDEX audit_tenant ON audit_logs(tenant_id,id DESC);
