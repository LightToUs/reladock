package platform

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func sourceFlowFixture(t *testing.T) (*Store, Identity, string, string) {
	t.Helper()
	s := testStore(t)
	who, _ := register(t, s, "source-flow@example.com")
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `UPDATE nodes SET listen_address='203.0.113.10',public_address='203.0.113.10' WHERE id='hk-01'`); err != nil {
		t.Fatal(err)
	}
	ruleID := newID()
	if _, err := s.pool.Exec(ctx, `INSERT INTO relay_rules(
		id,tenant_id,node_id,name,protocol,target_host,target_port,bandwidth_mbps,listen_port,status
	) VALUES($1,$2,'hk-01','source flow','tcp','198.51.100.20',443,100,24443,'pending-agent')`, ruleID, who.TenantID); err != nil {
		t.Fatal(err)
	}
	var allocationID string
	if err := s.pool.QueryRow(ctx, `INSERT INTO endpoint_allocations(
		node_id,listen_address,protocol,listen_port,rule_id,tenant_id
	) VALUES('hk-01','203.0.113.10','tcp',24443,$1,$2) RETURNING allocation_id::text`, ruleID, who.TenantID).Scan(&allocationID); err != nil {
		t.Fatal(err)
	}
	return s, who, ruleID, allocationID
}

