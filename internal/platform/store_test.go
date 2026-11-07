package platform

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMigration33IsCurrentSchemaVersion(t *testing.T) {
	if SchemaVersion != 33 {
		t.Fatalf("SchemaVersion = %d, want 33", SchemaVersion)
	}
}

func TestMigration29PreservesLegacyPlansAndAllowsNumericPlanCodes(t *testing.T) {
	store, _, who, rule, plan := subscriptionFixture(t)
	ctx := context.Background()
	pool := store.pool
	if _, err := store.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: who.TenantID, Key: "migration-legacy-credit", AmountCents: 100, Reason: "test", Operator: "test"}); err != nil {
		t.Fatal(err)
	}
	period, err := store.PurchasePlan(ctx, who, "migration-legacy-purchase", PurchasePlanInput{RuleID: rule.ID, PriceVersionID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	planID, subscriptionID := plan.ID, period.ID
	revertSchemaTo26(t, store)
	if _, err = pool.Exec(ctx, migration27SQL); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, "INSERT INTO schema_migrations(version) VALUES(27)"); err != nil {
		t.Fatal(err)
	}
	offeringIDValue := newID()
	if _, err = pool.Exec(ctx, `INSERT INTO line_offerings(id,code,name,node_id,source_market,destination_market,state,revision) VALUES($1,'hk-mainland','Hong Kong optimized','hk-01','中国大陆','香港','paused',3)`, offeringIDValue); err != nil {
		t.Fatal(err)
	}

	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var version int
	var offeringID *string
	var retainedPlan string
	if err = pool.QueryRow(ctx, "SELECT max(version) FROM schema_migrations").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion {
		t.Fatalf("schema version = %d, want %d", version, SchemaVersion)
	}
	if err = pool.QueryRow(ctx, "SELECT offering_id::text FROM plan_price_versions WHERE id=$1", planID).Scan(&offeringID); err != nil {
		t.Fatal(err)
	}
	if offeringID != nil {
		t.Fatalf("legacy plan received guessed offering %q", *offeringID)
	}
	if err = pool.QueryRow(ctx, "SELECT price_version_id::text FROM rule_subscriptions WHERE id=$1", subscriptionID).Scan(&retainedPlan); err != nil || retainedPlan != planID {
		t.Fatalf("subscription plan=%q want=%q err=%v", retainedPlan, planID, err)
	}

	var state string
	var revision int64
	var existingName, existingSource, existingDestination, existingNotes string
	if err = pool.QueryRow(ctx, "SELECT name,source_market,destination_market,state,revision,notes FROM line_offerings WHERE id=$1", offeringIDValue).Scan(&existingName, &existingSource, &existingDestination, &state, &revision, &existingNotes); err != nil || existingName != "Hong Kong optimized" || existingSource != "中国大陆" || existingDestination != "香港" || state != "paused" || revision != 3 || existingNotes != "" {
		t.Fatalf("migrated offering name=%q source=%q destination=%q state=%q revision=%d notes=%q err=%v", existingName, existingSource, existingDestination, state, revision, existingNotes, err)
	}
	optionalID := newID()
	if _, err = pool.Exec(ctx, `INSERT INTO line_offerings(id,code,name,node_id,source_market,destination_market,notes) VALUES($1,'hk-draft','Draft','hk-01','','','  手工核对出口  ')`, optionalID); err != nil {
		t.Fatalf("optional markets and notes rejected: %v", err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO plan_price_versions(id,code,version,name,price_cents,quota_bytes,bandwidth_mbps,period_seconds) VALUES('00000000-0000-0000-0000-000000000006','001',1,'Numeric',50,100,100,3600)`); err != nil {
		t.Fatalf("numeric plan code rejected after migration 29: %v", err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO line_offerings(id,code,name,node_id,source_market,destination_market) VALUES('00000000-0000-0000-0000-000000000007','001','Numeric offering','hk-01','','')`); err == nil {
		t.Fatal("numeric line offering code accepted after migration 29")
	}
	var source, destination, notes string
	if err = pool.QueryRow(ctx, "SELECT source_market,destination_market,notes FROM line_offerings WHERE id=$1", optionalID).Scan(&source, &destination, &notes); err != nil || source != "" || destination != "" || notes != "  手工核对出口  " {
		t.Fatalf("optional offering source=%q destination=%q notes=%q err=%v", source, destination, notes, err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO line_offerings(id,code,name,node_id,source_market,destination_market,notes) VALUES('00000000-0000-0000-0000-000000000005','notes-too-long','Bad','hk-01','','',repeat('x',501))`); err == nil {
		t.Fatal("notes length constraint was not enforced")
	}
	for name, statement := range map[string]string{
		"duplicate code": `INSERT INTO line_offerings(id,code,name,node_id,source_market,destination_market) VALUES('00000000-0000-0000-0000-000000000001','hk-mainland','Duplicate','hk-01','A','B')`,
		"invalid state":  `INSERT INTO line_offerings(id,code,name,node_id,source_market,destination_market,state) VALUES('00000000-0000-0000-0000-000000000002','bad-state','Bad','hk-01','A','B','online')`,
		"missing node":   `INSERT INTO line_offerings(id,code,name,node_id,source_market,destination_market) VALUES('00000000-0000-0000-0000-000000000003','bad-node','Bad','missing','A','B')`,
		"missing offer":  `INSERT INTO plan_price_versions(id,code,version,name,price_cents,quota_bytes,bandwidth_mbps,period_seconds,offering_id) VALUES('00000000-0000-0000-0000-000000000004','missing-offer',1,'Missing',50,100,100,3600,'00000000-0000-0000-0000-000000000099')`,
	} {
		if _, err = pool.Exec(ctx, statement); err == nil {
			t.Fatalf("%s constraint was not enforced", name)
		}
	}
}

// Every test owns a PostgreSQL schema; no existing tables are reset.
func testStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is required for PostgreSQL integration tests")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	schema := "test_" + strings.ReplaceAll(newID(), "-", "")
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	store := &Store{pool: pool}
	t.Cleanup(func() { pool.Close(); admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); admin.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return store
}

func register(t *testing.T, s *Store, email string) (Identity, string) {
	t.Helper()
	who, token, err := s.Register(context.Background(), "企业 "+email, email, "long-test-password-42")
	if err != nil {
		t.Fatal(err)
	}
	return who, token
}

func TestChangePasswordVerifiesCurrentPasswordKeepsCurrentSessionAndRevokesOthers(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	who, currentSession := register(t, s, "change-password@example.com")
	_, otherSession, err := s.Login(ctx, who.Email, "long-test-password-42")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ChangePassword(ctx, who, currentSession, "wrong-current-password", "new-long-password-42"); err != ErrUnauthorized {
		t.Fatalf("wrong current password: %v", err)
	}
	if err = s.ChangePassword(ctx, who, currentSession, "long-test-password-42", "new-long-password-42"); err != nil {
		t.Fatalf("change password: %v", err)
	}
	if _, err = s.Session(ctx, currentSession); err != nil {
		t.Fatalf("current session revoked: %v", err)
	}
	if _, err = s.Session(ctx, otherSession); err != ErrUnauthorized {
		t.Fatalf("other session retained: %v", err)
	}
	if _, _, err = s.Login(ctx, who.Email, "long-test-password-42"); err != ErrUnauthorized {
		t.Fatalf("old password still works: %v", err)
	}
	if _, _, err = s.Login(ctx, who.Email, "new-long-password-42"); err != nil {
		t.Fatalf("new password rejected: %v", err)
	}
}

func TestAccountSessionsAndDuplicateRegistration(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	who, token := register(t, s, "Owner@Example.com")
	if who.Email != "owner@example.com" || who.Role != "owner" {
		t.Fatalf("identity: %+v", who)
	}
	loaded, err := s.Session(ctx, token)
	if err != nil || loaded.TenantID != who.TenantID {
		t.Fatalf("session: %+v %v", loaded, err)
	}
	if _, _, err = s.Register(ctx, "Other", "owner@example.com", "long-test-password-42"); err != ErrConflict {
		t.Fatalf("duplicate: %v", err)
	}
	if _, _, err = s.Login(ctx, "owner@example.com", "wrong-password"); err != ErrUnauthorized {
		t.Fatalf("password: %v", err)
	}
	_, second, err := s.Login(ctx, "owner@example.com", "long-test-password-42")
	if err != nil {
		t.Fatal(err)
	}
	if second == token {
		t.Fatal("session token reused")
	}
	var digest string
	if err = s.pool.QueryRow(ctx, "SELECT token_hash FROM sessions LIMIT 1").Scan(&digest); err != nil {
		t.Fatal(err)
	}
	if digest == token {
		t.Fatal("plaintext token persisted")
	}
	if err = s.Logout(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Session(ctx, token); err != ErrUnauthorized {
		t.Fatalf("revocation: %v", err)
	}
	if _, err = s.Session(ctx, second); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, "UPDATE sessions SET expires_at=$1", time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Session(ctx, second); err != ErrUnauthorized {
		t.Fatalf("expiry: %v", err)
	}
	var tenants int
	s.pool.QueryRow(ctx, "SELECT count(*) FROM tenants").Scan(&tenants)
	if tenants != 1 {
		t.Fatalf("failed registration leaked tenant: %d", tenants)
	}
}

func TestInvalidRegistrationDoesNotPersist(t *testing.T) {
	s := testStore(t)
	for _, tc := range []struct{ company, email, password string }{
		{"", "a@example.com", "long-test-password-42"},
		{"Company", "Name <a@example.com>", "long-test-password-42"},
		{"Company", "a@example.com", "short"},
	} {
		if _, _, err := s.Register(context.Background(), tc.company, tc.email, tc.password); err != ErrInvalid {
			t.Fatalf("validation: %v", err)
		}
	}
	var count int
	s.pool.QueryRow(context.Background(), "SELECT count(*) FROM tenants").Scan(&count)
	if count != 0 {
		t.Fatal("invalid registration wrote data")
	}
}

func TestDataSurvivesNewDatabasePoolAndMigrationRerun(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	who, token := register(t, s, "persistent@example.com")
	rule, err := s.CreateRule(ctx, who, "persistent-rule", ruleInput("udp"))
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, s.pool.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	reopened := &Store{pool: pool}
	if err = reopened.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	loaded, err := reopened.Session(ctx, token)
	if err != nil || loaded.TenantID != who.TenantID {
		t.Fatalf("persisted session: %v", err)
	}
	rules, err := reopened.ListRules(ctx, loaded)
	if err != nil || len(rules) != 1 || rules[0].ID != rule.ID {
		t.Fatalf("persisted rules: %+v %v", rules, err)
	}
}
