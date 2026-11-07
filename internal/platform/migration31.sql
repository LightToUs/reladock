-- One paid period may be shared by multiple rules. Usage remains one row per
-- subscription/rule pair so rule-level telemetry is preserved while quota is
-- enforced against the aggregate of all rows in the paid period.
ALTER TABLE subscription_usage DROP CONSTRAINT subscription_usage_pkey;
ALTER TABLE subscription_usage ADD CONSTRAINT subscription_usage_pkey PRIMARY KEY (subscription_id,rule_id);
ALTER TABLE subscription_usage ADD CONSTRAINT subscription_usage_binding_unique UNIQUE (subscription_id,rule_id);
ALTER TABLE rule_subscriptions ADD CONSTRAINT rule_subscriptions_id_tenant_unique UNIQUE (id,tenant_id);
ALTER TABLE subscription_usage DROP CONSTRAINT subscription_usage_subscription_id_rule_id_tenant_id_fkey;
ALTER TABLE subscription_usage ADD CONSTRAINT subscription_usage_subscription_tenant_fkey FOREIGN KEY (subscription_id,tenant_id) REFERENCES rule_subscriptions(id,tenant_id);
ALTER TABLE subscription_usage ADD CONSTRAINT subscription_usage_rule_tenant_fkey FOREIGN KEY (rule_id,tenant_id) REFERENCES relay_rules(id,tenant_id);
CREATE INDEX subscription_usage_rule ON subscription_usage(rule_id,subscription_id);
