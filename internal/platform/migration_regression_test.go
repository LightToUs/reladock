package platform

import (
	"context"
	"testing"
)

func revertSchemaTo40(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(), `
DROP TABLE oauth_login_attempts;
DROP TABLE member_external_identities;
DELETE FROM schema_migrations WHERE version=41;`); err != nil {
		t.Fatal(err)
	}
}

func TestMigration41From40PreservesExistingFacts(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	who, token := register(t, store, "migration41-preserve@example.com")
	var tenantsBefore, membersBefore, sessionsBefore, auditsBefore int
	if err := store.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM tenants),(SELECT count(*) FROM members),(SELECT count(*) FROM sessions),(SELECT count(*) FROM audit_logs)`).Scan(&tenantsBefore, &membersBefore, &sessionsBefore, &auditsBefore); err != nil {
		t.Fatal(err)
	}
	revertSchemaTo40(t, store)
	for pass := 0; pass < 2; pass++ {
		if err := store.Migrate(ctx); err != nil {
			t.Fatalf("migration pass %d: %v", pass+1, err)
		}
	}
	var version, tenantsAfter, membersAfter, sessionsAfter, auditsAfter int
	if err := store.pool.QueryRow(ctx, `SELECT (SELECT max(version) FROM schema_migrations),(SELECT count(*) FROM tenants),(SELECT count(*) FROM members),(SELECT count(*) FROM sessions),(SELECT count(*) FROM audit_logs)`).Scan(&version, &tenantsAfter, &membersAfter, &sessionsAfter, &auditsAfter); err != nil {
		t.Fatal(err)
	}
	if version != 41 || tenantsAfter != tenantsBefore || membersAfter != membersBefore || sessionsAfter != sessionsBefore || auditsAfter != auditsBefore {
		t.Fatalf("migration changed facts: version=%d tenants=%d/%d members=%d/%d sessions=%d/%d audits=%d/%d", version, tenantsAfter, tenantsBefore, membersAfter, membersBefore, sessionsAfter, sessionsBefore, auditsAfter, auditsBefore)
	}
	if loaded, err := store.Session(ctx, token); err != nil || loaded.MemberID != who.MemberID {
		t.Fatalf("preserved session = %+v, err = %v", loaded, err)
	}
}

func revertSchemaTo38(t *testing.T, s *Store) {
	t.Helper()
	_, err := s.pool.Exec(context.Background(), `
DROP TABLE lightsail_jobs;
DROP TRIGGER protect_lightsail_create_identity ON lightsail_instances;
DROP TABLE lightsail_instances;
DROP FUNCTION protect_lightsail_create_identity();
DELETE FROM schema_migrations WHERE version=39;`)
	if err != nil {
		t.Fatal(err)
	}
}

func TestMigration39From38PreservesExistingFacts(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	who, _ := register(t, store, "migration39-preserve@example.com")
	if _, err := store.pool.Exec(ctx, `INSERT INTO platform_audit_logs(actor_id,actor_email,action,resource_id,details) VALUES($1,$2,'migration39-marker','existing-resource','{"preserve":true}')`, who.MemberID, who.Email); err != nil {
		t.Fatal(err)
	}
	var tenantsBefore, auditsBefore int
	if err := store.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM tenants),(SELECT count(*) FROM platform_audit_logs)`).Scan(&tenantsBefore, &auditsBefore); err != nil {
		t.Fatal(err)
	}
	revertSchemaTo38(t, store)
	for attempt := 0; attempt < 2; attempt++ {
		if err := store.Migrate(ctx); err != nil {
			t.Fatalf("migration pass %d: %v", attempt+1, err)
		}
	}
	var version, tenantsAfter, auditsAfter, markers int
	if err := store.pool.QueryRow(ctx, `SELECT (SELECT max(version) FROM schema_migrations),(SELECT count(*) FROM tenants),(SELECT count(*) FROM platform_audit_logs),(SELECT count(*) FROM platform_audit_logs WHERE action='migration39-marker' AND details->>'preserve'='true')`).Scan(&version, &tenantsAfter, &auditsAfter, &markers); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion || tenantsAfter != tenantsBefore || auditsAfter != auditsBefore || markers != 1 {
		t.Fatalf("migration changed existing facts: version=%d tenants=%d/%d audits=%d/%d markers=%d", version, tenantsAfter, tenantsBefore, auditsAfter, auditsBefore, markers)
	}
}

