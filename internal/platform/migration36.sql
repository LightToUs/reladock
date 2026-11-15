-- Optional receive-only TRON USDT funding. No private keys or signing data are
-- stored by RelaDock. Quotes are fixed in integer USD cents and USDT base units.
CREATE TABLE tron_payment_orders (
 id uuid PRIMARY KEY,
 tenant_id uuid NOT NULL REFERENCES tenants(id),
 created_by uuid NOT NULL,
 idempotency_key text NOT NULL CHECK (idempotency_key ~ '^[a-zA-Z0-9._:-]{8,128}$'),
 purpose text NOT NULL CHECK (purpose IN ('wallet-topup','plan-purchase')),
 rule_id uuid,
 price_version_id uuid REFERENCES plan_price_versions(id),
 amount_cents bigint NOT NULL CHECK (amount_cents BETWEEN 50 AND 1000000),
 amount_base_units bigint NOT NULL CHECK (amount_base_units > 0),
 currency text NOT NULL DEFAULT 'USD' CHECK (currency='USD'),
 asset text NOT NULL DEFAULT 'USDT' CHECK (asset='USDT'),
 network text NOT NULL DEFAULT 'TRON' CHECK (network='TRON'),
 collection_address text NOT NULL CHECK (collection_address ~ '^T[1-9A-HJ-NP-Za-km-z]{33}$'),
 state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','confirming','credited','needs-review','expired')),
 tx_hash text CHECK (tx_hash IS NULL OR tx_hash ~ '^[0-9a-f]{64}$'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 expires_at timestamptz NOT NULL DEFAULT clock_timestamp()+interval '30 minutes',
 claimed_at timestamptz,
 next_attempt_at timestamptz,
 attempts integer NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 1000000),
 lease_token uuid,
 lease_expires_at timestamptz,
 last_error text NOT NULL DEFAULT '' CHECK (length(last_error)<=120),
 credit_ledger_id bigint UNIQUE,
 purchase_state text NOT NULL CHECK (purchase_state IN ('none','pending','purchased','failed')),
 purchase_error text NOT NULL DEFAULT '' CHECK (length(purchase_error)<=120),
 subscription_id uuid,
 UNIQUE(tenant_id,idempotency_key),
 UNIQUE(id,tenant_id),
 FOREIGN KEY(created_by,tenant_id) REFERENCES members(id,tenant_id),
 FOREIGN KEY(rule_id,tenant_id) REFERENCES relay_rules(id,tenant_id),
 FOREIGN KEY(credit_ledger_id,tenant_id) REFERENCES billing_ledger(id,tenant_id),
 FOREIGN KEY(subscription_id,tenant_id) REFERENCES rule_subscriptions(id,tenant_id),
 CHECK (amount_base_units::numeric>=amount_cents::numeric*10000
    AND amount_base_units::numeric<(amount_cents::numeric+1)*10000),
 CHECK (expires_at>created_at),
 CHECK ((tx_hash IS NULL AND claimed_at IS NULL) OR (tx_hash IS NOT NULL AND claimed_at IS NOT NULL)),
 CHECK ((lease_token IS NULL)=(lease_expires_at IS NULL)),
 CHECK ((purpose='wallet-topup' AND rule_id IS NULL AND price_version_id IS NULL AND purchase_state='none' AND subscription_id IS NULL)
     OR (purpose='plan-purchase' AND price_version_id IS NOT NULL AND purchase_state IN ('pending','purchased','failed'))),
 CHECK (state<>'credited' OR credit_ledger_id IS NOT NULL),
 CHECK ((purchase_state='purchased')=(subscription_id IS NOT NULL))
);

CREATE INDEX tron_payment_orders_tenant_recent
 ON tron_payment_orders(tenant_id,created_at DESC,id DESC);
CREATE INDEX tron_payment_orders_confirmation_queue
 ON tron_payment_orders(next_attempt_at,id)
 WHERE state='confirming';
CREATE INDEX tron_payment_orders_plan_purchase_queue
 ON tron_payment_orders(next_attempt_at,id)
 WHERE state='credited' AND purpose='plan-purchase' AND purchase_state='pending';
CREATE UNIQUE INDEX tron_payment_orders_active_quote
 ON tron_payment_orders(amount_base_units)
 WHERE state IN ('pending','confirming');

CREATE TABLE tron_payment_receipts (
 tx_hash text PRIMARY KEY CHECK (tx_hash ~ '^[0-9a-f]{64}$'),
 order_id uuid NOT NULL UNIQUE,
 tenant_id uuid NOT NULL,
 log_index integer NOT NULL CHECK (log_index>=0),
 from_address text NOT NULL CHECK (from_address ~ '^T[1-9A-HJ-NP-Za-km-z]{33}$'),
 to_address text NOT NULL CHECK (to_address ~ '^T[1-9A-HJ-NP-Za-km-z]{33}$'),
 contract_address text NOT NULL CHECK (contract_address ~ '^T[1-9A-HJ-NP-Za-km-z]{33}$'),
 amount_base_units bigint NOT NULL CHECK (amount_base_units>0),
 block_number bigint NOT NULL CHECK (block_number>0),
 block_time timestamptz NOT NULL,
 recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 FOREIGN KEY(order_id,tenant_id) REFERENCES tron_payment_orders(id,tenant_id)
);
CREATE TRIGGER tron_payment_receipts_no_mutation BEFORE UPDATE OR DELETE ON tron_payment_receipts
 FOR EACH STATEMENT EXECUTE FUNCTION billing_reject_mutation();
CREATE TRIGGER tron_payment_receipts_no_truncate BEFORE TRUNCATE ON tron_payment_receipts
 FOR EACH STATEMENT EXECUTE FUNCTION billing_reject_mutation();
