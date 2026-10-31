package platform

import (
	"context"
	"testing"
	"time"
)

func TestOperationalMetricsCountsCurrentFacts(t *testing.T) {
	s := notificationStore(t)
	ctx := context.Background()
	owner, _ := register(t, s, "metrics@example.com")
	if _, err := s.CreateRule(ctx, owner, "metrics-rule", ruleInput("tcp")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE nodes SET certificate_fingerprint=repeat('a',64),last_seen_at=clock_timestamp()-interval '10 minutes' WHERE id='hk-01';UPDATE config_outbox SET state='superseded' WHERE node_id='hk-01' AND generation<(SELECT desired_generation FROM nodes WHERE id='hk-01');UPDATE config_outbox SET state='failed',error_code='test' WHERE node_id='hk-01' AND generation=(SELECT desired_generation FROM nodes WHERE id='hk-01')`); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileIncidents(ctx, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	m, err := s.OperationalMetrics(ctx, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if m.Nodes["unprovisioned"] != 1 || m.AgentContacts["stale"] != 1 || m.Rules["pending-agent"] != 1 || m.CurrentDeliveries["failed"] != 1 || m.CurrentDeliveries["superseded"] != 0 {
		t.Fatalf("snapshot: %+v", m)
	}
	if m.OpenIncidents["contact-stale"] != 1 || m.OpenIncidents["config-failed"] != 1 || m.NotificationOutbox["pending"] < 2 {
		t.Fatalf("operations: %+v", m)
	}
}

func TestOperationalMetricsRejectsUnknownLabels(t *testing.T) {
	counts := newMetricCounts([]string{"ready", "maintenance"})
	if err := counts.add("ready", 2); err != nil {
		t.Fatal(err)
	}
	if err := counts.add("invented", 1); err == nil {
		t.Fatal("accepted unbounded metric label")
	}
}

func TestOperationalMetricsValidatesThreshold(t *testing.T) {
	s := testStore(t)
	if _, err := s.OperationalMetrics(context.Background(), 0); err != ErrInvalid {
		t.Fatalf("threshold err=%v", err)
	}
}

func TestOperationalMetricsAggregatesBoundedNodeResourceStates(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `
INSERT INTO nodes(id,name,region,status,listen_address) VALUES
 ('sg-02','SG','SG','unprovisioned','0.0.0.0'),
 ('us-03','US','US','unprovisioned','0.0.0.0'),
 ('jp-04','JP','JP','unprovisioned','0.0.0.0'),
 ('vn-05','VN','VN','unprovisioned','0.0.0.0');
INSERT INTO node_resource_latest(node_id,sampled_at,received_at,cpu_percent,memory_percent,disk_percent,conntrack_percent,clock_skew_seconds,dataplane_state,cpu_high_since,dataplane_drift_since) VALUES
 ('hk-01',clock_timestamp(),clock_timestamp(),20,30,40,10,0,'in-sync',NULL,NULL),
 ('us-03',clock_timestamp(),clock_timestamp(),95,30,40,10,0,'in-sync',clock_timestamp(),NULL),
 ('jp-04',clock_timestamp(),clock_timestamp(),20,30,40,10,0,'drifted',NULL,clock_timestamp()),
 ('vn-05',clock_timestamp()-interval '5 minutes',clock_timestamp()-interval '5 minutes',20,30,40,10,0,'in-sync',NULL,NULL)`); err != nil {
		t.Fatal(err)
	}
	metrics, err := s.OperationalMetrics(ctx, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{"normal": 1, "unknown": 1, "pressure": 1, "drifted": 1, "stale": 1}
	for state, count := range want {
		if metrics.NodeResources[state] != count {
			t.Fatalf("resource state %s=%d want %d: %+v", state, metrics.NodeResources[state], count, metrics.NodeResources)
		}
	}
}
