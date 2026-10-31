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
