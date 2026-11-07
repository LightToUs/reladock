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
	input := NodeInput{ID: "hk-02", Name: "香港二号", Region: "Hong Kong", ListenAddress: "0.0.0.0", PublicAddress: "192.0.2.50", CapacityMbps: 1000, ResourceMode: ResourceModeShared}
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
	var enrolledExpiry time.Time
	if err = s.pool.QueryRow(ctx, "SELECT agent_certificate_not_after FROM nodes WHERE id=$1", input.ID).Scan(&enrolledExpiry); err != nil || !enrolledExpiry.Equal(cert.NotAfter) {
		t.Fatalf("enrolled certificate expiry: %v %v", enrolledExpiry, err)
	}
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

func TestAdminNodesExposeAgentContactStatus(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	status := func() string {
		t.Helper()
		nodes, err := s.AdminNodes(ctx, admin)
		if err != nil {
			t.Fatal(err)
		}
		for _, node := range nodes {
			if node.ID == "hk-01" {
				return node.ContactStatus
			}
		}
		t.Fatal("seed node not found")
		return ""
	}
	for _, tc := range []struct {
		name   string
		update string
		want   string
	}{
		{"never connected", "UPDATE nodes SET agent_heartbeat_at=NULL WHERE id='hk-01'", "never-seen"},
		{"online", "UPDATE nodes SET agent_heartbeat_at=clock_timestamp()-interval '89 seconds' WHERE id='hk-01'", "online"},
		{"offline", "UPDATE nodes SET agent_heartbeat_at=clock_timestamp()-interval '91 seconds' WHERE id='hk-01'", "offline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.pool.Exec(ctx, tc.update); err != nil {
				t.Fatal(err)
			}
			if got := status(); got != tc.want {
				t.Fatalf("contact status=%q want %q", got, tc.want)
			}
		})
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

