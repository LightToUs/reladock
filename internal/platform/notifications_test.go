package platform

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func notificationStore(t *testing.T) *Store {
	t.Helper()
	s := operationsStore(t)
	var exists bool
	if e := s.pool.QueryRow(context.Background(), "SELECT to_regclass('notification_outbox') IS NOT NULL").Scan(&exists); e != nil {
		t.Fatal(e)
	}
	if !exists {
		raw, e := os.ReadFile("migration15.sql")
		if e != nil {
			t.Fatal(e)
		}
		if _, e = s.pool.Exec(context.Background(), string(raw)); e != nil {
			t.Fatal(e)
		}
	}
	return s
}
func notificationIncident(t *testing.T, s *Store) string {
	t.Helper()
	id := newID()
	if _, e := s.pool.Exec(context.Background(), "INSERT INTO operations_incidents(id,node_id,kind,state) VALUES($1,'hk-01','contact-stale','open')", id); e != nil {
		t.Fatal(e)
	}
	return id
}
func notificationCount(t *testing.T, s *Store, want int) {
	t.Helper()
	var n int
	if e := s.pool.QueryRow(context.Background(), "SELECT count(*) FROM notification_outbox").Scan(&n); e != nil || n != want {
		t.Fatalf("outbox=%d want=%d error=%v", n, want, e)
	}
}

func TestNotificationsKeepShortTransitionsAndDeduplicateObservations(t *testing.T) {
	s := notificationStore(t)
	ctx := context.Background()
	id := notificationIncident(t, s)
	if _, e := s.pool.Exec(ctx, "UPDATE operations_incidents SET observed_at=clock_timestamp() WHERE id=$1", id); e != nil {
		t.Fatal(e)
	}
	notificationCount(t, s, 1)
	if _, e := s.pool.Exec(ctx, "UPDATE operations_incidents SET state='resolved',resolved_at=clock_timestamp() WHERE id=$1", id); e != nil {
		t.Fatal(e)
	}
	if _, e := s.pool.Exec(ctx, "UPDATE operations_incidents SET state='open',resolved_at=NULL,occurrences=occurrences+1,opened_at=clock_timestamp() WHERE id=$1", id); e != nil {
		t.Fatal(e)
	}
	notificationCount(t, s, 3)
	for i := 0; i < 2; i++ {
		if n, e := s.EnqueueIncidentNotifications(ctx, 100); e != nil || n != 0 {
			t.Fatalf("poll duplicate=%d %v", n, e)
		}
	}
	states := []string{}
	for i := 0; i < 3; i++ {
		n, e := s.ClaimNotification(ctx)
		if e != nil || n == nil {
			t.Fatalf("claim %+v %v", n, e)
		}
		states = append(states, n.Event.State)
		raw, e := json.Marshal(n.Event)
		if e != nil {
			t.Fatal(e)
		}
		for _, bad := range []string{"leaseToken", "tenantId", "ruleId", "targetHost", "subject", "body", "email"} {
			if strings.Contains(string(raw), bad) {
				t.Fatalf("private field in event: %s", raw)
			}
		}
		if e = s.ReportNotification(ctx, n.Event.EventID, n.LeaseToken, true, ""); e != nil {
			t.Fatal(e)
		}
	}
	if strings.Join(states, ",") != "open,resolved,open" {
		t.Fatalf("lost event history %v", states)
	}
}

