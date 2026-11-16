package platform

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func addDedicatedRangeNode(t *testing.T, s *Store, admin, owner Identity, id string) {
	t.Helper()
	if err := s.AdminAddNode(context.Background(), admin, NodeInput{ID: id, Name: id, Region: "Test", ListenAddress: "0.0.0.0", CapacityMbps: 1000, ResourceMode: ResourceModeDedicatedTenant, DedicatedTenantID: owner.TenantID}); err != nil {
		t.Fatal(err)
	}
}

func TestMigration42BackfillsRangeBoundsAndDefaultPools(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	who, _ := register(t, s, "range-migration@example.com")
	rule, err := s.CreateRule(ctx, who, "range-migration-rule", ruleInput("udp"))
	if err != nil {
		t.Fatal(err)
	}

	var end, pools int
	if err = s.pool.QueryRow(ctx, "SELECT listen_port_end FROM relay_rules WHERE id=$1", rule.ID).Scan(&end); err != nil {
		t.Fatal(err)
	}
	if end != rule.ListenPort {
		t.Fatalf("backfilled end=%d want start=%d", end, rule.ListenPort)
	}
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM node_customer_port_pools WHERE node_id='hk-01' AND port_start=20000 AND port_end=60000").Scan(&pools); err != nil {
		t.Fatal(err)
	}
	if pools != 2 {
		t.Fatalf("default pools=%d want tcp+udp", pools)
	}
}