func TestAdminWithdrawsNeverDeployedEmptyNode(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	input := NodeInput{ID: "unused-01", Name: "误登记节点", Region: "Hong Kong", ListenAddress: "0.0.0.0", CapacityMbps: 1000, ResourceMode: ResourceModeShared}
	if err := s.AdminAddNode(ctx, admin, input); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO node_bootstrap(token_hash,node_id,expires_at) VALUES($1,$2,clock_timestamp()+interval '15 minutes')`, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", input.ID); err != nil {
		t.Fatal(err)
	}
	nodes, err := s.AdminNodes(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	withdrawable := false
	for _, node := range nodes {
		if node.ID == input.ID {
			withdrawable = node.Withdrawable
		}
	}
	if !withdrawable {
		t.Fatal("untouched node registration was not withdrawable")
	}
	if err = s.AdminWithdrawNodeRegistration(ctx, admin, input.ID, 2); err != ErrConflict {
		t.Fatalf("stale withdrawal revision: %v", err)
	}
	if err = s.AdminWithdrawNodeRegistration(ctx, admin, input.ID, 1); err != nil {
		t.Fatal(err)
	}
	for table, query := range map[string]string{
		"node":      "SELECT count(*) FROM nodes WHERE id=$1",
		"config":    "SELECT count(*) FROM config_outbox WHERE node_id=$1",
		"bootstrap": "SELECT count(*) FROM node_bootstrap WHERE node_id=$1",
	} {
		var count int
		if err = s.pool.QueryRow(ctx, query, input.ID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s rows after withdrawal: count=%d err=%v", table, count, err)
		}
	}
	var audits int
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM platform_audit_logs WHERE action='node.withdraw-registration' AND resource_id=$1", input.ID).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("withdrawal audit: count=%d err=%v", audits, err)
	}
	if err = s.AdminAddNode(ctx, admin, input); err != nil {
		t.Fatalf("withdrawn never-deployed node id was not reusable: %v", err)
	}
	nodes, err = s.AdminNodes(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	replacementRevision := int64(0)
	for _, node := range nodes {
		if node.ID == input.ID {
			replacementRevision = node.Revision
		}
	}
	if replacementRevision <= 1 {
		t.Fatalf("replacement reused an old revision: %d", replacementRevision)
	}
	if err = s.AdminWithdrawNodeRegistration(ctx, admin, input.ID, 1); err != ErrConflict {
		t.Fatalf("stale request targeted replacement registration: %v", err)
	}
	var replacements int
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM nodes WHERE id=$1", input.ID).Scan(&replacements); err != nil || replacements != 1 {
		t.Fatalf("replacement registration was removed: count=%d err=%v", replacements, err)
	}
}

func TestAdminWithdrawRejectsLifecycleHistory(t *testing.T) {
	for _, tc := range []struct {
		name  string
		block func(context.Context, *Store, Identity, string) error
	}{
		{
			name: "certificate",
			block: func(ctx context.Context, s *Store, _ Identity, id string) error {
				_, err := s.pool.Exec(ctx, "UPDATE nodes SET certificate_fingerprint=$2 WHERE id=$1", id, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
				return err
			},
		},
		{
			name: "claimed configuration",
			block: func(ctx context.Context, s *Store, _ Identity, id string) error {
				_, err := s.pool.Exec(ctx, "UPDATE config_outbox SET state='leased',attempts=1,lease_token='claimed',lease_until=clock_timestamp()+interval '1 minute' WHERE node_id=$1", id)
				return err
			},
		},
		{
			name: "rule history",
			block: func(ctx context.Context, s *Store, _ Identity, id string) error {
				customer, _ := register(t, s, "withdraw-rule@example.com")
				input := ruleInput("tcp")
				input.NodeID = id
				_, err := s.CreateRule(ctx, customer, "withdraw-rule-key", input)
				return err
			},
		},
		{
			name: "redeemed bootstrap credential",
			block: func(ctx context.Context, s *Store, _ Identity, id string) error {
				_, err := s.pool.Exec(ctx, `INSERT INTO node_bootstrap(token_hash,node_id,expires_at,csr_hash,certificate,fingerprint) VALUES($1,$2,clock_timestamp()+interval '15 minutes',$3,$4,$5)`, "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", id, "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd", []byte("certificate"), "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")
				return err
			},
		},
		{
			name: "cost history",
			block: func(ctx context.Context, s *Store, admin Identity, id string) error {
				_, err := s.pool.Exec(ctx, `INSERT INTO node_cost_entries(id,node_id,amount_cents,service_from,service_to,invoice_ref,idempotency_key,actor_id,actor_email) VALUES($1,$2,100,clock_timestamp()-interval '1 day',clock_timestamp(),$3,$4,$5,$6)`, newID(), id, "invoice-withdraw", "cost-withdraw-key", admin.MemberID, admin.Email)
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			admin, _ := adminIdentity(t, s)
			id := "blocked-01"
			if err := s.AdminAddNode(ctx, admin, NodeInput{ID: id, Name: "不可撤销节点", Region: "Hong Kong", ListenAddress: "0.0.0.0", CapacityMbps: 1000, ResourceMode: ResourceModeShared}); err != nil {
				t.Fatal(err)
			}
			if err := tc.block(ctx, s, admin, id); err != nil {
				t.Fatal(err)
			}
			nodes, err := s.AdminNodes(ctx, admin)
			if err != nil {
				t.Fatal(err)
			}
			for _, node := range nodes {
				if node.ID == id && node.Withdrawable {
					t.Fatal("node with lifecycle history was presented as withdrawable")
				}
			}
			if err = s.AdminWithdrawNodeRegistration(ctx, admin, id, 1); err != ErrConflict {
				t.Fatalf("withdrawal with lifecycle history: %v", err)
			}
		})
	}
}

func TestAdminWithdrawRollbackOnAuditFailure(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	id := "rollback-01"
	if err := s.AdminAddNode(ctx, admin, NodeInput{ID: id, Name: "回滚节点", Region: "Hong Kong", ListenAddress: "0.0.0.0", CapacityMbps: 1000, ResourceMode: ResourceModeShared}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `CREATE FUNCTION reject_withdraw_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='node.withdraw-registration' THEN RAISE EXCEPTION 'audit unavailable'; END IF; RETURN NEW; END; $$; CREATE TRIGGER reject_withdraw_audit BEFORE INSERT ON platform_audit_logs FOR EACH ROW EXECUTE FUNCTION reject_withdraw_audit()`); err != nil {
		t.Fatal(err)
	}
	if err := s.AdminWithdrawNodeRegistration(ctx, admin, id, 1); err == nil {
		t.Fatal("withdrawal reported success without audit")
	}
	for table, query := range map[string]string{
		"node":   "SELECT count(*) FROM nodes WHERE id=$1",
		"config": "SELECT count(*) FROM config_outbox WHERE node_id=$1",
	} {
		var count int
		if err := s.pool.QueryRow(ctx, query, id).Scan(&count); err != nil || count != 1 {
			t.Fatalf("%s did not roll back: count=%d err=%v", table, count, err)
		}
	}
}