func TestNotificationsTransitionAndAuditRollbackTogether(t *testing.T) {
	s := notificationStore(t)
	ctx := context.Background()
	if _, e := s.pool.Exec(ctx, "UPDATE nodes SET certificate_fingerprint=$1,last_seen_at=clock_timestamp()-interval '1 hour' WHERE id='hk-01'", strings.Repeat("a", 64)); e != nil {
		t.Fatal(e)
	}
	if _, e := s.pool.Exec(ctx, "ALTER TABLE notification_outbox ADD CONSTRAINT reject_notification CHECK(false) NOT VALID"); e != nil {
		t.Fatal(e)
	}
	if e := s.ReconcileIncidents(ctx, time.Minute); e == nil {
		t.Fatal("incident committed without durable notification")
	}
	var n int
	if e := s.pool.QueryRow(ctx, "SELECT count(*) FROM operations_incidents").Scan(&n); e != nil || n != 0 {
		t.Fatalf("incident leaked after enqueue failure: %d %v", n, e)
	}
	if _, e := s.pool.Exec(ctx, "ALTER TABLE notification_outbox DROP CONSTRAINT reject_notification"); e != nil {
		t.Fatal(e)
	}
	if _, e := s.pool.Exec(ctx, "ALTER TABLE platform_audit_logs ADD CONSTRAINT reject_notification_audit CHECK(action NOT LIKE 'incident.%') NOT VALID"); e != nil {
		t.Fatal(e)
	}
	if e := s.ReconcileIncidents(ctx, time.Minute); e == nil {
		t.Fatal("incident committed despite failed audit")
	}
	notificationCount(t, s, 0)
}

func TestNotificationsConcurrentClaimRestartAndFencedReports(t *testing.T) {
	s := notificationStore(t)
	ctx := context.Background()
	notificationIncident(t, s)
	var wg sync.WaitGroup
	claims := make(chan *Notification, 12)
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); n, e := s.ClaimNotification(ctx); claims <- n; errs <- e }()
	}
	wg.Wait()
	close(claims)
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	var first *Notification
	for n := range claims {
		if n != nil {
			if first != nil {
				t.Fatal("concurrent workers claimed same lease")
			}
			first = n
		}
	}
	if first == nil {
		t.Fatal("no claim")
	}
	if _, e := s.pool.Exec(ctx, "UPDATE notification_outbox SET lease_until=clock_timestamp()-interval '1 second' WHERE event_id=$1", first.Event.EventID); e != nil {
		t.Fatal(e)
	}
	if e := s.ReportNotification(ctx, first.Event.EventID, first.LeaseToken, true, ""); !errors.Is(e, ErrConflict) {
		t.Fatalf("expired acknowledgement accepted %v", e)
	}
	restarted := &Store{pool: s.pool}
	next, e := restarted.ClaimNotification(ctx)
	if e != nil || next == nil || next.Event.EventID != first.Event.EventID || next.LeaseToken == first.LeaseToken || next.Attempts != 2 {
		t.Fatalf("restart claim %+v %v", next, e)
	}
	if e = s.ReportNotification(ctx, first.Event.EventID, first.LeaseToken, true, ""); !errors.Is(e, ErrConflict) {
		t.Fatalf("stale worker report %v", e)
	}
	for i := 0; i < 2; i++ {
		if e = s.ReportNotification(ctx, next.Event.EventID, next.LeaseToken, true, ""); e != nil {
			t.Fatal(e)
		}
	}
	if n, e := s.ClaimNotification(ctx); e != nil || n != nil {
		t.Fatalf("delivered requeued %+v %v", n, e)
	}
}

func TestNotificationsBoundedRetryAndNeverPretendDelivered(t *testing.T) {
	s := notificationStore(t)
	ctx := context.Background()
	notificationIncident(t, s)
	for attempt := 1; attempt <= NotificationMaxAttempts; attempt++ {
		n, e := s.ClaimNotification(ctx)
		if e != nil || n == nil || n.Attempts != attempt {
			t.Fatalf("attempt %d claim %+v %v", attempt, n, e)
		}
		for i := 0; i < 2; i++ {
			if e = s.ReportNotification(ctx, n.Event.EventID, n.LeaseToken, false, "delivery-failed"); e != nil {
				t.Fatal(e)
			}
		}
		if n, e := s.ClaimNotification(ctx); e != nil || n != nil {
			t.Fatalf("backoff skipped %+v %v", n, e)
		}
		if _, e = s.pool.Exec(ctx, "UPDATE notification_outbox SET next_attempt_at=clock_timestamp()-interval '1 second' WHERE state='pending'"); e != nil {
			t.Fatal(e)
		}
	}
	var state string
	var delivered bool
	if e := s.pool.QueryRow(ctx, "SELECT state,delivered_at IS NOT NULL FROM notification_outbox").Scan(&state, &delivered); e != nil || state != "exhausted" || delivered {
		t.Fatalf("exhaustion state=%s delivered=%v error=%v", state, delivered, e)
	}
}

