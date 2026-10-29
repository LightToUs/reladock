-- Wallet-level spending hold for payment incidents. Never automatically reset.
ALTER TABLE billing_wallets
 ADD COLUMN IF NOT EXISTS spending_frozen boolean NOT NULL DEFAULT false;

-- Existing review events will be deduplicated on replay. Backfill their holds
-- here so upgrading cannot leave already disputed/refunded funds spendable.
-- A historical incident remains a hold even if its order was later refunded.
INSERT INTO billing_wallets(tenant_id,spending_frozen)
 SELECT DISTINCT o.tenant_id,true FROM payment_orders o
 WHERE o.state='needs-review'
    OR EXISTS(SELECT 1 FROM payment_incidents i WHERE i.order_id=o.id)
 ON CONFLICT(tenant_id) DO UPDATE SET spending_frozen=true;
