package platform

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestResourceModeNormalization(t *testing.T) {
	for _, value := range []string{
		"shared",
		"dedicated-tenant",
		"dedicated-rule",
	} {
		got, err := normalizeResourceMode(value)
		if err != nil {
			t.Fatalf("normalize %q: %v", value, err)
		}
		if string(got) != value {
			t.Fatalf("normalize %q = %q", value, got)
		}
	}
	for _, value := range []string{"", "SHARED", "dedicated", " shared "} {
		if _, err := normalizeResourceMode(value); !errors.Is(err, ErrInvalid) {
			t.Fatalf("normalize %q error = %v, want ErrInvalid", value, err)
		}
	}
}

func TestAdminNodeResourceModeValidationAndMutation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	owner, _ := register(t, s, "resource-owner@example.com")
	other, _ := register(t, s, "resource-other@example.com")

	shared := NodeInput{ID: "shared-edit", Name: "Shared", Region: "Test", ListenAddress: "0.0.0.0", CapacityMbps: 1000, ResourceMode: ResourceModeShared}
	if err := s.AdminAddNode(ctx, admin, shared); err != nil {
		t.Fatal(err)
	}
	dedicated := NodeInput{ID: "tenant-edit", Name: "Tenant", Region: "Test", ListenAddress: "0.0.0.0", CapacityMbps: 1000, ResourceMode: ResourceModeDedicatedTenant, DedicatedTenantID: owner.TenantID}
	if err := s.AdminAddNode(ctx, admin, dedicated); err != nil {
		t.Fatal(err)
	}
	if err := s.AdminAddNode(ctx, admin, NodeInput{ID: "missing-mode", Name: "Missing", Region: "Test", ListenAddress: "0.0.0.0", CapacityMbps: 1000}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing mode: %v", err)
	}
	badShared := shared
	badShared.ID = "bad-shared-owner"
	badShared.DedicatedTenantID = owner.TenantID
	if err := s.AdminAddNode(ctx, admin, badShared); !errors.Is(err, ErrInvalid) {
		t.Fatalf("shared owner: %v", err)
	}
	unassigned := dedicated
	unassigned.ID = "unassigned-dedicated"
	unassigned.DedicatedTenantID = ""
	if err := s.AdminAddNode(ctx, admin, unassigned); err != nil {
		t.Fatalf("unassigned dedicated tenant: %v", err)
	}
	badDedicated := dedicated
	badDedicated.ID = "bad-dedicated-rule"
	badDedicated.ResourceMode = ResourceModeDedicatedRule
	badDedicated.DedicatedTenantID = ""
	if err := s.AdminAddNode(ctx, admin, badDedicated); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing dedicated-rule owner: %v", err)
	}
	badDedicated.ResourceMode = ResourceModeDedicatedTenant
	badDedicated.ID = "unknown-dedicated-owner"
	badDedicated.DedicatedTenantID = newID()
	if err := s.AdminAddNode(ctx, admin, badDedicated); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown dedicated owner: %v", err)
	}

	shared.ResourceMode = ResourceModeDedicatedTenant
	shared.DedicatedTenantID = owner.TenantID
	if err := s.AdminUpdateNode(ctx, admin, shared, 1); err != nil {
		t.Fatalf("empty node mode update: %v", err)
	}
	ruleInput := ruleInput("tcp")
	ruleInput.NodeID = shared.ID
	rule, err := s.CreateRule(ctx, owner, "mode-owner-rule", ruleInput)
	if err != nil {
		t.Fatal(err)
	}
	shared.ResourceMode = ResourceModeShared
	shared.DedicatedTenantID = ""
	if err = s.AdminUpdateNode(ctx, admin, shared, 2); !errors.Is(err, ErrConflict) {
		t.Fatalf("in-use mode update: %v", err)
	}
	shared.ResourceMode = ResourceModeDedicatedTenant
	shared.DedicatedTenantID = other.TenantID
	if err = s.AdminUpdateNode(ctx, admin, shared, 2); !errors.Is(err, ErrConflict) {
		t.Fatalf("in-use owner update: %v", err)
	}
	if err = s.DeleteRule(ctx, owner, rule.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.AdminUpdateNode(ctx, admin, shared, 2); !errors.Is(err, ErrConflict) {
		t.Fatalf("bound owner changed through generic edit: %v", err)
	}
	shared.DedicatedTenantID = ""
	if err = s.AdminUpdateNode(ctx, admin, shared, 2); !errors.Is(err, ErrConflict) {
		t.Fatalf("bound owner cleared through generic edit: %v", err)
	}

	nodes, err := s.AdminNodes(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, node := range nodes {
		if node.ID == dedicated.ID {
			found = true
			if node.ResourceMode != ResourceModeDedicatedTenant || node.DedicatedTenantID != owner.TenantID {
				t.Fatalf("managed node resource fields: %+v", node)
			}
		}
	}
	if !found {
		t.Fatal("dedicated node missing from admin inventory")
	}
}

func TestMigration43AllowsOnlyUnassignedDedicatedTenantNodes(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	owner, _ := register(t, s, "migration43-owner@example.com")
	assigned := NodeInput{ID: "migration43-assigned", Name: "Assigned", Region: "Test", ListenAddress: "0.0.0.0", CapacityMbps: 1000, ResourceMode: ResourceModeDedicatedTenant, DedicatedTenantID: owner.TenantID}
	if err := s.AdminAddNode(ctx, admin, assigned); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `ALTER TABLE nodes DROP CONSTRAINT nodes_resource_mode_owner_check;
ALTER TABLE nodes ADD CONSTRAINT nodes_resource_mode_owner_check CHECK (
 (resource_mode='shared' AND dedicated_tenant_id IS NULL) OR
 (resource_mode IN ('dedicated-tenant','dedicated-rule') AND dedicated_tenant_id IS NOT NULL)
);
DELETE FROM schema_migrations WHERE version=43;`); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var version int
	var assignedOwner string
	if err := s.pool.QueryRow(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 43 {
		t.Fatalf("schema version=%d want=43", version)
	}
	if err := s.pool.QueryRow(ctx, `SELECT dedicated_tenant_id::text FROM nodes WHERE id=$1`, assigned.ID).Scan(&assignedOwner); err != nil || assignedOwner != owner.TenantID {
		t.Fatalf("assigned owner=%q err=%v", assignedOwner, err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO nodes(id,name,region,status,listen_address,capacity_mbps,resource_mode,dedicated_tenant_id) VALUES('migration43-unassigned','Unassigned','Test','unprovisioned','0.0.0.0',1000,'dedicated-tenant',NULL)`); err != nil {
		t.Fatalf("unassigned dedicated tenant rejected: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE nodes SET resource_mode='shared',dedicated_tenant_id=$2 WHERE id=$1`, assigned.ID, owner.TenantID); err == nil {
		t.Fatal("shared node with owner was accepted")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE nodes SET resource_mode='dedicated-rule',dedicated_tenant_id=NULL WHERE id=$1`, assigned.ID); err == nil {
		t.Fatal("unassigned dedicated-rule node was accepted")
	}
}

func dedicatedReleaseFixture(t *testing.T, s *Store, nodeID string) (Identity, Identity, int64) {
	t.Helper()
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	owner, _ := register(t, s, nodeID+"@example.com")
	if err := s.AdminAddNode(ctx, admin, NodeInput{ID: nodeID, Name: "Release fixture", Region: "Test", ListenAddress: "0.0.0.0", CapacityMbps: 1000, ResourceMode: ResourceModeDedicatedTenant, DedicatedTenantID: owner.TenantID}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, "UPDATE config_outbox SET state='applied' WHERE node_id=$1", nodeID); err != nil {
		t.Fatal(err)
	}
	var revision int64
	if err := s.pool.QueryRow(ctx, "SELECT revision FROM nodes WHERE id=$1", nodeID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	return admin, owner, revision
}

func hasReleaseReason(reasons []string, want string) bool {
	for _, reason := range reasons {
		if reason == want {
			return true
		}
	}
	return false
}

func TestAdminReleaseDedicatedTenantNodeAuthorizationRevisionAndAudit(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin, owner, revision := dedicatedReleaseFixture(t, s, "release-success")
	if err := s.AdminReleaseDedicatedTenantNode(ctx, owner, "release-success", revision); !errors.Is(err, ErrForbidden) {
		t.Fatalf("tenant release err=%v", err)
	}
	if err := s.AdminReleaseDedicatedTenantNode(ctx, admin, "release-success", revision+1); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale release err=%v", err)
	}
	nodes, err := s.AdminNodes(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, node := range nodes {
		if node.ID == "release-success" {
			found = true
			if !node.DedicatedTenantReleaseEligible || len(node.DedicatedTenantReleaseReasons) != 0 {
				t.Fatalf("release eligibility=%v reasons=%v", node.DedicatedTenantReleaseEligible, node.DedicatedTenantReleaseReasons)
			}
		}
	}
	if !found {
		t.Fatal("release node missing")
	}
	if err = s.AdminReleaseDedicatedTenantNode(ctx, admin, "release-success", revision); err != nil {
		t.Fatal(err)
	}
	var dedicatedOwner, auditTenant string
	var afterRevision, audits int64
	if err = s.pool.QueryRow(ctx, `SELECT COALESCE(n.dedicated_tenant_id::text,''),n.revision,
	 (SELECT count(*) FROM platform_audit_logs WHERE action='node.dedicated-release' AND resource_id=n.id),
	 COALESCE((SELECT details->>'tenantId' FROM platform_audit_logs WHERE action='node.dedicated-release' AND resource_id=n.id ORDER BY id DESC LIMIT 1),'')
	 FROM nodes n WHERE n.id=$1`, "release-success").Scan(&dedicatedOwner, &afterRevision, &audits, &auditTenant); err != nil {
		t.Fatal(err)
	}
	if dedicatedOwner != "" || afterRevision != revision+1 || audits != 1 || auditTenant != owner.TenantID {
		t.Fatalf("owner=%q revision=%d audits=%d auditTenant=%q", dedicatedOwner, afterRevision, audits, auditTenant)
	}
	if err = s.AdminReleaseDedicatedTenantNode(ctx, admin, "release-success", revision+1); !errors.Is(err, ErrConflict) {
		t.Fatalf("already unassigned release err=%v", err)
	}
}

func TestAdminReleaseDedicatedTenantNodeAuditFailureRollsBack(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin, owner, revision := dedicatedReleaseFixture(t, s, "release-audit")
	if _, err := s.pool.Exec(ctx, `CREATE FUNCTION reject_dedicated_release_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='node.dedicated-release' THEN RAISE EXCEPTION 'reject release audit'; END IF; RETURN NEW; END; $$; CREATE TRIGGER reject_dedicated_release_audit BEFORE INSERT ON platform_audit_logs FOR EACH ROW EXECUTE FUNCTION reject_dedicated_release_audit()`); err != nil {
		t.Fatal(err)
	}
	if err := s.AdminReleaseDedicatedTenantNode(ctx, admin, "release-audit", revision); err == nil {
		t.Fatal("release committed despite audit failure")
	}
	var dedicatedOwner string
	var afterRevision, audits int64
	if err := s.pool.QueryRow(ctx, `SELECT dedicated_tenant_id::text,revision,(SELECT count(*) FROM platform_audit_logs WHERE action='node.dedicated-release' AND resource_id=$1) FROM nodes WHERE id=$1`, "release-audit").Scan(&dedicatedOwner, &afterRevision, &audits); err != nil {
		t.Fatal(err)
	}
	if dedicatedOwner != owner.TenantID || afterRevision != revision || audits != 0 {
		t.Fatalf("rollback owner=%q revision=%d audits=%d", dedicatedOwner, afterRevision, audits)
	}
}

func TestAdminReleaseDedicatedTenantNodeBlockers(t *testing.T) {
	configStates := []string{"queued", "leased", "validated", "applying", "failed"}
	for _, state := range configStates {
		t.Run("config-"+state, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			admin, _, revision := dedicatedReleaseFixture(t, s, "release-"+state)
			if _, err := s.pool.Exec(ctx, "UPDATE config_outbox SET state=$2 WHERE node_id=$1", "release-"+state, state); err != nil {
				t.Fatal(err)
			}
			nodes, err := s.AdminNodes(ctx, admin)
			if err != nil {
				t.Fatal(err)
			}
			for _, node := range nodes {
				if node.ID == "release-"+state && (node.DedicatedTenantReleaseEligible || !hasReleaseReason(node.DedicatedTenantReleaseReasons, "config-"+state)) {
					t.Fatalf("state=%s eligible=%v reasons=%v", state, node.DedicatedTenantReleaseEligible, node.DedicatedTenantReleaseReasons)
				}
			}
			if err = s.AdminReleaseDedicatedTenantNode(ctx, admin, "release-"+state, revision); !errors.Is(err, ErrConflict) {
				t.Fatalf("state=%s release err=%v", state, err)
			}
		})
	}

	for _, blocker := range []string{"active-subscription", "undeleted-rule", "endpoint-allocation", "endpoint-retirement"} {
		t.Run(blocker, func(t *testing.T) {
			s := subscriptionTestStore(t)
			ctx := context.Background()
			admin, owner, revision := dedicatedReleaseFixture(t, s, "release-"+blocker)
			nodeID := "release-" + blocker
			switch blocker {
			case "active-subscription":
				offering := publishedOffering(t, s, admin, "release-subscription", nodeID)
				input := configuredPlan()
				input.Code = "release-subscription"
				input.OfferingID = offering.ID
				input.ResourceMode = ResourceModeDedicatedTenant
				plan, err := s.AdminCreatePlan(ctx, admin, input)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = s.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: owner.TenantID, Key: "release-subscription-funds", AmountCents: 100, Reason: "test", Operator: "test"}); err != nil {
					t.Fatal(err)
				}
				if _, err = s.PurchasePlan(ctx, owner, "release-subscription-buy", PurchasePlanInput{PriceVersionID: plan.ID}); err != nil {
					t.Fatal(err)
				}
			case "undeleted-rule", "endpoint-allocation", "endpoint-retirement":
				ruleInput := ruleInput("tcp")
				ruleInput.NodeID = nodeID
				rule, err := s.CreateRule(ctx, owner, "release-blocker-"+blocker, ruleInput)
				if err != nil {
					t.Fatal(err)
				}
				if blocker == "endpoint-retirement" {
					var allocationID, address, protocol string
					var port int
					if err = s.pool.QueryRow(ctx, "SELECT allocation_id::text,host(listen_address),protocol,listen_port FROM endpoint_allocations WHERE rule_id=$1 LIMIT 1", rule.ID).Scan(&allocationID, &address, &protocol, &port); err != nil {
						t.Fatal(err)
					}
					if _, err = s.pool.Exec(ctx, `INSERT INTO endpoint_retirements(id,allocation_id,node_id,rule_id,tenant_id,listen_address,protocol,listen_port,state) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'waiting-removal')`, newID(), allocationID, nodeID, rule.ID, owner.TenantID, address, protocol, port); err != nil {
						t.Fatal(err)
					}
				}
			}
			nodes, err := s.AdminNodes(ctx, admin)
			if err != nil {
				t.Fatal(err)
			}
			for _, node := range nodes {
				if node.ID == nodeID && (node.DedicatedTenantReleaseEligible || !hasReleaseReason(node.DedicatedTenantReleaseReasons, blocker)) {
					t.Fatalf("blocker=%s eligible=%v reasons=%v", blocker, node.DedicatedTenantReleaseEligible, node.DedicatedTenantReleaseReasons)
				}
			}
			if err = s.AdminReleaseDedicatedTenantNode(ctx, admin, nodeID, revision); !errors.Is(err, ErrConflict) {
				t.Fatalf("blocker=%s release err=%v", blocker, err)
			}
		})
	}
}

func TestDedicatedTenantNodeAdmission(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	owner, _ := register(t, s, "dedicated-owner@example.com")
	other, _ := register(t, s, "dedicated-other@example.com")
	input := NodeInput{ID: "tenant-only", Name: "Tenant only", Region: "Test", ListenAddress: "0.0.0.0", CapacityMbps: 1000, ResourceMode: ResourceModeDedicatedTenant, DedicatedTenantID: owner.TenantID}
	if err := s.AdminAddNode(ctx, admin, input); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		rule := ruleInput("tcp")
		rule.NodeID = input.ID
		if _, err := s.CreateRule(ctx, owner, "tenant-owner-"+string(rune('a'+i)), rule); err != nil {
			t.Fatalf("owner rule %d: %v", i, err)
		}
	}
	rule := ruleInput("tcp")
	rule.NodeID = input.ID
	if _, err := s.CreateRule(ctx, other, "tenant-other-rule", rule); !errors.Is(err, ErrConflict) {
		t.Fatalf("other tenant admission: %v", err)
	}
}

func TestDedicatedRuleConcurrentAdmission(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	owner, _ := register(t, s, "rule-owner@example.com")
	other, _ := register(t, s, "rule-other@example.com")
	input := NodeInput{ID: "single-rule", Name: "Single rule", Region: "Test", ListenAddress: "0.0.0.0", CapacityMbps: 1000, ResourceMode: ResourceModeDedicatedRule, DedicatedTenantID: owner.TenantID}
	if err := s.AdminAddNode(ctx, admin, input); err != nil {
		t.Fatal(err)
	}
	foreign := ruleInput("tcp")
	foreign.NodeID = input.ID
	if _, err := s.CreateRule(ctx, other, "single-foreign", foreign); !errors.Is(err, ErrConflict) {
		t.Fatalf("foreign dedicated-rule admission: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rule := ruleInput("tcp")
			rule.NodeID = input.ID
			_, err := s.CreateRule(ctx, owner, "single-owner-"+string(rune('a'+i)), rule)
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	successes, conflicts := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrConflict):
			conflicts++
		default:
			t.Fatalf("concurrent admission: %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d", successes, conflicts)
	}
	for table, want := range map[string]int{"relay_rules": 1, "endpoint_allocations": 1, "idempotency_requests": 1} {
		var count int
		if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE "+map[string]string{"relay_rules": "node_id=$1 AND status<>'deleted'", "endpoint_allocations": "node_id=$1", "idempotency_requests": "tenant_id=$1"}[table], map[string]string{"relay_rules": input.ID, "endpoint_allocations": input.ID, "idempotency_requests": owner.TenantID}[table]).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != want {
			t.Fatalf("%s count=%d want=%d", table, count, want)
		}
	}
}

func TestMigration26ResourceModeDefaultsAndConstraints(t *testing.T) {
	store, _, who, _, plan := subscriptionFixture(t)
	ctx := context.Background()
	pool := store.pool
	tenantID, planID := who.TenantID, plan.ID
	revertSchemaTo26(t, store)
	_, err := pool.Exec(ctx, `ALTER TABLE plan_price_versions DROP COLUMN resource_mode;
 ALTER TABLE nodes DROP COLUMN resource_mode, DROP COLUMN dedicated_tenant_id;
 DELETE FROM schema_migrations WHERE version>25;`)
	if err != nil {
		t.Fatal(err)
	}

	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var version int
	if err = pool.QueryRow(ctx, "SELECT max(version) FROM schema_migrations").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion {
		t.Fatalf("schema version = %d, want %d", version, SchemaVersion)
	}
	var nodeMode, planMode string
	var owner *string
	if err = pool.QueryRow(ctx, "SELECT resource_mode,dedicated_tenant_id::text FROM nodes WHERE id='hk-01'").Scan(&nodeMode, &owner); err != nil {
		t.Fatal(err)
	}
	if nodeMode != "shared" || owner != nil {
		t.Fatalf("legacy node mode=%q owner=%v", nodeMode, owner)
	}
	if err = pool.QueryRow(ctx, "SELECT resource_mode FROM plan_price_versions WHERE id=$1", planID).Scan(&planMode); err != nil {
		t.Fatal(err)
	}
	if planMode != "shared" {
		t.Fatalf("legacy plan mode=%q", planMode)
	}

	if _, err = pool.Exec(ctx, "UPDATE nodes SET resource_mode='shared', dedicated_tenant_id=$1 WHERE id='hk-01'", tenantID); err == nil {
		t.Fatal("shared node with dedicated tenant was accepted")
	}
	if _, err = pool.Exec(ctx, "UPDATE nodes SET resource_mode='dedicated-tenant' WHERE id='hk-01'"); err != nil {
		t.Fatalf("unassigned dedicated-tenant node was rejected: %v", err)
	}
	if _, err = pool.Exec(ctx, "UPDATE nodes SET resource_mode='dedicated-rule' WHERE id='hk-01'"); err == nil {
		t.Fatal("dedicated-rule node without tenant was accepted")
	}
	if _, err = pool.Exec(ctx, "UPDATE nodes SET resource_mode='dedicated-rule', dedicated_tenant_id=$1 WHERE id='hk-01'", tenantID); err != nil {
		t.Fatalf("valid dedicated node: %v", err)
	}
	if _, err = pool.Exec(ctx, "INSERT INTO plan_price_versions(id,code,version,name,price_cents,quota_bytes,bandwidth_mbps,period_seconds,resource_mode) VALUES($1,'bad-mode',1,'Bad',50,100,100,3600,'unknown')", newID()); err == nil {
		t.Fatal("unknown plan resource mode was accepted")
	}
}