func TestNotificationsFinalClaimCrashExhaustsWithoutAcknowledgement(t *testing.T) {
	s := notificationStore(t)
	ctx := context.Background()
	notificationIncident(t, s)
	for i := 0; i < NotificationMaxAttempts; i++ {
		n, e := s.ClaimNotification(ctx)
		if e != nil || n == nil {
			t.Fatalf("claim %d %+v %v", i, n, e)
		}
		if _, e = s.pool.Exec(ctx, "UPDATE notification_outbox SET lease_until=clock_timestamp()-interval '1 second' WHERE event_id=$1", n.Event.EventID); e != nil {
			t.Fatal(e)
		}
	}
	if n, e := s.ClaimNotification(ctx); e != nil || n != nil {
		t.Fatalf("last crash reclaimed indefinitely %+v %v", n, e)
	}
	var state, code string
	var delivered bool
	if e := s.pool.QueryRow(ctx, "SELECT state,last_error_code,delivered_at IS NOT NULL FROM notification_outbox").Scan(&state, &code, &delivered); e != nil || state != "exhausted" || code != "lease-expired" || delivered {
		t.Fatalf("state=%s code=%s delivered=%v error=%v", state, code, delivered, e)
	}
}

func TestNotificationsCurrentBackfillIsBoundedAndEventSnapshotImmutable(t *testing.T) {
	s := notificationStore(t)
	ctx := context.Background()
	id := notificationIncident(t, s)
	if _, e := s.pool.Exec(ctx, "INSERT INTO operations_incidents(id,node_id,kind,state) VALUES($1,'hk-01','config-failed','open')", newID()); e != nil {
		t.Fatal(e)
	}
	// This test schema belongs solely to this test. Simulate old current rows
	// without captured outbox entries; backfill must not invent past transitions.
	if _, e := s.pool.Exec(ctx, "DELETE FROM notification_outbox"); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 2; i++ {
		if n, e := s.EnqueueIncidentNotifications(ctx, 1); e != nil || n != 1 {
			t.Fatalf("backfill %d %v", n, e)
		}
	}
	if n, e := s.EnqueueIncidentNotifications(ctx, 1); e != nil || n != 0 {
		t.Fatalf("duplicate backfill %d %v", n, e)
	}
	if _, e := s.pool.Exec(ctx, "UPDATE notification_outbox SET payload=payload||'{\"body\":\"customer-secret\"}'::jsonb WHERE incident_id=$1", id); e == nil {
		t.Fatal("event snapshot mutable")
	}
	if _, e := s.pool.Exec(ctx, "UPDATE operations_incidents SET observed_at=clock_timestamp() WHERE id=$1", id); e != nil {
		t.Fatal(e)
	}
	notificationCount(t, s, 2)
}

func TestNotificationsBothTransitionsInOneTransactionRemainAfterCommit(t *testing.T) {
	s := notificationStore(t)
	ctx := context.Background()
	tx, e := s.pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	id := newID()
	if _, e = tx.Exec(ctx, "INSERT INTO operations_incidents(id,node_id,kind,state) VALUES($1,'hk-01','contact-stale','open')", id); e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(ctx, "UPDATE operations_incidents SET state='resolved',resolved_at=clock_timestamp() WHERE id=$1", id); e != nil {
		t.Fatal(e)
	}
	notificationCount(t, s, 0) // Another connection cannot see uncommitted events.
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	notificationCount(t, s, 2)
}

