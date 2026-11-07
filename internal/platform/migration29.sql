ALTER TABLE plan_price_versions
 DROP CONSTRAINT IF EXISTS plan_price_versions_code_check;

ALTER TABLE plan_price_versions
 ADD CONSTRAINT plan_price_versions_code_check
 CHECK (code ~ '^[a-z0-9][a-z0-9-]{0,63}$');
