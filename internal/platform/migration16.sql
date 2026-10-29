CREATE TABLE node_cost_entries (
 id uuid PRIMARY KEY,
 node_id text NOT NULL REFERENCES nodes(id),
 currency text NOT NULL DEFAULT 'USD' CHECK(currency='USD'),
 amount_cents bigint NOT NULL CHECK(amount_cents>0),
 service_from timestamptz NOT NULL,
 service_to timestamptz NOT NULL CHECK(service_to>service_from),
 invoice_ref text NOT NULL CHECK(length(invoice_ref) BETWEEN 1 AND 160),
 idempotency_key text NOT NULL UNIQUE CHECK(idempotency_key ~ '^[a-zA-Z0-9._:-]{8,128}$'),
 actor_id uuid NOT NULL REFERENCES members(id),
 actor_email text NOT NULL,
 recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(node_id,invoice_ref)
);
CREATE INDEX node_cost_entries_recorded ON node_cost_entries(recorded_at,node_id);
CREATE INDEX cost_revenue_ledger_recorded ON billing_ledger(created_at,rule_id) WHERE kind='subscription-debit';
CREATE TRIGGER node_cost_entries_immutable BEFORE UPDATE OR DELETE ON node_cost_entries
 FOR EACH STATEMENT EXECUTE FUNCTION billing_reject_mutation();
CREATE TRIGGER node_cost_entries_no_truncate BEFORE TRUNCATE ON node_cost_entries
 FOR EACH STATEMENT EXECUTE FUNCTION billing_reject_mutation();
