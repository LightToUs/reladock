ALTER TABLE nodes DROP CONSTRAINT nodes_resource_mode_owner_check;
ALTER TABLE nodes ADD CONSTRAINT nodes_resource_mode_owner_check CHECK (
 (resource_mode='shared' AND dedicated_tenant_id IS NULL) OR
 resource_mode='dedicated-tenant' OR
 (resource_mode='dedicated-rule' AND dedicated_tenant_id IS NOT NULL)
);