// Reconstruct the actual pre-package-sharing schema, including its stricter
// bindings. Do not use CASCADE: an unexpected newer dependency must fail loudly.
// Fixtures must contain only bound, single-rule subscriptions valid before v30.
func revertSchemaTo29(t *testing.T, s *Store) {
	t.Helper()
	_, err := s.pool.Exec(context.Background(), `
DROP TABLE lightsail_jobs;
DROP TRIGGER protect_lightsail_create_identity ON lightsail_instances;
DROP TABLE lightsail_instances;
DROP FUNCTION protect_lightsail_create_identity();
DROP TABLE tron_payment_scan_cursors;
DROP TABLE tron_payment_receipts;
DROP TABLE tron_payment_orders;
ALTER TABLE operations_incidents
 DROP CONSTRAINT operations_incidents_kind_check,
 ADD CONSTRAINT operations_incidents_kind_check CHECK (kind IN (
  'contact-stale','config-failed','certificate-expiring','certificate-expired',
  'certificate-rotation-failed','resource-cpu-high','resource-memory-high',
  'resource-disk-high','resource-conntrack-high','resource-clock-skew','dataplane-drift'
 ));
DROP INDEX payment_orders_tenant_plan_recent;
DROP INDEX payment_orders_pending_plan_selection;
ALTER TABLE payment_orders
 DROP CONSTRAINT payment_orders_plan_shape_check,
 DROP CONSTRAINT payment_orders_subscription_tenant_fkey,
 DROP CONSTRAINT payment_orders_rule_tenant_fkey,
 DROP CONSTRAINT payment_orders_state_check,
 DROP COLUMN subscription_id,
 DROP COLUMN purchase_error,
 DROP COLUMN purchase_state,
 DROP COLUMN price_version_id,
 DROP COLUMN rule_id,
 DROP COLUMN purpose,
 ADD CONSTRAINT payment_orders_state_check CHECK (
  state IN ('pending','credited','refunded','needs-review')
 );
DROP TABLE source_flow_events;
DROP TABLE source_flow_gaps;
DROP TABLE applied_source_flow_rules;
DROP TABLE node_source_flow_latest;
ALTER TABLE relay_rules DROP COLUMN source_logging_enabled,
 DROP COLUMN source_logging_applied_enabled,
 DROP COLUMN source_logging_applied_version,
 DROP COLUMN source_logging_applied_generation;
ALTER TABLE nodes DROP COLUMN agent_source_flow_supported;
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

func TestMigration29ToCurrentPreservesBillingUsageAndIdentity(t *testing.T) {
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
			query := `SELECT COALESCE(jsonb_agg(row ORDER BY row::text),'[]'::jsonb)::text FROM (SELECT to_jsonb(t)-ARRAY['auto_renew','auto_renew_error','renewed_from','certificate_renewal_fingerprint','agent_certificate_rotation_supported','agent_source_flow_supported','source_logging_enabled','source_logging_applied_enabled','source_logging_applied_version','source_logging_applied_generation'] AS row FROM ` + table + ` t) history`
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
	if err = s.pool.QueryRow(ctx, `SELECT max(version),count(*) FILTER (WHERE version BETWEEN 30 AND $1) FROM schema_migrations`, SchemaVersion).Scan(&version, &records); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion || records != SchemaVersion-29 {
		t.Fatalf("migration records: version=%d records=%d", version, records)
	}
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM rule_subscriptions WHERE id=$1 AND NOT auto_renew AND auto_renew_error='' AND renewed_from IS NULL`, period.ID).Scan(&defaults); err != nil || defaults != 1 {
		t.Fatalf("renewal defaults=%d err=%v", defaults, err)
	}
	var rotationSupported, sourceFlowSupported bool
	var renewalFingerprint *string
	if err = s.pool.QueryRow(ctx, `SELECT agent_certificate_rotation_supported,agent_source_flow_supported,certificate_renewal_fingerprint FROM nodes WHERE id=$1`, rule.NodeID).Scan(&rotationSupported, &sourceFlowSupported, &renewalFingerprint); err != nil || rotationSupported || sourceFlowSupported || renewalFingerprint != nil {
		t.Fatalf("certificate defaults rotation=%v sourceFlow=%v fingerprint=%v err=%v", rotationSupported, sourceFlowSupported, renewalFingerprint, err)
	}
}
