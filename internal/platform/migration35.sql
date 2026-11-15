-- Direct package checkout keeps the selected immutable package on the local
-- payment order. Existing orders remain ordinary wallet top-ups.
ALTER TABLE payment_orders
 DROP CONSTRAINT payment_orders_state_check,
 ADD CONSTRAINT payment_orders_state_check CHECK (
  state IN ('pending','credited','refunded','needs-review','expired','failed')
 ),
 ADD COLUMN purpose text NOT NULL DEFAULT 'wallet-topup'
  CHECK (purpose IN ('wallet-topup','plan-purchase')),
 ADD COLUMN rule_id uuid,
 ADD COLUMN price_version_id uuid REFERENCES plan_price_versions(id),
 ADD COLUMN purchase_state text NOT NULL DEFAULT 'none'
  CHECK (purchase_state IN ('none','pending','purchased','failed')),
 ADD COLUMN purchase_error text NOT NULL DEFAULT ''
  CHECK (purchase_error IN ('','unavailable','insufficient-balance','wallet-frozen','unauthorized','conflict','payment-expired','payment-failed')),
 ADD COLUMN subscription_id uuid,
 ADD CONSTRAINT payment_orders_rule_tenant_fkey
  FOREIGN KEY(rule_id,tenant_id) REFERENCES relay_rules(id,tenant_id),
 ADD CONSTRAINT payment_orders_subscription_tenant_fkey
  FOREIGN KEY(subscription_id,tenant_id) REFERENCES rule_subscriptions(id,tenant_id),
 ADD CONSTRAINT payment_orders_plan_shape_check CHECK (
  (purpose='wallet-topup'
   AND rule_id IS NULL
   AND price_version_id IS NULL
   AND purchase_state='none'
   AND purchase_error=''
   AND subscription_id IS NULL)
  OR
  (purpose='plan-purchase'
   AND price_version_id IS NOT NULL
   AND purchase_state IN ('pending','purchased','failed')
   AND (purchase_state='purchased')=(subscription_id IS NOT NULL)
   AND (purchase_state='failed')=(purchase_error<>''))
 );

CREATE UNIQUE INDEX payment_orders_pending_plan_selection
 ON payment_orders(tenant_id,COALESCE(rule_id,'00000000-0000-0000-0000-000000000000'::uuid),price_version_id)
 WHERE purpose='plan-purchase' AND state='pending';

CREATE INDEX payment_orders_tenant_plan_recent
 ON payment_orders(tenant_id,created_at DESC,id)
 WHERE purpose='plan-purchase';
