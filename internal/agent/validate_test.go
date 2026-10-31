package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"reladock/internal/platform"
)

func validJob() platform.ConfigJob {
	cfg := platform.NodeConfig{SchemaVersion: 1, NodeID: "hk-01", Generation: 2, ListenAddress: "0.0.0.0", Rules: []platform.ConfigRule{{ID: "rule-a", TenantID: "tenant-a", RuleInput: platform.RuleInput{Name: "Relay", Protocol: "both", TargetHost: "192.0.2.10", TargetPort: 443, BandwidthMbps: 1000, NodeID: "hk-01"}, ListenPort: 20000, Version: 1, Status: "pending-agent"}}}
	payload, _ := json.Marshal(cfg)
	sum := sha256.Sum256(payload)
	return platform.ConfigJob{NodeID: "hk-01", Generation: 2, Payload: payload, Checksum: hex.EncodeToString(sum[:])}
}
func TestValidateConfigRejectsTamperingWrongNodeAndPortOverlap(t *testing.T) {
	job := validJob()
	if err := ValidateConfig("hk-01", job); err != nil {
		t.Fatal(err)
	}
	if err := ValidateConfig("other-node", job); err == nil {
		t.Fatal("accepted foreign node")
	}
	bad := job
	bad.Payload = append([]byte(" "), job.Payload...)
	if err := ValidateConfig("hk-01", bad); err == nil {
		t.Fatal("accepted checksum mismatch")
	}
	var cfg platform.NodeConfig
	json.Unmarshal(job.Payload, &cfg)
	cfg.Rules = append(cfg.Rules, cfg.Rules[0])
	cfg.Rules[1].ID = "rule-b"
	job.Payload, _ = json.Marshal(cfg)
	sum := sha256.Sum256(job.Payload)
	job.Checksum = hex.EncodeToString(sum[:])
	if err := ValidateConfig("hk-01", job); err == nil {
		t.Fatal("accepted duplicate protocol endpoint")
	}
}

func TestValidateConfigAcceptsCompleteSchemaV2AndRejectsIdentityGaps(t *testing.T) {
	cfg := platform.NodeConfig{
		SchemaVersion: 2,
		NodeID:        "hk-01",
		Generation:    3,
		ListenAddress: "192.0.2.1",
		Rules:         []platform.ConfigRule{{ID: "rule-a", TenantID: "tenant-a", RuleInput: platform.RuleInput{Name: "Relay", Protocol: "tcp", TargetHost: "198.51.100.2", TargetPort: 443, BandwidthMbps: 100, NodeID: "hk-01"}, ListenPort: 20000, Version: 1, Status: "pending-agent"}},
		Allocations:   []platform.ConfigAllocation{{ID: "11111111-1111-4111-8111-111111111111", RuleID: "rule-a", Protocol: "tcp", Address: "192.0.2.1", Port: 20000}},
		Retirements:   []platform.ConfigRetirement{{ID: "22222222-2222-4222-8222-222222222222", AllocationID: "33333333-3333-4333-8333-333333333333", RuleID: "44444444-4444-4444-8444-444444444444", Protocol: "udp", Address: "192.0.2.1", Port: 20001, QuarantineGeneration: 3, State: "queued"}},
	}
	jobFor := func(c platform.NodeConfig) platform.ConfigJob {
		payload, _ := json.Marshal(c)
		sum := sha256.Sum256(payload)
		return platform.ConfigJob{NodeID: c.NodeID, Generation: c.Generation, Payload: payload, Checksum: hex.EncodeToString(sum[:])}
	}
	if err := ValidateConfig("hk-01", jobFor(cfg)); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*platform.NodeConfig){
		"missing allocation": func(c *platform.NodeConfig) { c.Allocations = nil },
		"tuple overlap": func(c *platform.NodeConfig) {
			c.Retirements[0].Protocol = "tcp"
			c.Retirements[0].Port = 20000
		},
		"future retirement":  func(c *platform.NodeConfig) { c.Retirements[0].QuarantineGeneration++ },
		"allocation rule id": func(c *platform.NodeConfig) { c.Allocations[0].RuleID = "bad rule" },
		"duplicate predecessor": func(c *platform.NodeConfig) {
			const predecessor = "55555555-5555-4555-8555-555555555555"
			c.Allocations[0].PredecessorRetirementID = predecessor
			rule := c.Rules[0]
			rule.ID = "rule-b"
			rule.ListenPort = 20002
			c.Rules = append(c.Rules, rule)
			c.Allocations = append(c.Allocations, platform.ConfigAllocation{ID: "66666666-6666-4666-8666-666666666666", RuleID: rule.ID, Protocol: rule.Protocol, Address: c.ListenAddress, Port: rule.ListenPort, PredecessorRetirementID: predecessor})
		},
	} {
		t.Run(name, func(t *testing.T) {
			bad := cfg
			bad.Allocations = append([]platform.ConfigAllocation(nil), cfg.Allocations...)
			bad.Retirements = append([]platform.ConfigRetirement(nil), cfg.Retirements...)
			mutate(&bad)
			if err := ValidateConfig("hk-01", jobFor(bad)); err == nil {
				t.Fatal("accepted incomplete schema-v2 metadata")
			}
		})
	}
}
