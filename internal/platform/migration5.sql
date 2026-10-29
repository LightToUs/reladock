-- Billing foundations only: no plan entitlements, automatic prices or payments.
CREATE TABLE billing_wallets (
 tenant_id uuid PRIMARY KEY REFERENCES tenants(id),
 currency text NOT NULL DEFAULT 'USD' CHECK (currency = 'USD'),
 balance_cents bigint NOT NULL DEFAULT 0 CHECK (balance_cents >= 0)
);
CREATE TABLE billing_ledger (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 tenant_id uuid NOT NULL REFERENCES billing_wallets(tenant_id),
 rule_id uuid,
 currency text NOT NULL DEFAULT 'USD' CHECK (currency = 'USD'),
 kind text NOT NULL CHECK (kind = 'local-adjustment'),
 amount_cents bigint NOT NULL CHECK (amount_cents <> 0 AND amount_cents <> '-9223372036854775808'::bigint),
 balance_after_cents bigint NOT NULL CHECK (balance_after_cents >= 0),
 idempotency_key text NOT NULL CHECK (idempotency_key ~ '^[a-zA-Z0-9._:-]{8,128}$'),
 reason text NOT NULL CHECK (length(reason) BETWEEN 1 AND 500),
 operator_name text NOT NULL CHECK (length(operator_name) BETWEEN 1 AND 120),
 created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(tenant_id,idempotency_key),
 FOREIGN KEY(rule_id,tenant_id) REFERENCES relay_rules(id,tenant_id)
);
CREATE INDEX billing_ledger_tenant ON billing_ledger(tenant_id,id DESC);
CREATE INDEX billing_ledger_rule ON billing_ledger(tenant_id,rule_id,id DESC);

CREATE FUNCTION billing_reject_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 RAISE EXCEPTION 'billing history is immutable' USING ERRCODE = '23514';
END;
$$;
CREATE TRIGGER billing_ledger_no_mutation BEFORE UPDATE OR DELETE ON billing_ledger
 FOR EACH STATEMENT EXECUTE FUNCTION billing_reject_mutation();
CREATE TRIGGER billing_ledger_no_truncate BEFORE TRUNCATE ON billing_ledger
 FOR EACH STATEMENT EXECUTE FUNCTION billing_reject_mutation();

-- Every stream belongs to one DB-owned rule. No tenant byte pool is maintained.
CREATE TABLE billing_usage_totals (
 rule_id uuid PRIMARY KEY,
 tenant_id uuid NOT NULL,
 node_id text NOT NULL REFERENCES nodes(id),
 active_epoch text NOT NULL CHECK (active_epoch ~ '^[a-zA-Z0-9._:-]{8,128}$'),
 upload_bytes bigint NOT NULL CHECK (upload_bytes >= 0),
 download_bytes bigint NOT NULL CHECK (download_bytes >= 0),
 updated_at timestamptz NOT NULL DEFAULT now(),
 CHECK (upload_bytes::numeric + download_bytes::numeric <= 9223372036854775807),
 FOREIGN KEY(rule_id,tenant_id) REFERENCES relay_rules(id,tenant_id)
);
CREATE INDEX billing_usage_tenant ON billing_usage_totals(tenant_id,rule_id);
CREATE TABLE billing_usage_epochs (
 rule_id uuid NOT NULL,
 tenant_id uuid NOT NULL,
 node_id text NOT NULL REFERENCES nodes(id),
 epoch text NOT NULL CHECK (epoch ~ '^[a-zA-Z0-9._:-]{8,128}$'),
 last_sequence bigint NOT NULL CHECK (last_sequence > 0),
 upload_bytes bigint NOT NULL CHECK (upload_bytes >= 0),
 download_bytes bigint NOT NULL CHECK (download_bytes >= 0),
 PRIMARY KEY(rule_id,epoch),
 CHECK (upload_bytes::numeric + download_bytes::numeric <= 9223372036854775807),
 FOREIGN KEY(rule_id,tenant_id) REFERENCES relay_rules(id,tenant_id)
);
CREATE TABLE billing_usage_reports (
 rule_id uuid NOT NULL,
 tenant_id uuid NOT NULL,
 node_id text NOT NULL REFERENCES nodes(id),
 epoch text NOT NULL,
 sequence bigint NOT NULL CHECK (sequence > 0),
 upload_bytes bigint NOT NULL CHECK (upload_bytes >= 0),
 download_bytes bigint NOT NULL CHECK (download_bytes >= 0),
 upload_delta_bytes bigint NOT NULL CHECK (upload_delta_bytes BETWEEN 0 AND upload_bytes),
 download_delta_bytes bigint NOT NULL CHECK (download_delta_bytes BETWEEN 0 AND download_bytes),
 received_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(rule_id,epoch,sequence),
 CHECK (upload_bytes::numeric + download_bytes::numeric <= 9223372036854775807),
 FOREIGN KEY(rule_id,epoch) REFERENCES billing_usage_epochs(rule_id,epoch),
 FOREIGN KEY(rule_id,tenant_id) REFERENCES relay_rules(id,tenant_id)
);
CREATE TRIGGER billing_usage_reports_no_mutation BEFORE UPDATE OR DELETE ON billing_usage_reports
 FOR EACH STATEMENT EXECUTE FUNCTION billing_reject_mutation();
CREATE TRIGGER billing_usage_reports_no_truncate BEFORE TRUNCATE ON billing_usage_reports
 FOR EACH STATEMENT EXECUTE FUNCTION billing_reject_mutation();
