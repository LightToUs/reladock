package platform

import (
	"context"
	"fmt"
	"strings"
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

func TestUpdateRuleIsVersionedTenantScopedAndEnqueued(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner, _ := register(t, s, "update-owner@example.com")
	other, _ := register(t, s, "update-other@example.com")
	rule, err := s.CreateRule(ctx, owner, "update-rule", ruleInput("both"))
	if err != nil {
		t.Fatal(err)
	}
	var generationBefore int64
	if err = s.pool.QueryRow(ctx, "SELECT desired_generation FROM nodes WHERE id=$1", rule.NodeID).Scan(&generationBefore); err != nil {
		t.Fatal(err)
	}

	updated, err := s.UpdateRule(ctx, owner, rule.ID, RuleUpdateInput{
		Name: "  新加坡落地 B  ", TargetHost: "EDGE.EXAMPLE.COM", TargetPort: 8443,
		BandwidthMbps: 500, DesiredVersion: rule.DesiredVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != "新加坡落地 B" || updated.TargetHost != "edge.example.com" || updated.TargetPort != 8443 || updated.BandwidthMbps != 500 {
		t.Fatalf("update not normalized: %+v", updated)
	}
	if updated.Protocol != rule.Protocol || updated.NodeID != rule.NodeID || updated.ListenPort != rule.ListenPort || updated.DesiredVersion != rule.DesiredVersion+1 {
		t.Fatalf("allocation identity or version changed incorrectly: before=%+v after=%+v", rule, updated)
	}
	var generation int64
	var payload []byte
	if err = s.pool.QueryRow(ctx, `SELECT n.desired_generation,o.payload FROM nodes n JOIN config_outbox o ON o.node_id=n.id AND o.generation=n.desired_generation WHERE n.id=$1`, rule.NodeID).Scan(&generation, &payload); err != nil {
		t.Fatal(err)
	}
	if generation != generationBefore+1 || !strings.Contains(string(payload), `"targetPort":8443`) || !strings.Contains(string(payload), `"bandwidthMbps":500`) {
		t.Fatalf("latest config was not enqueued: generation=%d payload=%s", generation, payload)
	}
	logs, err := s.AuditLogs(ctx, owner)
	if err != nil || len(logs) < 1 || logs[0].Action != "rule.update" || logs[0].ResourceID != rule.ID {
		t.Fatalf("update audit missing: %+v %v", logs, err)
	}

	if _, err = s.UpdateRule(ctx, owner, rule.ID, RuleUpdateInput{Name: "stale", TargetHost: "192.0.2.2", TargetPort: 443, BandwidthMbps: 10, DesiredVersion: rule.DesiredVersion}); err != ErrConflict {
		t.Fatalf("stale update: %v", err)
	}
	if _, err = s.UpdateRule(ctx, other, rule.ID, RuleUpdateInput{Name: "foreign", TargetHost: "192.0.2.2", TargetPort: 443, BandwidthMbps: 10, DesiredVersion: updated.DesiredVersion}); err != ErrNotFound {
		t.Fatalf("cross-tenant update: %v", err)
	}
	viewer := owner
	viewer.Role = "viewer"
	if _, err = s.UpdateRule(ctx, viewer, rule.ID, RuleUpdateInput{Name: "viewer", TargetHost: "192.0.2.2", TargetPort: 443, BandwidthMbps: 10, DesiredVersion: updated.DesiredVersion}); err != ErrForbidden {
		t.Fatalf("viewer update: %v", err)
	}
	if _, err = s.UpdateRule(ctx, owner, rule.ID, RuleUpdateInput{Name: "invalid", TargetHost: "https://bad.example/a", TargetPort: 443, BandwidthMbps: 10, DesiredVersion: updated.DesiredVersion}); err != ErrInvalid {
		t.Fatalf("invalid update: %v", err)
	}
	if _, err = s.pool.Exec(ctx, "UPDATE nodes SET capacity_mbps=100 WHERE id=$1", rule.NodeID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.UpdateRule(ctx, owner, rule.ID, RuleUpdateInput{Name: "too large", TargetHost: "192.0.2.2", TargetPort: 443, BandwidthMbps: 101, DesiredVersion: updated.DesiredVersion}); err != ErrInvalid {
		t.Fatalf("over-capacity update: %v", err)
	}
}

func TestUpdateRuleRollsBackWhenAuditFails(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner, _ := register(t, s, "update-rollback@example.com")
	rule, err := s.CreateRule(ctx, owner, "update-rollback-rule", ruleInput("tcp"))
	if err != nil {
		t.Fatal(err)
	}
	var generationBefore int64
	if err = s.pool.QueryRow(ctx, "SELECT desired_generation FROM nodes WHERE id=$1", rule.NodeID).Scan(&generationBefore); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `CREATE FUNCTION reject_rule_update_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='rule.update' THEN RAISE EXCEPTION 'audit unavailable'; END IF; RETURN NEW; END; $$; CREATE TRIGGER reject_rule_update_audit BEFORE INSERT ON audit_logs FOR EACH ROW EXECUTE FUNCTION reject_rule_update_audit()`); err != nil {
		t.Fatal(err)
	}
	_, err = s.UpdateRule(ctx, owner, rule.ID, RuleUpdateInput{Name: "must roll back", TargetHost: "192.0.2.55", TargetPort: 9443, BandwidthMbps: 250, DesiredVersion: rule.DesiredVersion})
	if err == nil {
		t.Fatal("failed audit reported update success")
	}
	current, err := s.ListRules(ctx, owner)
	if err != nil || len(current) != 1 {
		t.Fatalf("read rule after rollback: %+v %v", current, err)
	}
	if current[0].Name != rule.Name || current[0].TargetHost != rule.TargetHost || current[0].DesiredVersion != rule.DesiredVersion {
		t.Fatalf("failed audit leaked rule mutation: before=%+v after=%+v", rule, current[0])
	}
	var generation int64
	if err = s.pool.QueryRow(ctx, "SELECT desired_generation FROM nodes WHERE id=$1", rule.NodeID).Scan(&generation); err != nil || generation != generationBefore {
		t.Fatalf("failed audit leaked node generation: %d %v", generation, err)
	}
}

func TestUpdateRuleCannotExceedActivePlanBandwidth(t *testing.T) {
	s, _, owner, rule, plan := subscriptionFixture(t)
	ctx := context.Background()
	if _, err := s.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: owner.TenantID, Key: "update-plan-funds", AmountCents: 500, Reason: "test funding", Operator: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PurchasePlan(ctx, owner, "update-plan-purchase", PurchasePlanInput{RuleID: rule.ID, PriceVersionID: plan.ID}); err != nil {
		t.Fatal(err)
	}
	rules, err := s.ListRules(ctx, owner)
	if err != nil || len(rules) != 1 {
		t.Fatalf("current rule: %+v %v", rules, err)
	}
	current := rules[0]
	if _, err = s.UpdateRule(ctx, owner, current.ID, RuleUpdateInput{Name: current.Name, TargetHost: current.TargetHost, TargetPort: current.TargetPort, BandwidthMbps: plan.BandwidthMbps + 1, DesiredVersion: current.DesiredVersion}); err != ErrInvalid {
		t.Fatalf("edit exceeded active plan bandwidth: %v", err)
	}
	updated, err := s.UpdateRule(ctx, owner, current.ID, RuleUpdateInput{Name: current.Name, TargetHost: current.TargetHost, TargetPort: current.TargetPort, BandwidthMbps: plan.BandwidthMbps - 25, DesiredVersion: current.DesiredVersion})
	if err != nil || updated.BandwidthMbps != plan.BandwidthMbps-25 {
		t.Fatalf("lower per-rule bandwidth rejected: %+v %v", updated, err)
	}
}
