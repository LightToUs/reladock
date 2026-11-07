package platform

import (
	"context"
	"testing"
)

// Reconstruct the actual pre-package-sharing schema, including its stricter
// bindings. Do not use CASCADE: an unexpected newer dependency must fail loudly.
// Fixtures must contain only bound, single-rule subscriptions valid before v30.
func revertSchemaTo29(t *testing.T, s *Store) {
	t.Helper()
	_, err := s.pool.Exec(context.Background(), `
DROP TRIGGER clear_node_certificate_renewal ON nodes;
DROP FUNCTION clear_node_certificate_renewal();
ALTER TABLE nodes DROP COLUMN certificate_renewal_fingerprint, DROP COLUMN agent_certificate_rotation_supported;
ALTER TABLE rule_subscriptions DROP CONSTRAINT subscription_renewal_tenant,
 DROP COLUMN renewed_from, DROP COLUMN auto_renew, DROP COLUMN auto_renew_error;
DROP INDEX subscription_usage_rule;
ALTER TABLE subscription_usage DROP CONSTRAINT subscription_usage_subscription_tenant_fkey,
 DROP CONSTRAINT subscription_usage_rule_tenant_fkey,
 DROP CONSTRAINT subscription_usage_binding_unique,
 DROP CONSTRAINT subscription_usage_pkey;
ALTER TABLE rule_subscriptions DROP CONSTRAINT rule_subscriptions_id_tenant_unique;
ALTER TABLE subscription_usage ADD CONSTRAINT subscription_usage_pkey PRIMARY KEY(subscription_id),
 ADD CONSTRAINT subscription_usage_subscription_id_rule_id_tenant_id_fkey
 FOREIGN KEY(subscription_id,rule_id,tenant_id) REFERENCES rule_subscriptions(id,rule_id,tenant_id);
ALTER TABLE rule_subscriptions ALTER COLUMN rule_id SET NOT NULL;
ALTER TABLE billing_ledger DROP CONSTRAINT billing_ledger_kind_check;
ALTER TABLE billing_ledger ADD CONSTRAINT billing_ledger_kind_check CHECK (
 (kind='local-adjustment' AND price_version_id IS NULL) OR
 (kind='subscription-debit' AND amount_cents<0 AND rule_id IS NOT NULL AND price_version_id IS NOT NULL) OR
 (kind='payment-credit' AND amount_cents>0 AND rule_id IS NULL AND price_version_id IS NULL) OR
 (kind='payment-refund' AND amount_cents<0 AND rule_id IS NULL AND price_version_id IS NULL)
);
DELETE FROM schema_migrations WHERE version>29;`)
	if err != nil {
		t.Fatal(err)
	}
}

func revertSchemaTo26(t *testing.T, s *Store) {
	t.Helper()
	revertSchemaTo29(t, s)
	_, err := s.pool.Exec(context.Background(), `
ALTER TABLE plan_price_versions DROP COLUMN offering_id;
DROP TABLE line_offerings;
ALTER TABLE plan_price_versions DROP CONSTRAINT plan_price_versions_code_check,
 ADD CONSTRAINT plan_price_versions_code_check CHECK (code ~ '^[a-z][a-z0-9-]{0,63}$');
DELETE FROM schema_migrations WHERE version>26;`)
	if err != nil {
		t.Fatal(err)
	}
}

func TestMigration29To33PreservesBillingUsageAndIdentity(t *testing.T) {
	s, _, who, rule, plan := subscriptionFixture(t)
	ctx := context.Background()
	if _, err := s.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: who.TenantID, Key: "migration29-credit", AmountCents: 200, Reason: "migration fixture", Operator: "test"}); err != nil {
		t.Fatal(err)
	}
	period, err := s.PurchasePlan(ctx, who, "migration29-purchase", PurchasePlanInput{RuleID: rule.ID, PriceVersionID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	revertSchemaTo29(t, s)
	// Nonzero historical usage must survive without resetting the paid period.
	if _, err = s.pool.Exec(ctx, `UPDATE subscription_usage SET upload_bytes=17,download_bytes=23 WHERE subscription_id=$1`, period.ID); err != nil {
		t.Fatal(err)
	}
	var version int
	var primaryKey, binding string
	var ruleRequired bool
	if err = s.pool.QueryRow(ctx, `SELECT (SELECT max(version) FROM schema_migrations),
 (SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid='subscription_usage'::regclass AND conname='subscription_usage_pkey'),
 (SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid='subscription_usage'::regclass AND conname='subscription_usage_subscription_id_rule_id_tenant_id_fkey'),
 (SELECT attnotnull FROM pg_attribute WHERE attrelid='rule_subscriptions'::regclass AND attname='rule_id')`).Scan(&version, &primaryKey, &binding, &ruleRequired); err != nil {
		t.Fatal(err)
	}
	if version != 29 || primaryKey != "PRIMARY KEY (subscription_id)" || binding != "FOREIGN KEY (subscription_id, rule_id, tenant_id) REFERENCES rule_subscriptions(id, rule_id, tenant_id)" || !ruleRequired {
		t.Fatalf("not a v29 source: version=%d key=%q binding=%q required=%v", version, primaryKey, binding, ruleRequired)
	}
	// Compare complete historical rows, omitting only columns introduced in v32/33.
	snapshot := func() map[string]string {
		t.Helper()
		result := map[string]string{}
		for _, table := range []string{"billing_wallets", "billing_ledger", "plan_price_versions", "line_offerings", "relay_rules", "rule_subscriptions", "subscription_usage", "nodes", "endpoint_allocations"} {
			var rows string
			query := `SELECT COALESCE(jsonb_agg(row ORDER BY row::text),'[]'::jsonb)::text FROM (SELECT to_jsonb(t)-ARRAY['auto_renew','auto_renew_error','renewed_from','certificate_renewal_fingerprint','agent_certificate_rotation_supported'] AS row FROM ` + table + ` t) history`
			if err := s.pool.QueryRow(ctx, query).Scan(&rows); err != nil {
				t.Fatal(err)
			}
			result[table] = rows
		}
		return result
	}
	before := snapshot()
	for attempt := 0; attempt < 2; attempt++ {
		if err = s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		for table, rows := range snapshot() {
			if rows != before[table] {
				t.Fatalf("migration pass %d changed historical %s rows", attempt+1, table)
			}
		}
	}
	var records, defaults int
	if err = s.pool.QueryRow(ctx, `SELECT max(version),count(*) FILTER (WHERE version BETWEEN 30 AND 33) FROM schema_migrations`).Scan(&version, &records); err != nil {
		t.Fatal(err)
	}
	if version != 33 || records != 4 {
		t.Fatalf("migration records: version=%d records=%d", version, records)
	}
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM rule_subscriptions WHERE id=$1 AND NOT auto_renew AND auto_renew_error='' AND renewed_from IS NULL`, period.ID).Scan(&defaults); err != nil || defaults != 1 {
		t.Fatalf("renewal defaults=%d err=%v", defaults, err)
	}
	var rotationSupported bool
	var renewalFingerprint *string
	if err = s.pool.QueryRow(ctx, `SELECT agent_certificate_rotation_supported,certificate_renewal_fingerprint FROM nodes WHERE id=$1`, rule.NodeID).Scan(&rotationSupported, &renewalFingerprint); err != nil || rotationSupported || renewalFingerprint != nil {
		t.Fatalf("certificate defaults supported=%v fingerprint=%v err=%v", rotationSupported, renewalFingerprint, err)
	}
}
