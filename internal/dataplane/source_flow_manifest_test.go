package dataplane

import (
	"context"
	"net/netip"
	"testing"

	"reladock/internal/sourceflow"
)

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
