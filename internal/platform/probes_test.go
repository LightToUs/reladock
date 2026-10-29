package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"reladock/internal/probe"
)

func probeStore(t *testing.T) *Store {
	t.Helper()
	s := testStore(t)
	ctx := context.Background()
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT to_regclass('probe_latest') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		sql, err := os.ReadFile("migration14.sql")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.pool.Exec(ctx, string(sql)); err != nil {
			t.Fatal(err)
		}
	}
	return s
}
func probeFixture(t *testing.T) (*Store, Identity, Rule, probe.Result, string) {
	t.Helper()
	ctx := context.Background()
	s := probeStore(t)
	admin, _ := adminIdentity(t, s)
	who, _ := register(t, s, "probe-owner@example.com")
	rule, err := s.CreateRule(ctx, who, "probe-fixture-rule", ruleInput("both"))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := s.AdminCreatePlan(ctx, admin, configuredPlan())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdjustWalletFromCLI(ctx, WalletAdjustment{TenantID: who.TenantID, Key: "probe-fixture-funds", AmountCents: 500, Operator: "test", Reason: "isolated test fixture"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PurchasePlan(ctx, who, "probe-fixture-purchase", PurchasePlanInput{RuleID: rule.ID, PriceVersionID: plan.ID}); err != nil {
		t.Fatal(err)
	}
	credential := strings.Repeat("a", 64)
	if _, err = s.pool.Exec(ctx, `UPDATE nodes SET certificate_fingerprint=$1 WHERE id='hk-01'`, credential); err != nil {
		t.Fatal(err)
	}
	job, err := s.ClaimConfig(ctx, "hk-01", "probe-test", credential)
	if err != nil || job == nil {
		t.Fatalf("claim: %v %v", job, err)
	}
	report := ConfigReport{LeaseToken: job.LeaseToken, Checksum: job.Checksum, Outcome: "applying"}
	if err = s.BeginApplication(ctx, "hk-01", job.ID, report, credential); err != nil {
		t.Fatal(err)
	}
	report.Outcome = "applied"
	if err = s.ReportConfig(ctx, "hk-01", job.ID, report, credential); err != nil {
		t.Fatal(err)
	}
	latency := 1.25
	result := probe.Result{NodeID: "hk-01", RuleID: rule.ID, Generation: job.Generation, ObservedAt: time.Now().UTC(), Type: probe.TCP, Status: probe.TargetReachable, LatencyMillis: &latency}
	return s, who, rule, result, credential
}

func TestProbeReportExactReplayOrderingAndTenantRead(t *testing.T) {
	s, who, _, result, credential := probeFixture(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- s.ReportProbe(ctx, "hk-01", result, credential) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var count int
	var received time.Time
	if err := s.pool.QueryRow(ctx, `SELECT count(*),max(received_at) FROM probe_latest`).Scan(&count, &received); err != nil || count != 1 {
		t.Fatalf("exact replay duplicated: %d %v", count, err)
	}
	if err := s.ReportProbe(ctx, "hk-01", result, credential); err != nil {
		t.Fatal(err)
	}
	var after time.Time
	if err := s.pool.QueryRow(ctx, `SELECT received_at FROM probe_latest`).Scan(&after); err != nil || !after.Equal(received) {
		t.Fatalf("replay refreshed observation: %v", err)
	}
	tampered := result
	tampered.Status = probe.TargetUnreachable
	if err := s.ReportProbe(ctx, "hk-01", tampered, credential); err != ErrConflict {
		t.Fatalf("same instant tampering: %v", err)
	}
	newer := result
	newer.ObservedAt = time.Now().UTC()
	newer.Status = probe.TargetUnreachable
	if err := s.ReportProbe(ctx, "hk-01", newer, credential); err != nil {
		t.Fatal(err)
	}
	if err := s.ReportProbe(ctx, "hk-01", result, credential); err != ErrConflict {
		t.Fatalf("out-of-order report: %v", err)
	}
	got, err := s.ProbeResults(ctx, who, result.RuleID)
	if err != nil || len(got) != 1 || got[0].Status != probe.TargetUnreachable || !got[0].ObservedAt.Equal(newer.ObservedAt) {
		t.Fatalf("latest observation: %+v %v", got, err)
	}
	other, _ := register(t, s, "probe-other@example.com")
	if _, err = s.ProbeResults(ctx, other, result.RuleID); err != ErrNotFound {
		t.Fatalf("cross-tenant read: %v", err)
	}
	forged := other
	forged.TenantID = who.TenantID
	if _, err = s.ProbeResults(ctx, forged, result.RuleID); err != ErrUnauthorized {
		t.Fatalf("forged identity: %v", err)
	}
}

func TestProbeNanosecondOrderingDoesNotRoundIntoReplay(t *testing.T) {
	s, who, _, first, credential := probeFixture(t)
	ctx := context.Background()
	// Keep observations safely behind the DB clock but preserve a one-nanosecond
	// difference that a timestamptz-only latest table would lose.
	first.ObservedAt = first.ObservedAt.Add(-time.Second)
	if err := s.ReportProbe(ctx, "hk-01", first, credential); err != nil {
		t.Fatal(err)
	}
	second := first
	second.ObservedAt = first.ObservedAt.Add(time.Nanosecond)
	second.Status = probe.TargetUnreachable
	if err := s.ReportProbe(ctx, "hk-01", second, credential); err != nil {
		t.Fatalf("nanosecond newer result rejected: %v", err)
	}
	if err := s.ReportProbe(ctx, "hk-01", first, credential); err != ErrConflict {
		t.Fatalf("nanosecond reordering accepted: %v", err)
	}
	got, err := s.ProbeResults(ctx, who, first.RuleID)
	if err != nil || len(got) != 1 || !got[0].ObservedAt.Equal(second.ObservedAt) {
		t.Fatalf("nanosecond observation lost: %+v %v", got, err)
	}
}

func TestProbeReportRejectsIdentityTimeAndStatusTampering(t *testing.T) {
	s, _, _, original, credential := probeFixture(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		mutate func(*probe.Result)
	}{
		{"node", func(r *probe.Result) { r.NodeID = "sg-02" }},
		{"future-time", func(r *probe.Result) { r.ObservedAt = time.Now().Add(time.Hour) }},
		{"stale-time", func(r *probe.Result) { r.ObservedAt = time.Now().Add(-time.Hour) }},
		{"zero-time", func(r *probe.Result) { r.ObservedAt = time.Time{} }},
		{"future-generation", func(r *probe.Result) { r.Generation++ }},
		{"stale-generation", func(r *probe.Result) { r.Generation-- }},
		{"http-not-bound", func(r *probe.Result) { r.Type = probe.HTTP; r.Status = probe.EndpointResponsive }},
		{"udp-claimed-reachable", func(r *probe.Result) { r.Type = probe.UDP }},
		{"tcp-healthy", func(r *probe.Result) { r.Status = "healthy" }},
		{"tcp-unknown", func(r *probe.Result) { r.Status = probe.Unknown }},
		{"missing-latency", func(r *probe.Result) { r.LatencyMillis = nil }},
		{"negative-latency", func(r *probe.Result) { v := -1.0; r.LatencyMillis = &v }},
		{"nan-latency", func(r *probe.Result) { v := math.NaN(); r.LatencyMillis = &v }},
		{"infinite-latency", func(r *probe.Result) { v := math.Inf(1); r.LatencyMillis = &v }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := original
			tc.mutate(&r)
			if err := s.ReportProbe(ctx, "hk-01", r, credential); err == nil {
				t.Fatal("tampered observation accepted")
			}
		})
	}
	for _, creds := range [][]string{nil, {""}, {strings.Repeat("b", 64)}, {credential, credential}} {
		if err := s.ReportProbe(ctx, "hk-01", original, creds...); err != ErrUnauthorized {
			t.Fatalf("credential bypass: %v", err)
		}
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM probe_latest`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejected report persisted: %d %v", count, err)
	}
	udp := original
	udp.Type = probe.UDP
	udp.Status = probe.Unknown
	udp.LatencyMillis = nil
	if err := s.ReportProbe(ctx, "hk-01", udp, credential); err != nil {
		t.Fatalf("valid UDP unknown: %v", err)
	}
}

func TestProbeReportChecksSnapshotTenantAndTargetBinding(t *testing.T) {
	s, _, _, result, credential := probeFixture(t)
	ctx := context.Background()
	var payload []byte
	var checksum string
	if err := s.pool.QueryRow(ctx, `SELECT payload,checksum FROM config_outbox WHERE node_id=$1 AND generation=$2`, result.NodeID, result.Generation).Scan(&payload, &checksum); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*NodeConfig){func(c *NodeConfig) { c.Rules[0].TenantID = newID() }, func(c *NodeConfig) { c.Rules[0].TargetHost = "198.51.100.99" }, func(c *NodeConfig) { c.Rules[0].Status = "paused" }, func(c *NodeConfig) { c.Rules[0].Version++ }, func(c *NodeConfig) { c.Rules = nil }} {
		var cfg NodeConfig
		if err := json.Unmarshal(payload, &cfg); err != nil {
			t.Fatal(err)
		}
		mutate(&cfg)
		raw, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(raw)
		if _, err = s.pool.Exec(ctx, `UPDATE config_outbox SET payload=$3,checksum=$4 WHERE node_id=$1 AND generation=$2`, result.NodeID, result.Generation, raw, hex.EncodeToString(hash[:])); err != nil {
			t.Fatal(err)
		}
		if err = s.ReportProbe(ctx, "hk-01", result, credential); err != ErrConflict {
			t.Fatalf("snapshot binding bypass: %v", err)
		}
	}
	if _, err := s.pool.Exec(ctx, `UPDATE config_outbox SET payload=$3,checksum=$4 WHERE node_id=$1 AND generation=$2`, result.NodeID, result.Generation, payload, checksum); err != nil {
		t.Fatal(err)
	}
	if err := s.ReportProbe(ctx, "hk-01", result, credential); err != nil {
		t.Fatal(err)
	}
}

func TestProbeReportRejectsUnappliedOldAndDeletedRules(t *testing.T) {
	for _, mode := range []string{"unapplied", "old-generation", "paused", "deleted", "wrong-node", "old-rule-version"} {
		t.Run(mode, func(t *testing.T) {
			s, who, rule, result, credential := probeFixture(t)
			ctx := context.Background()
			switch mode {
			case "unapplied":
				if _, err := s.pool.Exec(ctx, `UPDATE config_outbox SET state='validated' WHERE node_id=$1 AND generation=$2`, result.NodeID, result.Generation); err != nil {
					t.Fatal(err)
				}
			case "old-generation":
				if _, err := s.CreateRule(ctx, who, "probe-new-generation", ruleInput("tcp")); err != nil {
					t.Fatal(err)
				}
			case "paused":
				if _, err := s.PauseRule(ctx, who, rule.ID); err != nil {
					t.Fatal(err)
				}
			case "deleted":
				if err := s.DeleteRule(ctx, who, rule.ID); err != nil {
					t.Fatal(err)
				}
			case "wrong-node":
				if _, err := s.pool.Exec(ctx, `INSERT INTO nodes(id,name,status,listen_address,certificate_fingerprint) VALUES('sg-02','Other','unprovisioned','0.0.0.0',$1)`, strings.Repeat("b", 64)); err != nil {
					t.Fatal(err)
				}
				result.NodeID = "sg-02"
				credential = strings.Repeat("b", 64)
			case "old-rule-version":
				if _, err := s.pool.Exec(ctx, `UPDATE relay_rules SET applied_version=0 WHERE id=$1`, rule.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.ReportProbe(ctx, result.NodeID, result, credential); err == nil {
				t.Fatal("inactive/unapplied report accepted")
			}
		})
	}
}

func TestProbeCredentialRecheckedAfterNodeLockAndReadHidesOldResults(t *testing.T) {
	s, who, rule, result, credential := probeFixture(t)
	ctx := context.Background()
	if err := s.ReportProbe(ctx, "hk-01", result, credential); err != nil {
		t.Fatal(err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err = lockNode(ctx, tx, "hk-01"); err != nil {
		t.Fatal(err)
	}
	rotated := strings.Repeat("b", 64)
	if _, err = tx.Exec(ctx, `UPDATE nodes SET certificate_fingerprint=$1 WHERE id='hk-01'`, rotated); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.ReportProbe(ctx, "hk-01", result, credential) }()
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != ErrUnauthorized {
		t.Fatalf("rotated certificate remained valid: %v", err)
	}
	if _, err = s.PauseRule(ctx, who, rule.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.ProbeResults(ctx, who, rule.ID)
	if err != nil || len(got) != 0 {
		t.Fatalf("old observation exposed as current: %+v %v", got, err)
	}
}
