package platform

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func operationsStore(t *testing.T) *Store {
	t.Helper()
	s := testStore(t)
	ctx := context.Background()
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT to_regclass('operations_incidents') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		sql, err := os.ReadFile("migration8.sql")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.pool.Exec(ctx, string(sql)); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestOperationsIncidentDedupRecoveryAndUnknownContacts(t *testing.T) {
	s := operationsStore(t)
	ctx := context.Background()
	owner, _ := register(t, s, "ops-owner@example.com")
	if _, err := s.CreateRule(ctx, owner, "ops-rule-create", ruleInput("tcp")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE nodes SET certificate_fingerprint=$1,last_seen_at=clock_timestamp()-interval '1 hour' WHERE id='hk-01'`, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE config_outbox SET state='failed',error_code='invalid-config' WHERE node_id='hk-01' AND generation=(SELECT desired_generation FROM nodes WHERE id='hk-01')`); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- s.ReconcileIncidents(ctx, 5*time.Minute) }()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	var count, events int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM operations_incidents WHERE state='open'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM platform_audit_logs WHERE action='incident.open'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if count != 2 || events != 2 {
		t.Fatalf("duplicate incidents/audits: incidents=%d events=%d", count, events)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE nodes SET last_seen_at=clock_timestamp() WHERE id='hk-01'; UPDATE config_outbox SET state='validated' WHERE node_id='hk-01'`); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileIncidents(ctx, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM operations_incidents WHERE state='resolved' AND resolved_at IS NOT NULL`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("recovery: %d %v", count, err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE nodes SET last_seen_at=NULL WHERE id='hk-01'`); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileIncidents(ctx, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	nodes, err := s.ListOperationsNodes(ctx, owner, false, 5*time.Minute, 1)
	if err != nil || len(nodes.Items) != 1 || nodes.Items[0].ContactStatus != "never-seen" || nodes.Items[0].DataPlaneStatus != "unknown" {
		t.Fatalf("unknown contact: %+v %v", nodes, err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE nodes SET last_seen_at=clock_timestamp()-interval '1 hour',status='maintenance' WHERE id='hk-01'`); err != nil {
		t.Fatal(err)
	}
	if err = s.ReconcileIncidents(ctx, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM operations_incidents WHERE state='open'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("maintenance alert: %d %v", count, err)
	}
}

func TestOperationsTenantVisibilityAndSupportIsolation(t *testing.T) {
	s := operationsStore(t)
	ctx := context.Background()
	a, _ := register(t, s, "ops-a@example.com")
	b, _ := register(t, s, "ops-b@example.com")
	if _, err := s.CreateRule(ctx, a, "ops-a-rule", ruleInput("udp")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE nodes SET certificate_fingerprint=$1,last_seen_at=clock_timestamp()-interval '1 hour' WHERE id='hk-01'`, strings.Repeat("b", 64)); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileIncidents(ctx, time.Minute); err != nil {
		t.Fatal(err)
	}
	incidents, err := s.ListIncidents(ctx, b, false, 1)
	if err != nil || incidents.Total != 0 {
		t.Fatalf("incident tenant leak: %+v %v", incidents, err)
	}
	nodes, err := s.ListOperationsNodes(ctx, b, false, time.Minute, 1)
	if err != nil || nodes.Total != 0 {
		t.Fatalf("nodes tenant leak: %+v %v", nodes, err)
	}
	incidents, err = s.ListIncidents(ctx, a, false, 1)
	if err != nil || incidents.Total != 1 || len(incidents.Items[0].RuleIDs) != 1 {
		t.Fatalf("affected rule visibility: %+v %v", incidents, err)
	}
	ticket, err := s.CreateSupportTicket(ctx, a, "Delivery question", "Please investigate control delivery.")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SupportThread(ctx, b, ticket.ID, false); err != ErrNotFound {
		t.Fatalf("other tenant read thread: %v", err)
	}
	if _, err = s.ReplySupportTicket(ctx, b, ticket.ID, "attack", "", ticket.Revision, false); err != ErrNotFound {
		t.Fatalf("other tenant reply: %v", err)
	}
	if err = s.SetPlatformAdmin(ctx, b.Email, true); err != nil {
		t.Fatal(err)
	}
	b.PlatformAdmin = true
	updated, err := s.ReplySupportTicket(ctx, b, ticket.ID, "Investigating; please send timestamps.", "waiting-customer", ticket.Revision, true)
	if err != nil || updated.Status != "waiting-customer" {
		t.Fatalf("admin reply: %+v %v", updated, err)
	}
	if _, err = s.ReplySupportTicket(ctx, a, ticket.ID, "stale response", "", ticket.Revision, false); err != ErrConflict {
		t.Fatalf("stale ticket overwrite: %v", err)
	}
	updated, err = s.ReplySupportTicket(ctx, a, ticket.ID, "Here are the timestamps.", "", updated.Revision, false)
	if err != nil || updated.Status != "open" {
		t.Fatalf("tenant reply reopen: %+v %v", updated, err)
	}
	thread, err := s.SupportThread(ctx, a, ticket.ID, false)
	if err != nil || len(thread.Messages) != 3 || thread.Messages[1].AuthorKind != "platform" {
		t.Fatalf("persisted thread: %+v %v", thread, err)
	}
	if _, err = s.ListIncidents(WithAPIKey(ctx, "fake"), b, true, 1); err != ErrForbidden {
		t.Fatalf("key admin bypass: %v", err)
	}
}

func TestOperationsAuditFailuresRollbackIncidentAndTicket(t *testing.T) {
	s := operationsStore(t)
	ctx := context.Background()
	owner, _ := register(t, s, "ops-audit@example.com")
	if _, err := s.pool.Exec(ctx, `UPDATE nodes SET certificate_fingerprint=$1,last_seen_at=clock_timestamp()-interval '1 hour' WHERE id='hk-01'`, strings.Repeat("c", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `ALTER TABLE platform_audit_logs ADD CONSTRAINT fail_incident CHECK(action NOT LIKE 'incident.%') NOT VALID; ALTER TABLE audit_logs ADD CONSTRAINT fail_support CHECK(action NOT LIKE 'support.%') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileIncidents(ctx, time.Minute); err == nil {
		t.Fatal("incident ignored failed audit")
	}
	if _, err := s.CreateSupportTicket(ctx, owner, "Rollback", "Must not persist without audit."); err == nil {
		t.Fatal("ticket ignored failed audit")
	}
	var incidents, tickets, messages int
	if err := s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM operations_incidents),(SELECT count(*) FROM support_tickets),(SELECT count(*) FROM support_messages)`).Scan(&incidents, &tickets, &messages); err != nil {
		t.Fatal(err)
	}
	if incidents != 0 || tickets != 0 || messages != 0 {
		t.Fatalf("audit rollback leaked data: %d %d %d", incidents, tickets, messages)
	}
}

func TestOperationsRecurrenceIgnoresHistoricalFailureAndUnenrolledNodes(t *testing.T) {
	s := operationsStore(t)
	ctx := context.Background()
	owner, _ := register(t, s, "ops-recur@example.com")
	if _, err := s.CreateRule(ctx, owner, "ops-recur-rule", ruleInput("tcp")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE nodes SET certificate_fingerprint=$1,last_seen_at=clock_timestamp()-interval '1 hour' WHERE id='hk-01'`, strings.Repeat("d", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE config_outbox SET state='failed' WHERE node_id='hk-01'`); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileIncidents(ctx, time.Minute); err != nil {
		t.Fatal(err)
	}
	var contactID string
	if err := s.pool.QueryRow(ctx, `SELECT id FROM operations_incidents WHERE kind='contact-stale'`).Scan(&contactID); err != nil {
		t.Fatal(err)
	}
	// Queue a newer generation: the old failure is no longer actionable.
	if _, err := s.CreateRule(ctx, owner, "ops-recur-new-rule", ruleInput("udp")); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileIncidents(ctx, time.Minute); err != nil {
		t.Fatal(err)
	}
	var configState string
	if err := s.pool.QueryRow(ctx, `SELECT state FROM operations_incidents WHERE kind='config-failed'`).Scan(&configState); err != nil || configState != "resolved" {
		t.Fatalf("historical failure kept alert open: %s %v", configState, err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE nodes SET last_seen_at=clock_timestamp() WHERE id='hk-01'`); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileIncidents(ctx, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE nodes SET last_seen_at=clock_timestamp()-interval '1 hour' WHERE id='hk-01'`); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileIncidents(ctx, time.Minute); err != nil {
		t.Fatal(err)
	}
	var occurrences int
	var reopenedID string
	if err := s.pool.QueryRow(ctx, `SELECT id,occurrences FROM operations_incidents WHERE kind='contact-stale' AND state='open'`).Scan(&reopenedID, &occurrences); err != nil || reopenedID != contactID || occurrences != 2 {
		t.Fatalf("recurrence: %s %d %v", reopenedID, occurrences, err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE nodes SET certificate_fingerprint=NULL WHERE id='hk-01'`); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileIncidents(ctx, time.Minute); err != nil {
		t.Fatal(err)
	}
	nodes, err := s.ListOperationsNodes(ctx, owner, false, time.Minute, 1)
	if err != nil || nodes.Items[0].ContactStatus != "unknown" {
		t.Fatalf("unenrolled contact: %+v %v", nodes, err)
	}
	var open int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM operations_incidents WHERE state='open'`).Scan(&open); err != nil || open != 0 {
		t.Fatalf("unenrolled incident: %d %v", open, err)
	}
}

func TestOperationsKeyScopesRevocationAndBoundedSupportLists(t *testing.T) {
	s := operationsStore(t)
	ctx := context.Background()
	owner, _ := register(t, s, "ops-key@example.com")
	_, reader, err := s.CreateAPIKey(ctx, owner, "ops reader", "read", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	readWho, err := s.APIKey(ctx, reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateSupportTicket(WithAPIKey(ctx, reader), readWho, "Denied", "Read keys cannot create support tickets."); err != ErrForbidden {
		t.Fatalf("read key write: %v", err)
	}
	key, writer, err := s.CreateAPIKey(ctx, owner, "ops writer", "write", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	writeWho, err := s.APIKey(ctx, writer)
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := s.CreateSupportTicket(WithAPIKey(ctx, writer), writeWho, "Allowed", "Write-scoped key support request.")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SupportThread(WithAPIKey(ctx, reader), readWho, ticket.ID, false); err != nil {
		t.Fatalf("read scope cannot read thread: %v", err)
	}
	if err = s.RevokeAPIKey(ctx, owner, key.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReplySupportTicket(WithAPIKey(ctx, writer), writeWho, ticket.ID, "stale write", "", ticket.Revision, false); err != ErrUnauthorized {
		t.Fatalf("stale revoked key wrote reply: %v", err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO support_tickets(id,tenant_id,created_by,subject) SELECT md5('bounded-ticket-'||g)::uuid,$1,$2,'bounded' FROM generate_series(1,100) g`, owner.TenantID, owner.MemberID); err != nil {
		t.Fatal(err)
	}
	page, err := s.ListSupportTickets(ctx, owner, false, 1)
	if err != nil || len(page.Items) != 100 || page.Total != 101 {
		t.Fatalf("list bound: %d %d %v", len(page.Items), page.Total, err)
	}
	page, err = s.ListSupportTickets(ctx, owner, false, 2)
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("list continuation: %d %v", len(page.Items), err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO support_messages(ticket_id,tenant_id,author_id,author_kind,body) SELECT $1,$2,$3,'tenant','message-'||g FROM generate_series(1,100) g`, ticket.ID, owner.TenantID, owner.MemberID); err != nil {
		t.Fatal(err)
	}
	thread, err := s.SupportThread(ctx, owner, ticket.ID, false)
	if err != nil || len(thread.Messages) != 100 || thread.Messages[0].Body != "message-1" || thread.Messages[99].Body != "message-100" {
		t.Fatalf("thread bound/order: %d %v", len(thread.Messages), err)
	}
}