func TestNotificationsMigrationBackfillsOnlyExistingCurrentState(t *testing.T) {
	s := operationsStore(t)
	ctx := context.Background()
	// The test owns this random schema. Undo only v15 objects if the parent
	// already registered v15, to exercise upgrading real pre-notification rows.
	var schema string
	if e := s.pool.QueryRow(ctx, "SELECT current_schema()").Scan(&schema); e != nil || !strings.HasPrefix(schema, "test_") {
		t.Fatalf("requires isolated test schema: %s %v", schema, e)
	}
	var exists bool
	if e := s.pool.QueryRow(ctx, "SELECT to_regclass('notification_outbox') IS NOT NULL").Scan(&exists); e != nil {
		t.Fatal(e)
	}
	if exists {
		if _, e := s.pool.Exec(ctx, `DROP TRIGGER notification_incident_transition ON operations_incidents;
 DROP TABLE notification_outbox;
 DROP FUNCTION notification_capture_incident();
 DROP FUNCTION notification_preserve_event();
 DROP FUNCTION notification_incident_payload(uuid,text,text,text,bigint,timestamptz);`); e != nil {
			t.Fatal(e)
		}
	}
	id := newID()
	if _, e := s.pool.Exec(ctx, "INSERT INTO operations_incidents(id,node_id,kind,state,occurrences,resolved_at) VALUES($1,'hk-01','contact-stale','resolved',3,clock_timestamp())", id); e != nil {
		t.Fatal(e)
	}
	raw, e := os.ReadFile("migration15.sql")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.pool.Exec(ctx, string(raw)); e != nil {
		t.Fatal(e)
	}
	notificationCount(t, s, 1)
	n, e := s.ClaimNotification(ctx)
	if e != nil || n == nil || n.Event.State != "resolved" || n.Event.Occurrences != 3 {
		t.Fatalf("migration backfill %+v %v", n, e)
	}
}

