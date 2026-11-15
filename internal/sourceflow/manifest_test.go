package sourceflow

import "testing"

func manifestRules() []AppliedRule {
	return []AppliedRule{
		{AllocationID: "22222222-2222-4222-8222-222222222222", TenantID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", RuleID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", RuleVersion: 4, SourceLoggingEnabled: true, Protocol: "udp", RelayIP: "203.0.113.10", RelayPort: 24444, TargetIP: "198.51.100.21", TargetPort: 8443},
		{AllocationID: "11111111-1111-4111-8111-111111111111", TenantID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", RuleID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", RuleVersion: 3, SourceLoggingEnabled: true, Protocol: "tcp", RelayIP: "203.0.113.10", RelayPort: 24443, TargetIP: "198.51.100.20", TargetPort: 443},
	}
}

func TestSourceFlowManifestCanonicalizesRulesAndDigest(t *testing.T) {
	manifest, digest, err := NewManifest("hk-01", 9, manifestRules())
	if err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != 1 || manifest.NodeID != "hk-01" || manifest.Generation != 9 || len(manifest.Rules) != 2 || manifest.Rules[0].AllocationID != "11111111-1111-4111-8111-111111111111" || len(digest) != 64 {
		t.Fatalf("manifest=%+v digest=%q", manifest, digest)
	}
	reversed := manifestRules()
	reversed[0], reversed[1] = reversed[1], reversed[0]
	other, otherDigest, err := NewManifest("hk-01", 9, reversed)
	if err != nil || otherDigest != digest || other.Rules[0].AllocationID != manifest.Rules[0].AllocationID {
		t.Fatalf("non-canonical digest: %+v %q %v", other, otherDigest, err)
	}
	if err := ValidateManifest(manifest, digest); err != nil {
		t.Fatal(err)
	}
}

func TestSourceFlowManifestRejectsMalformedOrTamperedMetadata(t *testing.T) {
	manifest, digest, err := NewManifest("hk-01", 9, manifestRules())
	if err != nil {
		t.Fatal(err)
	}
	manifest.Rules[0].TargetPort++
	if err := ValidateManifest(manifest, digest); err == nil {
		t.Fatal("tampered target accepted")
	}
	for name, mutate := range map[string]func(*AppliedRule){
		"payload protocol":     func(r *AppliedRule) { r.Protocol = "icmp" },
		"invalid source state": func(r *AppliedRule) { r.SourceLoggingEnabled = false },
		"invalid address":      func(r *AppliedRule) { r.TargetIP = "127.0.0.1" },
		"invalid port":         func(r *AppliedRule) { r.RelayPort = 0 },
		"invalid allocation":   func(r *AppliedRule) { r.AllocationID = "allocation" },
	} {
		t.Run(name, func(t *testing.T) {
			rules := manifestRules()
			mutate(&rules[0])
			if _, _, err := NewManifest("hk-01", 9, rules); err == nil {
				t.Fatal("invalid manifest accepted")
			}
		})
	}
}
