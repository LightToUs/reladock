ALTER TABLE members ADD COLUMN is_platform_admin boolean NOT NULL DEFAULT false;
ALTER TABLE tenants ADD COLUMN status text NOT NULL DEFAULT 'active' CHECK (status IN ('active','suspended'));
ALTER TABLE tenants ADD COLUMN revision bigint NOT NULL DEFAULT 1;
ALTER TABLE nodes ADD COLUMN revision bigint NOT NULL DEFAULT 1;
ALTER TABLE nodes ADD COLUMN public_address inet;
ALTER TABLE nodes ADD COLUMN capacity_mbps integer NOT NULL DEFAULT 1000 CHECK (capacity_mbps BETWEEN 1 AND 1000000);
CREATE TABLE platform_audit_logs (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 actor_id uuid REFERENCES members(id),
 actor_email text NOT NULL,
 action text NOT NULL,
 resource_id text NOT NULL,
 details jsonb NOT NULL DEFAULT '{}',
 created_at timestamptz NOT NULL DEFAULT now()
);
