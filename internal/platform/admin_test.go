package platform

import (
	"context"
	"github.com/jackc/pgx/v5/pgxpool"
	"os"
	"path/filepath"
	"reladock/internal/nodeauth"
	"testing"
	"time"
)

func adminIdentity(t *testing.T, s *Store) (Identity, string) {
	t.Helper()
	who, _ := register(t, s, "platform-admin@example.com")
	if err := s.SetPlatformAdmin(context.Background(), who.Email, true); err != nil {
		t.Fatal(err)
	}
	who, token, err := s.Login(context.Background(), who.Email, "long-test-password-42")
	if err != nil {
		t.Fatal(err)
	}
	return who, token
}

func TestLogoutConcurrentWithSuspensionAndPrivilegeGrant(t *testing.T) {
	for _, operation := range []string{"suspend", "grant"} {
		t.Run(operation, func(t *testing.T) {
			s := testStore(t)
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			admin, _ := adminIdentity(t, s)
			customer, token := register(t, s, "concurrent-access@example.com")
			makeStore := func(label string) *Store {
				config := s.pool.Config()
				config.ConnConfig.RuntimeParams["application_name"] = label
				pool, err := pgxpool.NewWithConfig(ctx, config)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(pool.Close)
				return &Store{pool: pool}
			}
			logoutName := "logout-" + newID()
			adminName := "admin-" + newID()
			logoutStore := makeStore(logoutName)
			adminStore := makeStore(adminName)
			barrier := "logout-barrier-" + newID()
			control, err := s.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer control.Rollback(context.Background())
			if _, err = control.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", barrier); err != nil {
				t.Fatal(err)
			}
			// Stop logout immediately before its audit FK checks, after session deletion.
			trigger := `CREATE FUNCTION logout_barrier() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='account.logout' THEN PERFORM pg_advisory_xact_lock(hashtextextended('` + barrier + `',0)); END IF; RETURN NEW; END; $$; CREATE TRIGGER logout_barrier BEFORE INSERT ON audit_logs FOR EACH ROW EXECUTE FUNCTION logout_barrier()`
			if _, err = s.pool.Exec(ctx, trigger); err != nil {
				t.Fatal(err)
			}
			waitBlocked := func(name string) {
				t.Helper()
				deadline := time.Now().Add(4 * time.Second)
				for time.Now().Before(deadline) {
					var blocked bool
					err = s.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type='Lock')", name).Scan(&blocked)
					if err != nil {
						t.Fatal(err)
					}
					if blocked {
						return
					}
					time.Sleep(10 * time.Millisecond)
				}
				t.Fatal("expected operation to reach database lock barrier")
			}
			logoutDone := make(chan error, 1)
			adminDone := make(chan error, 1)
			go func() { logoutDone <- logoutStore.Logout(ctx, token) }()
			waitBlocked(logoutName)
			go func() {
				if operation == "suspend" {
					adminDone <- adminStore.AdminTenantAccess(ctx, admin, customer.TenantID, false, 1)
				} else {
					adminDone <- adminStore.SetPlatformAdmin(ctx, customer.Email, true)
				}
			}()
			waitBlocked(adminName)
			if err = control.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			for _, done := range []chan error{logoutDone, adminDone} {
				select {
				case err = <-done:
					if err != nil {
						t.Fatalf("concurrent logout/access update failed: %v", err)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			if _, err = s.Session(ctx, token); err != ErrUnauthorized {
				t.Fatal("concurrent change retained session")
			}
		})
	}
}

func TestPlatformPrivilegeCannotBeForgedAndRevokesSessions(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner, old := register(t, s, "owner@example.com")
	forged := owner
	forged.PlatformAdmin = true
	if _, err := s.AdminOverview(ctx, forged); err != ErrForbidden {
		t.Fatalf("forged flag: %v", err)
	}
	if err := s.SetPlatformAdmin(ctx, owner.Email, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Session(ctx, old); err != ErrUnauthorized {
		t.Fatal("grant did not revoke old sessions")
	}
	who, token, err := s.Login(ctx, owner.Email, "long-test-password-42")
	if err != nil || !who.PlatformAdmin {
		t.Fatal("admin privilege not loaded")
	}
	if _, err = s.AdminOverview(ctx, who); err != nil {
		t.Fatal(err)
	}
	if err = s.SetPlatformAdmin(ctx, who.Email, false); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Session(ctx, token); err != ErrUnauthorized {
		t.Fatal("revoke kept session")
	}
	if _, err = s.AdminOverview(ctx, who); err != ErrForbidden {
		t.Fatal("stale admin identity retained access")
	}
}

func TestAdminNodeLifecycleAndRevisionGuards(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	input := NodeInput{ID: "hk-02", Name: "香港二号", Region: "Hong Kong", ListenAddress: "0.0.0.0", PublicAddress: "192.0.2.50", CapacityMbps: 1000}
	if err := s.AdminAddNode(ctx, admin, input); err != nil {
		t.Fatal(err)
	}
	page, err := s.AdminNodes(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	var node ManagedNode
	for _, n := range page {
		if n.ID == input.ID {
			node = n
		}
	}
	if node.Revision != 1 || node.PublicAddress != "192.0.2.50" {
		t.Fatalf("node: %+v", node)
	}
	input.Name = "香港入口 B"
	if err = s.AdminUpdateNode(ctx, admin, input, node.Revision); err != nil {
		t.Fatal(err)
	}
	if err = s.AdminUpdateNode(ctx, admin, input, node.Revision); err != ErrConflict {
		t.Fatalf("stale revision: %v", err)
	}
	if err = s.AdminNodeAction(ctx, admin, input.ID, 2, "maintenance", ""); err != nil {
		t.Fatal(err)
	}
	customer, _ := register(t, s, "customer@example.com")
	r := ruleInput("udp")
	r.NodeID = input.ID
	if _, err = s.CreateRule(ctx, customer, "maintenance-admin", r); err != ErrConflict {
		t.Fatal("maintenance accepted allocation")
	}
	if err = s.AdminNodeAction(ctx, admin, input.ID, 3, "enable", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateRule(ctx, customer, "maintenance-admin", r); err != nil {
		t.Fatal(err)
	}
	input.ListenAddress = "192.0.2.51"
	if err = s.AdminUpdateNode(ctx, admin, input, 4); err != ErrConflict {
		t.Fatalf("changed in-use bind address: %v", err)
	}
	input.ListenAddress = "0.0.0.0"
	input.CapacityMbps = 500
	if err = s.AdminUpdateNode(ctx, admin, input, 4); err != ErrConflict {
		t.Fatalf("capacity below saved rule: %v", err)
	}
	if err = s.AdminNodeAction(ctx, admin, input.ID, 4, "retry", ""); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "pki")
	if err = nodeauth.CreateLocalBundle(dir, input.ID); err != nil {
		t.Fatal(err)
	}
	certificate, err := os.ReadFile(filepath.Join(dir, "agent.crt"))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AdminNodeAction(ctx, admin, input.ID, 5, "enroll", string(certificate)); err != nil {
		t.Fatal(err)
	}
	cert, _ := nodeauth.ReadCertificate(filepath.Join(dir, "agent.crt"))
	if err = s.AuthenticateNode(ctx, input.ID, nodeauth.Fingerprint(cert)); err != nil {
		t.Fatal(err)
	}
	if err = s.AdminNodeAction(ctx, admin, input.ID, 6, "revoke-certificate", ""); err != nil {
		t.Fatal(err)
	}
	if err = s.AuthenticateNode(ctx, input.ID, nodeauth.Fingerprint(cert)); err != ErrUnauthorized {
		t.Fatal("revoked node certificate still authorized")
	}
}

func TestTenantSuspensionAndPlatformRuleActions(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	customer, token := register(t, s, "tenant@example.com")
	r, err := s.CreateRule(ctx, customer, "tenant-admin-rule", ruleInput("both"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdminChangeRule(ctx, admin, customer.TenantID, r.ID, "pause", r.DesiredVersion); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdminChangeRule(ctx, admin, customer.TenantID, r.ID, "resume", r.DesiredVersion); err != ErrConflict {
		t.Fatal("rule version overwritten")
	}
	if err = s.AdminTenantAccess(ctx, admin, customer.TenantID, false, 1); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Session(ctx, token); err != ErrUnauthorized {
		t.Fatal("suspended tenant still authenticated")
	}
	if _, _, err = s.Login(ctx, customer.Email, "long-test-password-42"); err != ErrUnauthorized {
		t.Fatal("suspended tenant can log in")
	}
	if err = s.AdminTenantAccess(ctx, admin, admin.TenantID, false, 1); err != ErrConflict {
		t.Fatal("admin tenant can be disabled")
	}
	if err = s.AdminTenantAccess(ctx, admin, customer.TenantID, true, 2); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.Login(ctx, customer.Email, "long-test-password-42"); err != nil {
		t.Fatal(err)
	}
	rules, err := s.AdminRules(ctx, admin, "", "", 1)
	if err != nil || rules.Total != 1 || rules.Items[0].TenantID != customer.TenantID {
		t.Fatalf("global rules %+v %v", rules, err)
	}
	tenants, err := s.AdminTenants(ctx, admin, 1)
	if err != nil || tenants.Total != 2 {
		t.Fatal("tenant list failed")
	}
	if own, _ := s.ListRules(ctx, admin); len(own) != 0 {
		t.Fatal("platform permission leaked into tenant query")
	}
}

func TestPlatformAuditFailureRollsBackNodeMutation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	customer, token := register(t, s, "audit-tenant@example.com")
	rule, err := s.CreateRule(ctx, customer, "audit-admin-rule", ruleInput("both"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.pool.Exec(ctx, `CREATE FUNCTION reject_platform_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'audit unavailable'; END; $$; CREATE TRIGGER reject_platform_audit BEFORE INSERT ON platform_audit_logs FOR EACH ROW EXECUTE FUNCTION reject_platform_audit()`)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AdminNodeAction(ctx, admin, "hk-01", 1, "maintenance", ""); err == nil {
		t.Fatal("reported success without audit")
	}
	nodes, _ := s.ListNodes(ctx)
	if nodes[0].Status != "unprovisioned" {
		t.Fatal("mutation survived failed audit")
	}
	if _, err = s.AdminChangeRule(ctx, admin, customer.TenantID, rule.ID, "pause", rule.DesiredVersion); err == nil {
		t.Fatal("rule changed without platform audit")
	}
	rules, _ := s.ListRules(ctx, customer)
	if len(rules) != 1 || rules[0].Status != "pending-agent" || rules[0].DesiredVersion != 1 {
		t.Fatal("rule mutation survived audit failure")
	}
	if err = s.AdminTenantAccess(ctx, admin, customer.TenantID, false, 1); err == nil {
		t.Fatal("tenant changed without audit")
	}
	if _, err = s.Session(ctx, token); err != nil {
		t.Fatal("failed suspension revoked tenant session")
	}
}
