CREATE TABLE rule_source_ip_blocks (
 id uuid PRIMARY KEY,
 tenant_id uuid NOT NULL,
 rule_id uuid NOT NULL,
 cidr cidr NOT NULL,
 reason text NOT NULL CHECK (length(reason) BETWEEN 1 AND 500),
 state text NOT NULL DEFAULT 'active' CHECK (state IN ('active','removed')),
 requested_version bigint NOT NULL CHECK (requested_version >= 1),
 removed_version bigint CHECK (removed_version >= requested_version),
 created_by uuid NOT NULL REFERENCES members(id),
 removed_by uuid REFERENCES members(id),
 created_at timestamptz NOT NULL DEFAULT now(),
 removed_at timestamptz,
 FOREIGN KEY(rule_id,tenant_id) REFERENCES relay_rules(id,tenant_id),
 CHECK (family(cidr)=4 AND masklen(cidr)>0),
 CHECK ((state='active' AND removed_version IS NULL AND removed_by IS NULL AND removed_at IS NULL)
     OR (state='removed' AND removed_version IS NOT NULL AND removed_by IS NOT NULL AND removed_at IS NOT NULL))
);
CREATE UNIQUE INDEX rule_source_ip_blocks_active_cidr
 ON rule_source_ip_blocks(rule_id,cidr) WHERE state='active';
CREATE INDEX rule_source_ip_blocks_active_rule
 ON rule_source_ip_blocks(rule_id,created_at,id) WHERE state='active';