func TestMigration42SeedsNewNodesAndAllowsExactLowPorts(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	if err := s.AdminAddNode(ctx, admin, NodeInput{ID: "range-new", Name: "Range new", Region: "Test", ListenAddress: "0.0.0.0", CapacityMbps: 1000, ResourceMode: ResourceModeShared}); err != nil {
		t.Fatal(err)
	}
	var pools int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM node_customer_port_pools WHERE node_id='range-new' AND port_start=20000 AND port_end=60000").Scan(&pools); err != nil || pools != 2 {
		t.Fatalf("new node pools=%d err=%v", pools, err)
	}
	who, _ := register(t, s, "range-low-port@example.com")
	ruleID := newID()
	if _, err := s.pool.Exec(ctx, `INSERT INTO relay_rules(id,tenant_id,node_id,name,protocol,target_host,target_port,bandwidth_mbps,listen_port,listen_port_end,status)
		VALUES($1,$2,'hk-01','low port','udp','198.51.100.2',443,100,443,443,'pending-agent')`, ruleID, who.TenantID); err != nil {
		t.Fatalf("low rule port rejected after migration: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO endpoint_allocations(node_id,listen_address,protocol,listen_port,rule_id,tenant_id)
		VALUES('hk-01','0.0.0.0','udp',443,$1,$2)`, ruleID, who.TenantID); err != nil {
		t.Fatalf("low allocation port rejected after migration: %v", err)
	}
}

func TestMigration42RangeConstraintsAndCapabilityFollowCertificateIdentity(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	who, _ := register(t, s, "range-constraints@example.com")
	for name, values := range map[string][2]int{
		"reversed": {41000, 40000},
		"zero":     {0, 1},
		"too high": {65535, 65536},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := s.pool.Exec(ctx, `INSERT INTO relay_rules(id,tenant_id,node_id,name,protocol,target_host,target_port,bandwidth_mbps,listen_port,listen_port_end,status)
				VALUES($1,$2,'hk-01',$3,'udp','198.51.100.2',443,100,$4,$5,'pending-agent')`, newID(), who.TenantID, name, values[0], values[1]); err == nil {
				t.Fatal("invalid range accepted")
			}
		})
	}
	first, second := strings.Repeat("a", 64), strings.Repeat("b", 64)
	if _, err := s.pool.Exec(ctx, "UPDATE nodes SET certificate_fingerprint=$1,agent_port_range_supported=true WHERE id='hk-01'", first); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, "UPDATE nodes SET certificate_fingerprint=$1 WHERE id='hk-01'", second); err != nil {
		t.Fatal(err)
	}
	var supported bool
	if err := s.pool.QueryRow(ctx, "SELECT agent_port_range_supported FROM nodes WHERE id='hk-01'").Scan(&supported); err != nil || supported {
		t.Fatalf("range capability survived identity replacement: supported=%v err=%v", supported, err)
	}
}

func TestDedicatedTenantCustomRangeReservationIsExactAndIdempotent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner, _ := register(t, s, "range-owner@example.com")
	admin, _ := adminIdentity(t, s)
	addDedicatedRangeNode(t, s, admin, owner, "range-exact")
	input := ruleInput("udp")
	input.NodeID, input.ListenPort, input.ListenPortEnd = "range-exact", 40000, 41000

	rule, err := s.CreateRule(ctx, owner, "range-exact-key", input)
	if err != nil {
		t.Fatal(err)
	}
	if rule.ListenPort != 40000 || rule.ListenPortEnd != 41000 {
		t.Fatalf("created range=%d-%d", rule.ListenPort, rule.ListenPortEnd)
	}
	var endpoints, minimum, maximum int
	if err = s.pool.QueryRow(ctx, "SELECT count(*),min(listen_port),max(listen_port) FROM endpoint_allocations WHERE rule_id=$1", rule.ID).Scan(&endpoints, &minimum, &maximum); err != nil {
		t.Fatal(err)
	}
	if endpoints != 1001 || minimum != 40000 || maximum != 41000 {
		t.Fatalf("allocations count=%d range=%d-%d", endpoints, minimum, maximum)
	}
	replayed, err := s.CreateRule(ctx, owner, "range-exact-key", input)
	if err != nil || replayed.ID != rule.ID || replayed.ListenPortEnd != 41000 {
		t.Fatalf("replay=%+v err=%v", replayed, err)
	}
	input.ListenPortEnd = 41001
	if _, err = s.CreateRule(ctx, owner, "range-exact-key", input); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed range replay error=%v", err)
	}
}

func TestCustomRangeEligibilityAndValidationAreAtomic(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner, _ := register(t, s, "range-validation@example.com")
	admin, _ := adminIdentity(t, s)
	addDedicatedRangeNode(t, s, admin, owner, "range-validation")

	tests := []struct {
		name  string
		node  string
		start int
		end   int
		want  error
	}{
		{name: "shared node", node: "hk-01", start: 40000, end: 40001, want: ErrConflict},
		{name: "end only", node: "range-validation", end: 40001, want: ErrInvalid},
		{name: "reversed", node: "range-validation", start: 41000, end: 40000, want: ErrInvalid},
		{name: "outside pool", node: "range-validation", start: 19999, end: 20000, want: ErrConflict},
		{name: "too many", node: "range-validation", start: 40000, end: 44096, want: ErrInvalid},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			input := ruleInput("udp")
			input.NodeID, input.ListenPort, input.ListenPortEnd = tc.node, tc.start, tc.end
			if _, err := s.CreateRule(ctx, owner, "range-invalid-"+strings.ReplaceAll(tc.name, " ", "-"), input); !errors.Is(err, tc.want) {
				t.Fatalf("error=%v want=%v", err, tc.want)
			}
		})
	}
	var rules, endpoints int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM relay_rules WHERE node_id IN ('hk-01','range-validation') AND tenant_id=$1", owner.TenantID).Scan(&rules); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM endpoint_allocations WHERE node_id='range-validation'").Scan(&endpoints); err != nil {
		t.Fatal(err)
	}
	if rules != 0 || endpoints != 0 {
		t.Fatalf("invalid requests left rules=%d endpoints=%d", rules, endpoints)
	}
}

func TestOverlappingAndBothProtocolRangesRollbackCompletely(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner, _ := register(t, s, "range-overlap@example.com")
	admin, _ := adminIdentity(t, s)
	addDedicatedRangeNode(t, s, admin, owner, "range-overlap")

	first := ruleInput("udp")
	first.NodeID, first.ListenPort, first.ListenPortEnd = "range-overlap", 40000, 40010
	if _, err := s.CreateRule(ctx, owner, "range-overlap-first", first); err != nil {
		t.Fatal(err)
	}
	overlap := ruleInput("udp")
	overlap.NodeID, overlap.ListenPort, overlap.ListenPortEnd = "range-overlap", 40005, 40015
	if _, err := s.CreateRule(ctx, owner, "range-overlap-second", overlap); !errors.Is(err, ErrConflict) {
		t.Fatalf("overlap error=%v", err)
	}
	tcp := ruleInput("tcp")
	tcp.NodeID, tcp.ListenPort, tcp.ListenPortEnd = "range-overlap", 42000, 42000
	if _, err := s.CreateRule(ctx, owner, "range-tcp-first", tcp); err != nil {
		t.Fatal(err)
	}
	both := ruleInput("both")
	both.NodeID, both.ListenPort, both.ListenPortEnd = "range-overlap", 42000, 42002
	if _, err := s.CreateRule(ctx, owner, "range-both-conflict", both); !errors.Is(err, ErrConflict) {
		t.Fatalf("both conflict error=%v", err)
	}
	var leaked int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM endpoint_allocations WHERE node_id='range-overlap' AND protocol='udp' AND listen_port BETWEEN 42000 AND 42002").Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("both rollback leaked udp=%d err=%v", leaked, err)
	}
}

func TestCustomRangeNodeEndpointLimitCountsProtocols(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner, _ := register(t, s, "range-capacity@example.com")
	admin, _ := adminIdentity(t, s)
	addDedicatedRangeNode(t, s, admin, owner, "range-capacity")
	large := ruleInput("both")
	large.NodeID, large.ListenPort, large.ListenPortEnd = "range-capacity", 40000, 44095
	if _, err := s.CreateRule(ctx, owner, "range-capacity-first", large); err != nil {
		t.Fatal(err)
	}
	over := ruleInput("udp")
	over.NodeID, over.ListenPort, over.ListenPortEnd = "range-capacity", 45000, 46808
	if _, err := s.CreateRule(ctx, owner, "range-capacity-second", over); !errors.Is(err, ErrCapacity) {
		t.Fatalf("node endpoint limit error=%v", err)
	}
	var endpoints int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM endpoint_allocations WHERE node_id='range-capacity'").Scan(&endpoints); err != nil || endpoints != 8192 {
		t.Fatalf("capacity rollback endpoints=%d err=%v", endpoints, err)
	}
	exact := over
	exact.ListenPortEnd = 46807
	if _, err := s.CreateRule(ctx, owner, "range-capacity-fill", exact); err != nil {
		t.Fatal(err)
	}
	automatic := ruleInput("udp")
	automatic.NodeID = "range-capacity"
	if _, err := s.CreateRule(ctx, owner, "range-capacity-auto", automatic); !errors.Is(err, ErrCapacity) {
		t.Fatalf("automatic allocation bypassed node endpoint limit: %v", err)
	}
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM endpoint_allocations WHERE node_id='range-capacity'").Scan(&endpoints); err != nil || endpoints != 10000 {
		t.Fatalf("exact endpoint limit=%d err=%v", endpoints, err)
	}
}

func TestAutomaticAllocationStillReturnsSinglePortRange(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner, _ := register(t, s, "range-auto@example.com")
	rule, err := s.CreateRule(ctx, owner, "range-auto-key", ruleInput("tcp"))
	if err != nil {
		t.Fatal(err)
	}
	if rule.ListenPort < 20000 || rule.ListenPort > 60000 || rule.ListenPortEnd != rule.ListenPort {
		t.Fatalf("automatic range=%d-%d", rule.ListenPort, rule.ListenPortEnd)
	}
}
