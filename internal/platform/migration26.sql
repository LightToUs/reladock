ALTER TABLE nodes
 ADD COLUMN resource_mode text NOT NULL DEFAULT 'shared'
  CHECK (resource_mode IN ('shared','dedicated-tenant','dedicated-rule')),
 ADD COLUMN dedicated_tenant_id uuid REFERENCES tenants(id),
 ADD CONSTRAINT nodes_resource_mode_owner_check CHECK (
  (resource_mode='shared' AND dedicated_tenant_id IS NULL) OR
  (resource_mode IN ('dedicated-tenant','dedicated-rule') AND dedicated_tenant_id IS NOT NULL)
 );

ALTER TABLE plan_price_versions
 ADD COLUMN resource_mode text NOT NULL DEFAULT 'shared'
  CHECK (resource_mode IN ('shared','dedicated-tenant','dedicated-rule'));
