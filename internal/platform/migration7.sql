-- All prices/entitlements are explicitly configured. There are no seeded plans.
CREATE TABLE plan_price_versions (
 id uuid PRIMARY KEY,
 code text NOT NULL CHECK (code ~ '^[a-z][a-z0-9-]{0,63}$'),
 version bigint NOT NULL CHECK (version BETWEEN 1 AND 1000000),
 name text NOT NULL CHECK (length(name) BETWEEN 1 AND 120),
 currency text NOT NULL DEFAULT 'USD' CHECK (currency='USD'),
 price_cents bigint NOT NULL CHECK (price_cents>0),
 quota_bytes bigint NOT NULL CHECK (quota_bytes>0),
 bandwidth_mbps integer NOT NULL CHECK (bandwidth_mbps BETWEEN 1 AND 1000),
 period_seconds bigint NOT NULL CHECK (period_seconds BETWEEN 1 AND 31536000),
 created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(code,version)
);
CREATE TRIGGER plan_prices_no_mutation BEFORE UPDATE OR DELETE ON plan_price_versions
 FOR EACH STATEMENT EXECUTE FUNCTION billing_reject_mutation();
CREATE TRIGGER plan_prices_no_truncate BEFORE TRUNCATE ON plan_price_versions
 FOR EACH STATEMENT EXECUTE FUNCTION billing_reject_mutation();

-- Extend allowed inserts; existing immutable ledger triggers remain intact.
ALTER TABLE billing_ledger DROP CONSTRAINT billing_ledger_kind_check;
ALTER TABLE billing_ledger ADD COLUMN price_version_id uuid REFERENCES plan_price_versions(id);
ALTER TABLE billing_ledger ADD CONSTRAINT billing_ledger_kind_check CHECK (
 (kind='local-adjustment' AND price_version_id IS NULL) OR
 (kind='subscription-debit' AND amount_cents<0 AND rule_id IS NOT NULL AND price_version_id IS NOT NULL)
);
ALTER TABLE billing_ledger ADD CONSTRAINT billing_ledger_id_tenant UNIQUE(id,tenant_id);

CREATE TABLE rule_subscriptions (
 id uuid PRIMARY KEY,
 ordinal bigint GENERATED ALWAYS AS IDENTITY UNIQUE,
 tenant_id uuid NOT NULL REFERENCES tenants(id),
 rule_id uuid NOT NULL,
 price_version_id uuid NOT NULL REFERENCES plan_price_versions(id),
 ledger_id bigint NOT NULL UNIQUE,
 idempotency_key text NOT NULL CHECK (idempotency_key ~ '^[a-zA-Z0-9._:-]{8,128}$'),
 starts_at timestamptz NOT NULL,
 ends_at timestamptz NOT NULL CHECK (ends_at>starts_at),
 base_upload_bytes bigint NOT NULL CHECK (base_upload_bytes>=0),
 base_download_bytes bigint NOT NULL CHECK (base_download_bytes>=0),
 final_upload_bytes bigint,
 final_download_bytes bigint,
 enforced_reason text NOT NULL DEFAULT '' CHECK (enforced_reason IN ('','expired','quota-exhausted')),
 enforced_at timestamptz,
 UNIQUE(tenant_id,idempotency_key),
 CHECK (base_upload_bytes::numeric+base_download_bytes::numeric<=9223372036854775807),
 CHECK ((final_upload_bytes IS NULL AND final_download_bytes IS NULL) OR
  (final_upload_bytes IS NOT NULL AND final_download_bytes IS NOT NULL AND
   final_upload_bytes>=base_upload_bytes AND final_download_bytes>=base_download_bytes AND
   final_upload_bytes::numeric+final_download_bytes::numeric<=9223372036854775807)),
 FOREIGN KEY(rule_id,tenant_id) REFERENCES relay_rules(id,tenant_id),
 FOREIGN KEY(ledger_id,tenant_id) REFERENCES billing_ledger(id,tenant_id)
);
CREATE INDEX rule_subscriptions_history ON rule_subscriptions(rule_id,ordinal DESC);
CREATE INDEX rule_subscriptions_expiry ON rule_subscriptions(ends_at,rule_id);
