package platform

import (
	"context"
	"testing"
)

// Removing begin fencing would allow a delayed old Agent to resurrect a
// deleted/reallocated tuple after the controller issued a newer generation.
func TestApplicationFenceAndDeletionReservation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	who, _ := register(t, s, "application@example.com")
	r, e := s.CreateRule(ctx, who, "application-one", ruleInput("tcp"))
	if e != nil {
		t.Fatal(e)
	}
	j, e := s.ClaimConfig(ctx, "hk-01", "apply-test")
	if e != nil || j == nil {
		t.Fatal(e)
	}
	report := ConfigReport{LeaseToken: j.LeaseToken, Checksum: j.Checksum, Outcome: "applied"}
	if e = s.ReportConfig(ctx, "hk-01", j.ID, report); e != ErrConflict {
		t.Fatalf("applied without begin: %v", e)
	}
	report.Outcome = "applying"
	if e = s.BeginApplication(ctx, "hk-01", j.ID, report); e != nil {
		t.Fatal(e)
	}
	if e = s.BeginApplication(ctx, "hk-01", j.ID, report); e != nil {
		t.Fatal(e)
	}
	if e = s.DeleteRule(ctx, who, r.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = s.pool.Exec(ctx, "UPDATE config_outbox SET lease_until=now()-interval '1 minute' WHERE id=$1", j.ID); e != nil {
		t.Fatal(e)
	}
	newer, e := s.ClaimConfig(ctx, "hk-01", "apply-test")
	if e != nil || newer != nil {
		t.Fatalf("overlapping apply: %+v %v", newer, e)
	}
	var count int
	s.pool.QueryRow(ctx, "SELECT count(*) FROM endpoint_allocations WHERE rule_id=$1", r.ID).Scan(&count)
	if count != 1 {
		t.Fatal("in-flight allocation prematurely released")
	}
	report.Outcome = "applied"
	if e = s.ReportConfig(ctx, "hk-01", j.ID, report); e != nil {
		t.Fatal(e)
	}
	var oldStatus string
	if e = s.pool.QueryRow(ctx, "SELECT applied_status FROM relay_rules WHERE id=$1", r.ID).Scan(&oldStatus); e != nil {
		t.Fatal(e)
	}
	if oldStatus == "deleted" {
		t.Fatal("old application acknowledged a newer deletion")
	}
	if e = s.ReportConfig(ctx, "hk-01", j.ID, report); e != nil {
		t.Fatal(e)
	}
	newer, e = s.ClaimConfig(ctx, "hk-01", "apply-test")
	if e != nil || newer == nil {
		t.Fatalf("removal not delivered: %v", e)
	}
	report = ConfigReport{LeaseToken: newer.LeaseToken, Checksum: newer.Checksum, Outcome: "applying"}
	if e = s.BeginApplication(ctx, "hk-01", newer.ID, report); e != nil {
		t.Fatal(e)
	}
	report.Outcome = "applied"
	if e = s.ReportConfig(ctx, "hk-01", newer.ID, report); e != nil {
		t.Fatal(e)
	}
	var applied, desired int64
	s.pool.QueryRow(ctx, "SELECT applied_version,desired_version FROM relay_rules WHERE id=$1", r.ID).Scan(&applied, &desired)
	if applied != desired {
		t.Fatal("removal not acknowledged")
	}
	s.pool.QueryRow(ctx, "SELECT count(*) FROM endpoint_allocations WHERE rule_id=$1", r.ID).Scan(&count)
	if count != 1 {
		t.Fatal("conntrack tombstone reused without reconciliation")
	}
}

func TestSupersededLeaseCannotBeginApplication(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	who, _ := register(t, s, "begin-stale@example.com")
	r, e := s.CreateRule(ctx, who, "begin-stale-one", ruleInput("udp"))
	if e != nil {
		t.Fatal(e)
	}
	j, e := s.ClaimConfig(ctx, "hk-01", "apply-test")
	if e != nil || j == nil {
		t.Fatal(e)
	}
	if _, e = s.PauseRule(ctx, who, r.ID); e != nil {
		t.Fatal(e)
	}
	if e = s.BeginApplication(ctx, "hk-01", j.ID, ConfigReport{LeaseToken: j.LeaseToken, Checksum: j.Checksum, Outcome: "applying"}); e != ErrConflict {
		t.Fatalf("stale begin: %v", e)
	}
}
