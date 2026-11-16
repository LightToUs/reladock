package dataplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"testing"

	"reladock/internal/platform"
	"reladock/internal/sourceflow"
)

func TestMaximumSourceFlowReportFitsBoundedHTTPAndJournalLimits(t *testing.T) {
	rules := make([]sourceflow.AppliedRule, 0, 10000)
	for i := 0; i < 10000; i++ {
		rules = append(rules, sourceflow.AppliedRule{
			AllocationID: fmt.Sprintf("00000000-0000-4000-8000-%012d", i),
			TenantID:     "00000000-0000-4000-8000-000000000001",
			RuleID:       fmt.Sprintf("00000000-0000-4000-8001-%012d", i), RuleVersion: 1,
			SourceLoggingEnabled: true, Protocol: []string{"tcp", "udp"}[i%2],
			RelayIP: "203.0.113.254", RelayPort: 1 + i%65535,
			TargetIP: "198.51.100.254", TargetPort: 65535,
		})
	}
	manifest, digest, err := sourceflow.NewManifest("maximum-endpoints", 1, rules)
	if err != nil {
		t.Fatal(err)
	}
	report, err := json.Marshal(platform.ConfigReport{LeaseToken: "token", Checksum: fmt.Sprintf("%064d", 0), Outcome: "applied", SourceFlowManifest: &manifest, SourceFlowManifestDigest: digest})
	if err != nil {
		t.Fatal(err)
	}
	if len(report) > platform.MaxConfigReportBytes {
		t.Fatalf("maximum source-flow report size %d exceeds %d-byte HTTP bound", len(report), platform.MaxConfigReportBytes)
	}
}

func TestCompileSourceFlowManifestUsesResolvedTargetAndAllocationIdentity(t *testing.T) {
	cfg := fixtureV2()
	cfg.SchemaVersion = 3
	cfg.Generation = 7
	cfg.Rules[0].ID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	cfg.Rules[0].TenantID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	cfg.Rules[0].TargetHost = "relay-target.example"
	cfg.Rules[0].SourceLoggingEnabled = true
	cfg.Allocations[0].RuleID = cfg.Rules[0].ID
	o := options()
	o.Resolver = resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("198.51.100.40"), netip.MustParseAddr("198.51.100.20")}, nil
	})
	plan, err := Compile(context.Background(), cfg, o)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SourceFlowManifest == nil || len(plan.SourceFlowManifest.Rules) != 1 {
		t.Fatalf("manifest missing: %+v", plan.SourceFlowManifest)
	}
	rule := plan.SourceFlowManifest.Rules[0]
	if rule.AllocationID != cfg.Allocations[0].ID || rule.RuleID != cfg.Rules[0].ID || rule.TargetIP != "198.51.100.20" || rule.RelayIP != cfg.ListenAddress || rule.Protocol != "tcp" || !rule.SourceLoggingEnabled {
		t.Fatalf("manifest rule: %+v", rule)
	}
	if err := sourceflow.ValidateManifest(*plan.SourceFlowManifest, plan.SourceFlowManifestDigest); err != nil {
		t.Fatal(err)
	}
}

func TestCompileV4SourceFlowManifestIncludesEveryRangeEndpoint(t *testing.T) {
	cfg := fixtureV4()
	cfg.Rules[0].ID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	cfg.Rules[0].TenantID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	cfg.Rules[0].SourceLoggingEnabled = true
	for i := range cfg.Allocations {
		cfg.Allocations[i].RuleID = cfg.Rules[0].ID
	}
	plan, err := Compile(context.Background(), cfg, options())
	if err != nil {
		t.Fatal(err)
	}
	if plan.SourceFlowManifest == nil || len(plan.SourceFlowManifest.Rules) != 3 {
		t.Fatalf("range manifest: %+v", plan.SourceFlowManifest)
	}
	ports := map[int]bool{}
	for _, rule := range plan.SourceFlowManifest.Rules {
		ports[rule.RelayPort] = true
	}
	for port := 40000; port <= 40002; port++ {
		if !ports[port] {
			t.Fatalf("range manifest missing port %d: %+v", port, plan.SourceFlowManifest.Rules)
		}
	}
}
