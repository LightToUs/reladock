-- Renewal is opt-in. One successor per paid period prevents duplicate debits.
ALTER TABLE rule_subscriptions ADD COLUMN auto_renew boolean NOT NULL DEFAULT false;
ALTER TABLE rule_subscriptions ADD COLUMN auto_renew_error text NOT NULL DEFAULT '';
ALTER TABLE rule_subscriptions ADD COLUMN renewed_from uuid UNIQUE;
ALTER TABLE rule_subscriptions ADD CONSTRAINT subscription_renewal_tenant
 FOREIGN KEY(renewed_from,tenant_id) REFERENCES rule_subscriptions(id,tenant_id);