func TestNotificationsAdminListRetryAuthorizationAndFreshLease(t *testing.T) {
	s := notificationStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	owner, _ := register(t, s, "notification-owner@example.com")
	notificationIncident(t, s)
	job, e := s.ClaimNotification(ctx)
	if e != nil || job == nil {
		t.Fatalf("claim %+v %v", job, e)
	}
	if _, e = s.pool.Exec(ctx, "UPDATE notification_outbox SET state='exhausted',attempts=8,lease_until=NULL,last_error_code='delivery-failed' WHERE event_id=$1", job.Event.EventID); e != nil {
		t.Fatal(e)
	}
	for _, who := range []Identity{owner, {MemberID: owner.MemberID, TenantID: owner.TenantID, PlatformAdmin: true}} {
		if _, e = s.AdminNotificationDeliveries(ctx, who, 1); !errors.Is(e, ErrForbidden) {
			t.Fatalf("unauthorized list %v", e)
		}
		if e = s.AdminRetryNotification(ctx, who, job.Event.EventID); !errors.Is(e, ErrForbidden) {
			t.Fatalf("unauthorized retry %v", e)
		}
	}
	if _, e = s.AdminNotificationDeliveries(WithAPIKey(ctx, "fake"), admin, 1); !errors.Is(e, ErrForbidden) {
		t.Fatalf("bearer list %v", e)
	}
	if e = s.AdminRetryNotification(WithAPIKey(ctx, "fake"), admin, job.Event.EventID); !errors.Is(e, ErrForbidden) {
		t.Fatalf("bearer retry %v", e)
	}
	page, e := s.AdminNotificationDeliveries(ctx, admin, 1)
	if e != nil || page.Total != 1 || len(page.Items) != 1 || page.Items[0].State != "exhausted" || page.Items[0].Attempts != 8 {
		t.Fatalf("list %+v %v", page, e)
	}
	raw, e := json.Marshal(page)
	if e != nil {
		t.Fatal(e)
	}
	for _, bad := range []string{job.LeaseToken, "leaseToken", "lease_until", "payload", "secret", "webhook"} {
		if strings.Contains(string(raw), bad) {
			t.Fatalf("metadata leak %s", raw)
		}
	}
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- s.AdminRetryNotification(ctx, admin, job.Event.EventID) }()
	}
	wg.Wait()
	close(results)
	for e := range results {
		if e != nil {
			t.Fatal(e)
		}
	}
	var audits int
	if e = s.pool.QueryRow(ctx, "SELECT count(*) FROM platform_audit_logs WHERE action='notification.retry'").Scan(&audits); e != nil || audits != 1 {
		t.Fatalf("retry audit %d %v", audits, e)
	}
	next, e := s.ClaimNotification(ctx)
	if e != nil || next == nil || next.Attempts != 1 || next.LeaseToken == job.LeaseToken || next.Event.EventID != job.Event.EventID {
		t.Fatalf("fresh retry %+v %v", next, e)
	}
	if e = s.ReportNotification(ctx, job.Event.EventID, job.LeaseToken, true, ""); !errors.Is(e, ErrConflict) {
		t.Fatalf("old token survived retry %v", e)
	}
	if e = s.AdminRetryNotification(ctx, admin, next.Event.EventID); !errors.Is(e, ErrConflict) {
		t.Fatalf("claimed retry %v", e)
	}
	if e = s.ReportNotification(ctx, next.Event.EventID, next.LeaseToken, true, ""); e != nil {
		t.Fatal(e)
	}
	if e = s.AdminRetryNotification(ctx, admin, next.Event.EventID); !errors.Is(e, ErrConflict) {
		t.Fatalf("delivered retry %v", e)
	}
	page, e = s.AdminNotificationDeliveries(ctx, admin, 1)
	if e != nil || page.Items[0].DeliveredAt == nil || page.Items[0].State != "delivered" {
		t.Fatalf("delivery metadata %+v %v", page, e)
	}
	if _, e = s.AdminNotificationDeliveries(ctx, admin, 0); !errors.Is(e, ErrInvalid) {
		t.Fatal("invalid page accepted")
	}
	if e = s.SetPlatformAdmin(ctx, admin.Email, false); e != nil {
		t.Fatal(e)
	}
	if _, e = s.AdminNotificationDeliveries(ctx, admin, 1); !errors.Is(e, ErrForbidden) {
		t.Fatalf("revoked list %v", e)
	}
}

func TestNotificationsAdminRetryAuditFailureRollsBack(t *testing.T) {
	s := notificationStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	notificationIncident(t, s)
	job, e := s.ClaimNotification(ctx)
	if e != nil || job == nil {
		t.Fatalf("claim %+v %v", job, e)
	}
	if _, e = s.pool.Exec(ctx, "UPDATE notification_outbox SET state='exhausted',attempts=8,lease_until=NULL,last_error_code='delivery-failed' WHERE event_id=$1", job.Event.EventID); e != nil {
		t.Fatal(e)
	}
	if _, e = s.pool.Exec(ctx, "ALTER TABLE platform_audit_logs ADD CONSTRAINT notification_retry_audit_reject CHECK(action<>'notification.retry') NOT VALID"); e != nil {
		t.Fatal(e)
	}
	if e = s.AdminRetryNotification(ctx, admin, job.Event.EventID); e == nil {
		t.Fatal("retry ignored audit failure")
	}
	var state, token string
	var attempts int
	if e = s.pool.QueryRow(ctx, "SELECT state,attempts,lease_token FROM notification_outbox WHERE event_id=$1", job.Event.EventID).Scan(&state, &attempts, &token); e != nil || state != "exhausted" || attempts != 8 || token != job.LeaseToken {
		t.Fatalf("retry rollback state=%s attempts=%d tokenChanged=%v error=%v", state, attempts, token != job.LeaseToken, e)
	}
}
