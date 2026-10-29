-- Register this migration as version 4 in Store.Migrate before exposing members APIs.
CREATE UNIQUE INDEX members_single_owner ON members(tenant_id) WHERE role='owner';
CREATE TABLE member_invitations (
 id uuid PRIMARY KEY,
 tenant_id uuid NOT NULL REFERENCES tenants(id),
 email text NOT NULL CHECK (email=lower(btrim(email)) AND length(email) BETWEEN 3 AND 254),
 role text NOT NULL CHECK (role IN ('admin','viewer')),
 token_hash text NOT NULL UNIQUE CHECK (length(token_hash)=64),
 invited_by uuid NOT NULL,
 expires_at timestamptz NOT NULL,
 accepted_at timestamptz,
 revoked_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(invited_by,tenant_id) REFERENCES members(id,tenant_id),
 CHECK (expires_at>created_at),
 CHECK (accepted_at IS NULL OR revoked_at IS NULL)
);
CREATE UNIQUE INDEX invitations_pending_email ON member_invitations(tenant_id,email)
 WHERE accepted_at IS NULL AND revoked_at IS NULL;
CREATE TABLE api_keys (
 id uuid PRIMARY KEY,
 tenant_id uuid NOT NULL,
 member_id uuid NOT NULL,
 name text NOT NULL CHECK (length(name) BETWEEN 1 AND 80),
 token_hash text NOT NULL UNIQUE CHECK (length(token_hash)=64),
 scope text NOT NULL CHECK (scope IN ('read','write')),
 expires_at timestamptz NOT NULL,
 revoked_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(member_id,tenant_id) REFERENCES members(id,tenant_id),
 CHECK (expires_at>created_at)
);
CREATE INDEX api_keys_member ON api_keys(tenant_id,member_id,created_at);
