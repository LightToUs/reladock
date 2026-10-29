package platform

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

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
