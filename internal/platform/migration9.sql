-- Optional Stripe connector. Requires migrations 5 and 7. No seeded payments.
ALTER TABLE billing_ledger DROP CONSTRAINT billing_ledger_kind_check;
ALTER TABLE billing_ledger ADD CONSTRAINT billing_ledger_kind_check CHECK (
 (kind='local-adjustment' AND price_version_id IS NULL) OR
 (kind='subscription-debit' AND amount_cents<0 AND rule_id IS NOT NULL AND price_version_id IS NOT NULL) OR
 (kind='payment-credit' AND amount_cents>0 AND rule_id IS NULL AND price_version_id IS NULL) OR
 (kind='payment-refund' AND amount_cents<0 AND rule_id IS NULL AND price_version_id IS NULL)
);
CREATE TABLE payment_orders (
 id uuid PRIMARY KEY,
 tenant_id uuid NOT NULL REFERENCES tenants(id),
 created_by uuid NOT NULL,
 idempotency_key text NOT NULL CHECK (idempotency_key ~ '^[a-zA-Z0-9._:-]{8,128}$'),
 amount_cents bigint NOT NULL CHECK (amount_cents BETWEEN 50 AND 1000000),
 currency text NOT NULL DEFAULT 'USD' CHECK (currency='USD'),
 state text NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','credited','refunded','needs-review')),
 session_id text UNIQUE,
 checkout_url text,
 payment_intent text UNIQUE,
 credit_ledger_id bigint UNIQUE,
 refund_ledger_id bigint UNIQUE,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(tenant_id,idempotency_key),
 FOREIGN KEY(created_by,tenant_id) REFERENCES members(id,tenant_id),
 FOREIGN KEY(credit_ledger_id,tenant_id) REFERENCES billing_ledger(id,tenant_id),
 FOREIGN KEY(refund_ledger_id,tenant_id) REFERENCES billing_ledger(id,tenant_id),
 CHECK ((session_id IS NULL)=(checkout_url IS NULL)),
 CHECK(state<>'credited' OR credit_ledger_id IS NOT NULL),
 CHECK(state<>'refunded' OR (credit_ledger_id IS NOT NULL AND refund_ledger_id IS NOT NULL))
);
CREATE TABLE payment_provider_events (
 provider text NOT NULL DEFAULT 'stripe' CHECK(provider='stripe'),
 event_id text NOT NULL UNIQUE,
 payload_hash text NOT NULL CHECK(length(payload_hash)=64),
 event_type text NOT NULL,
 received_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(provider,event_id)
);
CREATE TRIGGER payment_events_no_mutation BEFORE UPDATE OR DELETE ON payment_provider_events
 FOR EACH STATEMENT EXECUTE FUNCTION billing_reject_mutation();
CREATE TRIGGER payment_events_no_truncate BEFORE TRUNCATE ON payment_provider_events
 FOR EACH STATEMENT EXECUTE FUNCTION billing_reject_mutation();
CREATE TABLE payment_incidents (
 id uuid PRIMARY KEY,
 dedupe_key text NOT NULL UNIQUE,
 order_id uuid REFERENCES payment_orders(id),
 event_id text NOT NULL,
 payment_intent text NOT NULL DEFAULT '',
 reason text NOT NULL,
 state text NOT NULL DEFAULT 'needs-review' CHECK(state='needs-review'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 FOREIGN KEY(event_id) REFERENCES payment_provider_events(event_id) -- event_id is unique for this Stripe-only table
);
CREATE INDEX payment_incidents_intent ON payment_incidents(payment_intent) WHERE payment_intent<>'';
