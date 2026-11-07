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

func TestNodeResourceModeAdministration(t *testing.T) {
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
	badDedicated := dedicated
	badDedicated.ID = "bad-dedicated-owner"
	badDedicated.DedicatedTenantID = ""
	if err := s.AdminAddNode(ctx, admin, badDedicated); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing dedicated owner: %v", err)
	}
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
	if err = s.AdminUpdateNode(ctx, admin, shared, 2); err != nil {
		t.Fatalf("empty-after-delete owner update: %v", err)
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
	if _, err = pool.Exec(ctx, "UPDATE nodes SET resource_mode='dedicated-tenant' WHERE id='hk-01'"); err == nil {
		t.Fatal("dedicated node without tenant was accepted")
	}
	if _, err = pool.Exec(ctx, "UPDATE nodes SET resource_mode='dedicated-rule', dedicated_tenant_id=$1 WHERE id='hk-01'", tenantID); err != nil {
		t.Fatalf("valid dedicated node: %v", err)
	}
	if _, err = pool.Exec(ctx, "INSERT INTO plan_price_versions(id,code,version,name,price_cents,quota_bytes,bandwidth_mbps,period_seconds,resource_mode) VALUES($1,'bad-mode',1,'Bad',50,100,100,3600,'unknown')", newID()); err == nil {
		t.Fatal("unknown plan resource mode was accepted")
	}
}
