package platform

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"reladock/internal/sourceflow"
)

func sourceIngestFixture(t *testing.T) (*Store, sourceflow.Event) {
	t.Helper()
	s, who, ruleID, allocationID := sourceFlowFixture(t)
	s.sourceFlowRetentionDays = 180
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `INSERT INTO applied_source_flow_rules(node_id,config_generation,tenant_id,rule_id,allocation_id,rule_version,source_logging_enabled,protocol,relay_ip,relay_port,target_ip,target_port,manifest_digest)
VALUES('hk-01',7,$1,$2,$3,3,true,'tcp','203.0.113.10',24443,'198.51.100.20',443,$4)`, who.TenantID, ruleID, allocationID, "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"); err != nil {
		t.Fatal(err)
	}
	return s, sourceflow.Event{EventID: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", TenantID: who.TenantID, RuleID: ruleID, NodeID: "hk-01", AllocationID: allocationID, ConfigGeneration: 7, RuleVersion: 3, Protocol: "tcp", SourceIP: "192.0.2.40", SourcePort: 53000, RelayIP: "203.0.113.10", RelayPort: 24443, TargetIP: "198.51.100.20", TargetPort: 443, ObservedAt: time.Now().UTC().Add(-time.Second).Truncate(time.Microsecond), KernelBootID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd", ConntrackID: 42}
}

func TestSourceFlowIngestValidatesHistoricalManifestAndIsIdempotent(t *testing.T) {
	s, event := sourceIngestFixture(t)
	ctx := context.Background()
	batch := sourceflow.Batch{NodeID: "hk-01", Events: []sourceflow.Event{event}}
	receipt, err := s.IngestSourceFlows(ctx, "hk-01", batch)
	if err != nil || len(receipt.EventIDs) != 1 || receipt.EventIDs[0] != event.EventID {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	if replay, replayErr := s.IngestSourceFlows(ctx, "hk-01", batch); replayErr != nil || len(replay.EventIDs) != 1 {
		t.Fatalf("replay=%+v err=%v", replay, replayErr)
	}
	var count int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM source_flow_events WHERE node_id='hk-01' AND event_id=$1`, event.EventID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	changed := batch
	changed.Events = append([]sourceflow.Event(nil), batch.Events...)
	changed.Events[0].TargetPort = 444
	if _, err = s.IngestSourceFlows(ctx, "hk-01", changed); !errors.Is(err, ErrInvalid) {
		t.Fatalf("manifest mismatch accepted: %v", err)
	}
}

func TestSourceFlowIngestIsAtomicAndAcceptsClosedGap(t *testing.T) {
	s, event := sourceIngestFixture(t)
	ctx := context.Background()
	closed := event.ObservedAt.Add(time.Second)
	gap := sourceflow.Gap{NodeID: "hk-01", CollectorEpoch: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", Sequence: 1, Reason: "netlink-overrun", DroppedCount: 2, OpenedAt: event.ObservedAt, ClosedAt: &closed}
	bad := event
	bad.EventID = "ffffffff-ffff-4fff-8fff-ffffffffffff"
	bad.AllocationID = "99999999-9999-4999-8999-999999999999"
	if _, err := s.IngestSourceFlows(ctx, "hk-01", sourceflow.Batch{NodeID: "hk-01", Events: []sourceflow.Event{event, bad}, Gaps: []sourceflow.Gap{gap}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("mixed batch accepted: %v", err)
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM source_flow_events WHERE event_id=$1`, event.EventID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial event count=%d err=%v", count, err)
	}
	receipt, err := s.IngestSourceFlows(ctx, "hk-01", sourceflow.Batch{NodeID: "hk-01", Gaps: []sourceflow.Gap{gap}})
	if err != nil || len(receipt.Gaps) != 1 || receipt.Gaps[0].Sequence != 1 {
		t.Fatalf("gap receipt=%+v err=%v", receipt, err)
	}
}