func TestMigration34SourceFlowSchema(t *testing.T) {
	s, who, ruleID, allocationID := sourceFlowFixture(t)
	ctx := context.Background()

	var version int
	var requested, applied, supported bool
	var appliedVersion, appliedGeneration int64
	if err := s.pool.QueryRow(ctx, `SELECT
		(SELECT max(version) FROM schema_migrations),
		source_logging_enabled,source_logging_applied_enabled,
		source_logging_applied_version,source_logging_applied_generation,
		(SELECT agent_source_flow_supported FROM nodes WHERE id='hk-01')
		FROM relay_rules WHERE id=$1`, ruleID).Scan(&version, &requested, &applied, &appliedVersion, &appliedGeneration, &supported); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion || requested || applied || appliedVersion != 0 || appliedGeneration != 0 || supported {
		t.Fatalf("source-flow defaults: version=%d requested=%v applied=%v appliedVersion=%d appliedGeneration=%d supported=%v", version, requested, applied, appliedVersion, appliedGeneration, supported)
	}

	indexNames := []string{
		"source_flow_events_rule_observed",
		"source_flow_events_source_observed",
		"source_flow_events_node_observed",
	}
	for _, name := range indexNames {
		var exists bool
		if err := s.pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&exists); err != nil || !exists {
			t.Fatalf("index %s exists=%v err=%v", name, exists, err)
		}
	}
	for _, table := range []string{"node_source_flow_latest", "source_flow_events", "source_flow_gaps", "applied_source_flow_rules"} {
		var exists bool
		if err := s.pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil || !exists {
			t.Fatalf("table %s exists=%v err=%v", table, exists, err)
		}
	}

	// A capability belongs to the currently authenticated certificate identity.
	fingerprintA := strings.Repeat("a", 64)
	fingerprintB := strings.Repeat("b", 64)
	if _, err := s.pool.Exec(ctx, `UPDATE nodes SET certificate_fingerprint=$1,agent_source_flow_supported=true WHERE id='hk-01'`, fingerprintA); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE nodes SET certificate_fingerprint=$1 WHERE id='hk-01'`, fingerprintB); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT agent_source_flow_supported FROM nodes WHERE id='hk-01'`).Scan(&supported); err != nil || supported {
		t.Fatalf("source-flow capability survived certificate replacement: supported=%v err=%v", supported, err)
	}

	// The manifest is immutable evidence for delayed event validation.
	digest := strings.Repeat("c", 64)
	if _, err := s.pool.Exec(ctx, `INSERT INTO applied_source_flow_rules(
		node_id,config_generation,tenant_id,rule_id,allocation_id,rule_version,
		source_logging_enabled,protocol,relay_ip,relay_port,target_ip,target_port,manifest_digest
	) VALUES('hk-01',7,$1,$2,$3,3,true,'tcp','203.0.113.10',24443,'198.51.100.20',443,$4)`, who.TenantID, ruleID, allocationID, digest); err != nil {
		t.Fatal(err)
	}

	// Re-running migration must preserve source-flow rows as well as v33 data.
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var manifests int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM applied_source_flow_rules WHERE node_id='hk-01' AND config_generation=7`).Scan(&manifests); err != nil || manifests != 1 {
		t.Fatalf("rerun manifests=%d err=%v", manifests, err)
	}
}

func TestSourceFlowSchemaConstraints(t *testing.T) {
	s, who, ruleID, allocationID := sourceFlowFixture(t)
	ctx := context.Background()
	digest := strings.Repeat("d", 64)
	if _, err := s.pool.Exec(ctx, `INSERT INTO applied_source_flow_rules(
		node_id,config_generation,tenant_id,rule_id,allocation_id,rule_version,
		source_logging_enabled,protocol,relay_ip,relay_port,target_ip,target_port,manifest_digest
	) VALUES('hk-01',9,$1,$2,$3,4,true,'tcp','203.0.113.10',24443,'198.51.100.20',443,$4)`, who.TenantID, ruleID, allocationID, digest); err != nil {
		t.Fatal(err)
	}
	eventID := newID()
	bootID := newID()
	insertEvent := `INSERT INTO source_flow_events(
		node_id,event_id,tenant_id,rule_id,allocation_id,config_generation,rule_version,
		protocol,source_ip,source_port,relay_ip,relay_port,target_ip,target_port,
		observed_at,kernel_boot_id,conntrack_zone,conntrack_id
	) VALUES('hk-01',$1,$2,$3,$4,9,4,$5,$6,$7,'203.0.113.10',24443,'198.51.100.20',443,$8,$9,0,42)`
	if _, err := s.pool.Exec(ctx, insertEvent, eventID, who.TenantID, ruleID, allocationID, "tcp", "192.0.2.40", 53000, time.Now().UTC(), bootID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, insertEvent, eventID, who.TenantID, ruleID, allocationID, "tcp", "192.0.2.40", 53000, time.Now().UTC(), bootID); err == nil {
		t.Fatal("duplicate node/event identity accepted")
	}
	for name, args := range map[string][]any{
		"protocol":       {newID(), who.TenantID, ruleID, allocationID, "icmp", "192.0.2.40", 53000, time.Now().UTC(), bootID},
		"source port":    {newID(), who.TenantID, ruleID, allocationID, "tcp", "192.0.2.40", 0, time.Now().UTC(), bootID},
		"source address": {newID(), who.TenantID, ruleID, allocationID, "tcp", "not-an-address", 53000, time.Now().UTC(), bootID},
	} {
		if _, err := s.pool.Exec(ctx, insertEvent, args...); err == nil {
			t.Fatalf("invalid %s accepted", name)
		}
	}

	validGap := `INSERT INTO source_flow_gaps(node_id,collector_epoch,sequence,reason,dropped_count,opened_at,closed_at)
		VALUES('hk-01',$1,1,'queue-full',12,$2,$3)`
	now := time.Now().UTC()
	if _, err := s.pool.Exec(ctx, validGap, newID(), now.Add(-time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO source_flow_gaps(node_id,collector_epoch,sequence,reason,dropped_count,opened_at)
		VALUES('hk-01',$1,2,'made-up',1,$2)`, newID(), now); err == nil {
		t.Fatal("unknown gap reason accepted")
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO source_flow_gaps(node_id,collector_epoch,sequence,reason,dropped_count,opened_at,closed_at)
		VALUES('hk-01',$1,3,'wal-full',1,$2,$3)`, newID(), now, now.Add(-time.Minute)); err == nil {
		t.Fatal("gap closed before it opened")
	}
}

func TestSourceFlowSchemaDomainTypesContainMetadataOnly(t *testing.T) {
	value := SourceFlowEvent{
		EventID: newID(), SourceIP: "192.0.2.1", SourcePort: 50000,
		RelayIP: "203.0.113.10", RelayPort: 24443,
		TargetIP: "198.51.100.20", TargetPort: 443,
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"payload", "dns", "sni", "url", "packet"} {
		if strings.Contains(strings.ToLower(string(raw)), forbidden) {
			t.Fatalf("source-flow event exposed forbidden content field %q: %s", forbidden, raw)
		}
	}
	_ = SourceFlowGap{}
	_ = AppliedSourceFlowRule{}
	_ = SourceFlowStatus{}
}
