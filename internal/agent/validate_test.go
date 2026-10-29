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