func TestSourceFlowIngestUpsertsOpenGapProgressAndClosure(t *testing.T) {
	s, event := sourceIngestFixture(t)
	ctx := context.Background()
	gap := sourceflow.Gap{NodeID: "hk-01", CollectorEpoch: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", Sequence: 2, Reason: "queue-full", DroppedCount: 1, OpenedAt: event.ObservedAt}
	if _, err := s.IngestSourceFlows(ctx, "hk-01", sourceflow.Batch{NodeID: "hk-01", Gaps: []sourceflow.Gap{gap}}); err != nil {
		t.Fatal(err)
	}
	gap.DroppedCount = 4
	if _, err := s.IngestSourceFlows(ctx, "hk-01", sourceflow.Batch{NodeID: "hk-01", Gaps: []sourceflow.Gap{gap}}); err != nil {
		t.Fatal(err)
	}
	closed := event.ObservedAt.Add(time.Minute)
	gap.ClosedAt = &closed
	if _, err := s.IngestSourceFlows(ctx, "hk-01", sourceflow.Batch{NodeID: "hk-01", Gaps: []sourceflow.Gap{gap}}); err != nil {
		t.Fatal(err)
	}
	var dropped int64
	var closedAt *time.Time
	if err := s.pool.QueryRow(ctx, `SELECT dropped_count,closed_at FROM source_flow_gaps WHERE node_id='hk-01' AND collector_epoch=$1 AND sequence=2`, gap.CollectorEpoch).Scan(&dropped, &closedAt); err != nil || dropped != 4 || closedAt == nil {
		t.Fatalf("gap dropped=%d closed=%v err=%v", dropped, closedAt, err)
	}
	gap.DroppedCount = 5
	if _, err := s.IngestSourceFlows(ctx, "hk-01", sourceflow.Batch{NodeID: "hk-01", Gaps: []sourceflow.Gap{gap}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("closed gap mutation accepted: %v", err)
	}
}

func TestSourceFlowIngestSerializesConcurrentGapReplayAndClosure(t *testing.T) {
	s, event := sourceIngestFixture(t)
	ctx := context.Background()
	gap := sourceflow.Gap{NodeID: "hk-01", CollectorEpoch: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", Sequence: 4, Reason: "queue-full", DroppedCount: 5, OpenedAt: event.ObservedAt}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := s.IngestSourceFlows(ctx, "hk-01", sourceflow.Batch{NodeID: "hk-01", Gaps: []sourceflow.Gap{gap}})
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent first replay failed: %v", err)
		}
	}
	closed := event.ObservedAt.Add(time.Minute)
	closing := gap
	closing.ClosedAt = &closed
	start = make(chan struct{})
	errs = make(chan error, 2)
	for _, candidate := range []sourceflow.Gap{gap, closing} {
		candidate := candidate
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := s.IngestSourceFlows(ctx, "hk-01", sourceflow.Batch{NodeID: "hk-01", Gaps: []sourceflow.Gap{candidate}})
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil && !errors.Is(err, ErrInvalid) {
			t.Fatalf("concurrent close returned unexpected error: %v", err)
		}
	}
	var dropped int64
	var closedAt *time.Time
	if err := s.pool.QueryRow(ctx, `SELECT dropped_count,closed_at FROM source_flow_gaps WHERE node_id='hk-01' AND collector_epoch=$1 AND sequence=4`, gap.CollectorEpoch).Scan(&dropped, &closedAt); err != nil || dropped != 5 || closedAt == nil {
		t.Fatalf("concurrent final gap dropped=%d closed=%v err=%v", dropped, closedAt, err)
	}
}

func TestSourceFlowQueryIsAdminAuditedBoundedAndRetentionUsesReceivedTime(t *testing.T) {
	s, event := sourceIngestFixture(t)
	ctx := context.Background()
	if _, err := s.IngestSourceFlows(ctx, "hk-01", sourceflow.Batch{NodeID: "hk-01", Events: []sourceflow.Event{event}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPlatformAdmin(ctx, "source-flow@example.com", true); err != nil {
		t.Fatal(err)
	}
	admin, _, err := s.Login(ctx, "source-flow@example.com", "long-test-password-42")
	if err != nil {
		t.Fatal(err)
	}
	to := time.Now().UTC().Add(time.Minute)
	gap := sourceflow.Gap{NodeID: "hk-01", CollectorEpoch: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", Sequence: 3, Reason: "collector-error", DroppedCount: 1, OpenedAt: event.ObservedAt}
	if _, err = s.IngestSourceFlows(ctx, "hk-01", sourceflow.Batch{NodeID: "hk-01", Gaps: []sourceflow.Gap{gap}}); err != nil {
		t.Fatal(err)
	}
	page, err := s.AdminSourceFlows(ctx, admin, SourceFlowQuery{From: to.Add(-time.Hour), To: to, RuleID: event.RuleID, SourceIP: event.SourceIP, Page: 1})
	if err != nil || page.Total != 1 || len(page.Items) != 1 || page.Items[0].SourcePort != event.SourcePort || page.PageSize != 50 || page.RetentionDays != 180 || len(page.Gaps) != 1 || page.Gaps[0].ClosedAt != nil {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	if _, err = s.AdminSourceFlows(ctx, admin, SourceFlowQuery{From: to.Add(-25 * time.Hour), To: to, Page: 1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unbounded query accepted: %v", err)
	}
	if _, err = s.AdminSourceFlows(ctx, admin, SourceFlowQuery{From: to.Add(-time.Hour), To: to, SourceIP: "2001:db8::1", Page: 1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("IPv6 source filter accepted: %v", err)
	}
	var details string
	if err = s.pool.QueryRow(ctx, `SELECT details::text FROM platform_audit_logs WHERE action='source-flow.query' ORDER BY id DESC LIMIT 1`).Scan(&details); err != nil || strings.Contains(details, event.SourceIP) {
		t.Fatalf("privacy audit details=%q err=%v", details, err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE source_flow_events SET received_at=clock_timestamp()-interval '181 days' WHERE node_id='hk-01' AND event_id=$1`, event.EventID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE source_flow_gaps SET received_at=clock_timestamp()-interval '181 days' WHERE node_id='hk-01' AND collector_epoch=$1 AND sequence=3`, gap.CollectorEpoch); err != nil {
		t.Fatal(err)
	}
	deleted, err := s.PruneSourceFlows(ctx, 10)
	if err != nil || deleted < 1 {
		t.Fatalf("deleted=%d err=%v", deleted, err)
	}
	var openGaps int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM source_flow_gaps WHERE node_id='hk-01' AND collector_epoch=$1 AND sequence=3`, gap.CollectorEpoch).Scan(&openGaps); err != nil || openGaps != 1 {
		t.Fatalf("open gap pruned count=%d err=%v", openGaps, err)
	}
}

func TestSourceFlowManifestPruningWaitsForSupersessionRetention(t *testing.T) {
	s, _ := sourceIngestFixture(t)
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `UPDATE applied_source_flow_rules SET applied_at=clock_timestamp()-interval '181 days',superseded_at=clock_timestamp()-interval '181 days' WHERE node_id='hk-01' AND config_generation=7; UPDATE nodes SET applied_generation=7 WHERE id='hk-01'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PruneSourceFlows(ctx, 10); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM applied_source_flow_rules WHERE node_id='hk-01' AND config_generation=7`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("current manifest pruned count=%d err=%v", count, err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE nodes SET applied_generation=8 WHERE id='hk-01'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PruneSourceFlows(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM applied_source_flow_rules WHERE node_id='hk-01' AND config_generation=7`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("superseded manifest retained count=%d err=%v", count, err)
	}
}

func TestSourceFlowPruningIsBoundedAndPreservesNonSourceFacts(t *testing.T) {
	s, event := sourceIngestFixture(t)
	ctx := context.Background()
	if _, err := s.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: event.TenantID, Key: "source-retention-credit", AmountCents: 100, Reason: "retention isolation fixture", Operator: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPlatformAdmin(ctx, "source-flow@example.com", true); err != nil {
		t.Fatal(err)
	}
	events := []sourceflow.Event{event}
	for _, id := range []string{"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa2"} {
		copy := event
		copy.EventID = id
		copy.ConntrackID++
		events = append(events, copy)
	}
	if _, err := s.IngestSourceFlows(ctx, "hk-01", sourceflow.Batch{NodeID: "hk-01", Events: events}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE source_flow_events SET received_at=clock_timestamp()-interval '181 days'`); err != nil {
		t.Fatal(err)
	}
	var rulesBefore, auditsBefore, platformAuditsBefore, walletsBefore, ledgerBefore int
	if err := s.pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM relay_rules),
 (SELECT count(*) FROM audit_logs),
 (SELECT count(*) FROM platform_audit_logs),
 (SELECT count(*) FROM billing_wallets),
 (SELECT count(*) FROM billing_ledger)`).Scan(&rulesBefore, &auditsBefore, &platformAuditsBefore, &walletsBefore, &ledgerBefore); err != nil {
		t.Fatal(err)
	}
	deleted, err := s.PruneSourceFlows(ctx, 2)
	if err != nil || deleted != 2 {
		t.Fatalf("bounded prune deleted=%d err=%v", deleted, err)
	}
	var eventsAfter, rulesAfter, auditsAfter, platformAuditsAfter, walletsAfter, ledgerAfter int
	if err = s.pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM source_flow_events),
 (SELECT count(*) FROM relay_rules),
 (SELECT count(*) FROM audit_logs),
 (SELECT count(*) FROM platform_audit_logs),
 (SELECT count(*) FROM billing_wallets),
 (SELECT count(*) FROM billing_ledger)`).Scan(&eventsAfter, &rulesAfter, &auditsAfter, &platformAuditsAfter, &walletsAfter, &ledgerAfter); err != nil {
		t.Fatal(err)
	}
	if eventsAfter != 1 || rulesAfter != rulesBefore || auditsAfter != auditsBefore || platformAuditsAfter != platformAuditsBefore || walletsAfter != walletsBefore || ledgerAfter != ledgerBefore {
		t.Fatalf("events=%d rules=%d/%d audits=%d/%d platformAudits=%d/%d wallets=%d/%d ledger=%d/%d", eventsAfter, rulesAfter, rulesBefore, auditsAfter, auditsBefore, platformAuditsAfter, platformAuditsBefore, walletsAfter, walletsBefore, ledgerAfter, ledgerBefore)
	}
}

func TestSourceFlowCollectorIncidentUsesSustainedDegradeAndRecovery(t *testing.T) {
	s, _, _, _ := sourceFlowFixture(t)
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `UPDATE nodes SET certificate_fingerprint=$2,status='ready' WHERE id=$1`, "hk-01", strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO node_source_flow_latest(node_id,collector_status,subscription_alive,backlog_count,oldest_backlog_at,wal_bytes_used,wal_bytes_limit,dropped_total,open_gap,collector_epoch,collector_sequence,observed_at,received_at,degraded_since,normal_since)
VALUES('hk-01','degraded',true,1,clock_timestamp()-interval '3 minutes',900,1000,1,true,$1,1,clock_timestamp(),clock_timestamp(),clock_timestamp()-interval '3 minutes',NULL)`, "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileIncidents(ctx, DefaultContactStaleAfter); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := s.pool.QueryRow(ctx, `SELECT state FROM operations_incidents WHERE node_id='hk-01' AND kind='source-flow-collector-degraded'`).Scan(&state); err != nil || state != "open" {
		t.Fatalf("degraded incident state=%q err=%v", state, err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE node_source_flow_latest SET collector_status='normal',degraded_since=NULL,normal_since=clock_timestamp()-interval '3 minutes',open_gap=false,backlog_count=0,oldest_backlog_at=NULL,wal_bytes_used=100 WHERE node_id='hk-01'`); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileIncidents(ctx, DefaultContactStaleAfter); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT state FROM operations_incidents WHERE node_id='hk-01' AND kind='source-flow-collector-degraded'`).Scan(&state); err != nil || state != "resolved" {
		t.Fatalf("resolved incident state=%q err=%v", state, err)
	}
}
