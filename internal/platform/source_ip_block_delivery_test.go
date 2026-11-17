package platform

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSourceIPBlockConfigUsesSchemaFiveAndWaitsForCapability(t *testing.T) {
	s, admin, customer, ruleID := sourceIPBlockFixture(t)
	ctx := context.Background()
	// A low port independently requires schema 4. Adding a source block must
	// retain the higher cumulative schema 5 after allocations are serialized.
	if _, err := s.pool.Exec(ctx, `UPDATE relay_rules SET listen_port=443,listen_port_end=443 WHERE id=$1`, ruleID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE endpoint_allocations SET listen_port=443 WHERE rule_id=$1`, ruleID); err != nil {
		t.Fatal(err)
	}
	first, err := s.AdminAddSourceIPBlock(ctx, admin, customer.TenantID, ruleID, SourceIPBlockInput{CIDR: "203.0.113.9", Reason: "abuse", DesiredVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdminAddSourceIPBlock(ctx, admin, customer.TenantID, ruleID, SourceIPBlockInput{CIDR: "198.51.100.0/24", Reason: "abuse", DesiredVersion: first.RequestedVersion}); err != nil {
		t.Fatal(err)
	}

	cfg := latestConfig(t, s, "hk-01")
	want := []string{"198.51.100.0/24", "203.0.113.9/32"}
	if cfg.SchemaVersion != 5 || len(cfg.Rules) != 1 || !equalStrings(cfg.Rules[0].BlockedSourceCIDRs, want) {
		t.Fatalf("schema five snapshot: %+v", cfg)
	}

	fingerprint := strings.Repeat("a", 64)
	if err = s.EnrollNode(ctx, "hk-01", fingerprint); err != nil {
		t.Fatal(err)
	}
	heartbeat := validAgentHeartbeat(time.Now().UTC())
	heartbeat.PortRangeSupported = true
	heartbeat.SourceIPBlockSupported = false
	if err = s.ReportAgentHeartbeat(ctx, "hk-01", fingerprint, heartbeat); err != nil {
		t.Fatal(err)
	}
	if job, claimErr := s.ClaimConfig(ctx, "hk-01", "legacy-agent", fingerprint); claimErr != nil || job != nil {
		t.Fatalf("legacy Agent leased schema five: job=%+v err=%v", job, claimErr)
	}
	heartbeat.SourceIPBlockSupported = true
	if err = s.ReportAgentHeartbeat(ctx, "hk-01", fingerprint, heartbeat); err != nil {
		t.Fatal(err)
	}
	job, err := s.ClaimConfig(ctx, "hk-01", "block-agent", fingerprint)
	if err != nil || job == nil {
		t.Fatalf("capable Agent claim=%+v err=%v", job, err)
	}
	var leased NodeConfig
	if err = json.Unmarshal(job.Payload, &leased); err != nil || leased.SchemaVersion != 5 {
		t.Fatalf("leased config=%+v err=%v", leased, err)
	}
}

func TestSourceIPBlockCapabilityIsBoundToCertificateIdentity(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	oldFingerprint := strings.Repeat("b", 64)
	newFingerprint := strings.Repeat("c", 64)
	if err := s.EnrollNode(ctx, "hk-01", oldFingerprint); err != nil {
		t.Fatal(err)
	}
	heartbeat := validAgentHeartbeat(time.Now().UTC())
	heartbeat.SourceIPBlockSupported = true
	if err := s.ReportAgentHeartbeat(ctx, "hk-01", oldFingerprint, heartbeat); err != nil {
		t.Fatal(err)
	}
	var supported bool
	if err := s.pool.QueryRow(ctx, `SELECT agent_source_ip_block_supported FROM nodes WHERE id='hk-01'`).Scan(&supported); err != nil || !supported {
		t.Fatalf("stored capability=%v err=%v", supported, err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE nodes SET certificate_fingerprint=$1 WHERE id='hk-01'`, newFingerprint); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT agent_source_ip_block_supported FROM nodes WHERE id='hk-01'`).Scan(&supported); err != nil || supported {
		t.Fatalf("certificate replacement retained capability=%v err=%v", supported, err)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
