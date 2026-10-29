package platform

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

func ruleInput(protocol string) RuleInput {
	return RuleInput{Name: "客户落地", Protocol: protocol, TargetHost: "192.0.2.10", TargetPort: 443, BandwidthMbps: 1000, NodeID: "hk-01"}
}

func TestPortsAreAtomicAndSeparateByTransport(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	who, _ := register(t, s, "ports@example.com")
	tcp, err := s.CreateRule(ctx, who, "tcp-first", ruleInput("tcp"))
	if err != nil {
		t.Fatal(err)
	}
	udp, err := s.CreateRule(ctx, who, "udp-first", ruleInput("udp"))
	if err != nil {
		t.Fatal(err)
	}
	if tcp.ListenPort != 20000 || udp.ListenPort != 20000 {
		t.Fatalf("separate transports: %d %d", tcp.ListenPort, udp.ListenPort)
	}
	both, err := s.CreateRule(ctx, who, "both-first", ruleInput("both"))
	if err != nil {
		t.Fatal(err)
	}
	if both.ListenPort != 20001 || both.Status != "pending-agent" || both.AppliedVersion != 0 {
		t.Fatalf("both: %+v", both)
	}
	var occupied int
	s.pool.QueryRow(ctx, "SELECT count(*) FROM endpoint_allocations WHERE rule_id=$1", both.ID).Scan(&occupied)
	if occupied != 2 {
		t.Fatalf("both allocated %d endpoints", occupied)
	}
	var count int
	if err = s.pool.QueryRow(ctx, `WITH invalid AS (INSERT INTO endpoint_allocations SELECT * FROM endpoint_allocations WHERE rule_id=$1 RETURNING 1) SELECT count(*) FROM invalid`, both.ID).Scan(&count); err == nil {
		t.Fatal("database allowed duplicate endpoint")
	}
}

func TestConcurrentPortAllocationAndIdempotency(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	who, _ := register(t, s, "concurrent@example.com")
	var wg sync.WaitGroup
	results := make(chan Rule, 16)
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, e := s.CreateRule(ctx, who, fmt.Sprintf("request-%d", i), ruleInput("tcp"))
			results <- r
			errs <- e
		}(i)
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	seen := map[int]bool{}
	for r := range results {
		if seen[r.ListenPort] {
			t.Fatal("duplicate port")
		}
		seen[r.ListenPort] = true
	}
	replay := make(chan Rule, 8)
	failures := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := s.CreateRule(ctx, who, "same-request", ruleInput("both"))
			replay <- r
			failures <- e
		}()
	}
	wg.Wait()
	close(replay)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	var id string
	for r := range replay {
		if id != "" && r.ID != id {
			t.Fatal("idempotency created extra rules")
		}
		id = r.ID
	}
	changed := ruleInput("both")
	changed.TargetPort = 8443
	if _, err := s.CreateRule(ctx, who, "same-request", changed); err != ErrConflict {
		t.Fatalf("payload mismatch: %v", err)
	}
	rules, err := s.ListRules(ctx, who)
	if err != nil || len(rules) != 17 {
		t.Fatalf("rules %d: %v", len(rules), err)
	}
}

func TestTenantAuthorizationPauseDeleteAndAudit(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a, _ := register(t, s, "a@example.com")
	b, _ := register(t, s, "b@example.com")
	rule, err := s.CreateRule(ctx, a, "tenant-a-rule", ruleInput("both"))
	if err != nil {
		t.Fatal(err)
	}
	rules, err := s.ListRules(ctx, b)
	if err != nil || len(rules) != 0 {
		t.Fatal("tenant data leaked")
	}
	if _, err = s.PauseRule(ctx, b, rule.ID); err != ErrNotFound {
		t.Fatalf("cross-tenant pause: %v", err)
	}
	if err = s.DeleteRule(ctx, b, rule.ID); err != ErrNotFound {
		t.Fatalf("cross-tenant delete: %v", err)
	}
	viewer := a
	viewer.Role = "viewer"
	if _, err = s.CreateRule(ctx, viewer, "viewer-rule", ruleInput("tcp")); err != ErrForbidden {
		t.Fatalf("viewer write: %v", err)
	}
	paused, err := s.PauseRule(ctx, a, rule.ID)
	if err != nil || paused.Status != "paused" || paused.DesiredVersion != 2 {
		t.Fatalf("pause: %+v %v", paused, err)
	}
	again, err := s.PauseRule(ctx, a, rule.ID)
	if err != nil || again.DesiredVersion != 2 {
		t.Fatal("pause is not idempotent")
	}
	if err = s.DeleteRule(ctx, a, rule.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteRule(ctx, a, rule.ID); err != nil {
		t.Fatal("delete is not idempotent")
	}
	replacement, err := s.CreateRule(ctx, a, "replacement", ruleInput("both"))
	if err != nil || replacement.ListenPort != 20000 {
		t.Fatalf("port reclaim: %+v %v", replacement, err)
	}
	old, err := s.CreateRule(ctx, a, "tenant-a-rule", ruleInput("both"))
	if err != nil || old.ID != rule.ID || old.Status != "deleted" {
		t.Fatal("replay resurrected deleted rule")
	}
	logs, err := s.AuditLogs(ctx, a)
	if err != nil || len(logs) != 5 {
		t.Fatalf("audit %d: %v", len(logs), err)
	}
	for _, log := range logs {
		if log.ActorID != a.MemberID {
			t.Fatal("foreign audit record")
		}
	}
}

func TestRuleTransactionRollsBackOnAuditFailure(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	who, _ := register(t, s, "rollback@example.com")
	_, err := s.pool.Exec(ctx, `CREATE FUNCTION reject_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'audit unavailable'; END; $$; CREATE TRIGGER reject_audit BEFORE INSERT ON audit_logs FOR EACH ROW EXECUTE FUNCTION reject_audit()`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateRule(ctx, who, "rollback-request", ruleInput("both")); err == nil {
		t.Fatal("failed audit reported success")
	}
	for _, table := range []string{"relay_rules", "endpoint_allocations", "idempotency_requests"} {
		var count int
		if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s leaked %d rows: %v", table, count, err)
		}
	}
}

func TestRejectInvalidRulesBeforeAllocation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	who, _ := register(t, s, "invalid@example.com")
	for _, patch := range []func(*RuleInput){
		func(r *RuleInput) { r.TargetHost = "https://example.com/a" }, func(r *RuleInput) { r.TargetHost = "a..example.com" },
		func(r *RuleInput) { r.TargetPort = 0 }, func(r *RuleInput) { r.BandwidthMbps = 1001 },
		func(r *RuleInput) { r.Protocol = "shell" }, func(r *RuleInput) { r.Name = " " },
	} {
		input := ruleInput("udp")
		patch(&input)
		if _, err := s.CreateRule(ctx, who, "invalid-request", input); err != ErrInvalid {
			t.Fatalf("validation: %v", err)
		}
	}
	if _, err := s.CreateRule(ctx, who, "short", ruleInput("udp")); err != ErrInvalid {
		t.Fatalf("invalid key: %v", err)
	}
	input := ruleInput("udp")
	input.NodeID = "absent"
	if _, err := s.CreateRule(ctx, who, "missing-node", input); err != ErrNotFound {
		t.Fatalf("node: %v", err)
	}
}
