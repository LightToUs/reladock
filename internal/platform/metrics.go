package platform

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

type metricCounts map[string]int64

func newMetricCounts(labels []string) metricCounts {
	m := metricCounts{}
	for _, label := range labels {
		m[label] = 0
	}
	return m
}
func (m metricCounts) add(label string, count int64) error {
	if _, ok := m[label]; !ok {
		return fmt.Errorf("unknown operational metric state %q", label)
	}
	if count < 0 {
		return fmt.Errorf("negative operational metric count")
	}
	m[label] = count
	return nil
}

type OperationalMetrics struct {
	Nodes               map[string]int64
	AgentContacts       map[string]int64
	NodeResources       map[string]int64
	Rules               map[string]int64
	CurrentDeliveries   map[string]int64
	OpenIncidents       map[string]int64
	NotificationOutbox  map[string]int64
	AgentUpgrades       map[string]int64
	EndpointRetirements map[string]int64
	FrozenWallets       int64
}

func loadMetricCounts(ctx context.Context, tx pgx.Tx, query string, args []any, counts metricCounts) error {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var label string
		var count int64
		if err = rows.Scan(&label, &count); err != nil {
			return err
		}
		if err = counts.add(label, count); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (s *Store) OperationalMetrics(ctx context.Context, staleAfter time.Duration) (OperationalMetrics, error) {
	var result OperationalMetrics
	if !validStaleAfter(staleAfter) {
		return result, ErrInvalid
	}
	nodes := newMetricCounts([]string{"unprovisioned", "ready", "maintenance", "draining", "retired"})
	contacts := newMetricCounts([]string{"unenrolled", "never-seen", "recent", "stale"})
	resources := newMetricCounts([]string{"normal", "pressure", "drifted", "unknown", "stale"})
	rules := newMetricCounts([]string{"pending-agent", "paused", "deleted"})
	deliveries := newMetricCounts([]string{"queued", "leased", "applying", "applied", "validated", "failed", "superseded"})
	incidents := newMetricCounts([]string{"contact-stale", "config-failed", "certificate-expiring", "certificate-expired", "certificate-rotation-failed", "resource-cpu-high", "resource-memory-high", "resource-disk-high", "resource-conntrack-high", "resource-clock-skew", "dataplane-drift"})
	notifications := newMetricCounts([]string{"pending", "claimed", "delivered", "exhausted"})
	upgrades := newMetricCounts([]string{"queued", "claimed", "installing", "verifying", "succeeded", "failed", "rolled-back", "cancelled"})
	retirements := newMetricCounts([]string{"waiting-removal", "queued", "claimed", "purging", "released", "failed"})
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	queries := []struct {
		sql    string
		args   []any
		counts metricCounts
	}{
		{`SELECT status,count(*) FROM nodes GROUP BY status`, nil, nodes},
		{`SELECT CASE WHEN certificate_fingerprint IS NULL THEN 'unenrolled' WHEN last_seen_at IS NULL THEN 'never-seen' WHEN last_seen_at<now()-$1*interval '1 second' THEN 'stale' ELSE 'recent' END,count(*) FROM nodes GROUP BY 1`, []any{staleAfter.Seconds()}, contacts},
		{`SELECT CASE WHEN rs.node_id IS NULL THEN 'unknown' WHEN rs.received_at<now()-interval '90 seconds' THEN 'stale' WHEN rs.dataplane_state='drifted' THEN 'drifted' WHEN COALESCE(rs.cpu_percent>=90,false) OR COALESCE(rs.memory_percent>=90,false) OR COALESCE(rs.disk_percent>=85,false) OR COALESCE(rs.conntrack_percent>=80,false) OR rs.clock_skew_seconds>120 THEN 'pressure' WHEN rs.cpu_percent IS NULL AND rs.memory_percent IS NULL AND rs.disk_percent IS NULL AND rs.conntrack_percent IS NULL THEN 'unknown' ELSE 'normal' END,count(*) FROM nodes n LEFT JOIN node_resource_latest rs ON rs.node_id=n.id GROUP BY 1`, nil, resources},
		{`SELECT status,count(*) FROM relay_rules GROUP BY status`, nil, rules},
		{`SELECT o.state,count(*) FROM nodes n JOIN config_outbox o ON o.node_id=n.id AND o.generation=n.desired_generation GROUP BY o.state`, nil, deliveries},
		{`SELECT kind,count(*) FROM operations_incidents WHERE state='open' GROUP BY kind`, nil, incidents},
		{`SELECT state,count(*) FROM notification_outbox GROUP BY state`, nil, notifications},
		{`SELECT state,count(*) FROM node_agent_upgrades GROUP BY state`, nil, upgrades},
		{`SELECT state,count(*) FROM endpoint_retirements GROUP BY state`, nil, retirements},
	}
	for _, q := range queries {
		if err = loadMetricCounts(ctx, tx, q.sql, q.args, q.counts); err != nil {
			return result, err
		}
	}
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM billing_wallets WHERE spending_frozen`).Scan(&result.FrozenWallets); err != nil {
		return result, err
	}
	if err = tx.Commit(ctx); err != nil {
		return result, err
	}
	result.Nodes = map[string]int64(nodes)
	result.AgentContacts = map[string]int64(contacts)
	result.NodeResources = map[string]int64(resources)
	result.Rules = map[string]int64(rules)
	result.CurrentDeliveries = map[string]int64(deliveries)
	result.OpenIncidents = map[string]int64(incidents)
	result.NotificationOutbox = map[string]int64(notifications)
	result.AgentUpgrades = map[string]int64(upgrades)
	result.EndpointRetirements = map[string]int64(retirements)
	return result, nil
}
