package platform

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"reladock/internal/sourceflow"
)

func sourceFlowManifestForConfig(t *testing.T, cfg NodeConfig) (sourceflow.Manifest, string) {
	t.Helper()
	rules := map[string]ConfigRule{}
	for _, rule := range cfg.Rules {
		rules[rule.ID] = rule
	}
	applied := make([]sourceflow.AppliedRule, 0)
	for _, allocation := range cfg.Allocations {
		rule := rules[allocation.RuleID]
		if !rule.SourceLoggingEnabled {
			continue
		}
		applied = append(applied, sourceflow.AppliedRule{
			AllocationID: allocation.ID, TenantID: rule.TenantID, RuleID: rule.ID,
			RuleVersion: rule.Version, SourceLoggingEnabled: true, Protocol: allocation.Protocol,
			RelayIP: allocation.Address, RelayPort: allocation.Port,
			TargetIP: rule.TargetHost, TargetPort: rule.TargetPort,
		})
	}
	manifest, digest, err := sourceflow.NewManifest(cfg.NodeID, cfg.Generation, applied)
	if err != nil {
		t.Fatal(err)
	}
	return manifest, digest
}

func beginSourceFlowApplication(t *testing.T, s *Store, job *ConfigJob) NodeConfig {
	t.Helper()
	ctx := context.Background()
	if err := s.BeginApplication(ctx, job.NodeID, job.ID, ConfigReport{LeaseToken: job.LeaseToken, Checksum: job.Checksum, Outcome: "applying"}); err != nil {
		t.Fatal(err)
	}
	var cfg NodeConfig
	if err := json.Unmarshal(job.Payload, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func applySourceFlowJob(t *testing.T, s *Store, job *ConfigJob) NodeConfig {
	t.Helper()
	cfg := beginSourceFlowApplication(t, s, job)
	report := ConfigReport{LeaseToken: job.LeaseToken, Checksum: job.Checksum, Outcome: "applied"}
	if cfg.SchemaVersion == 3 {
		manifest, digest := sourceFlowManifestForConfig(t, cfg)
		report.SourceFlowManifest = &manifest
		report.SourceFlowManifestDigest = digest
	}
	if err := s.ReportConfig(context.Background(), job.NodeID, job.ID, report); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestSourceFlowManifestApplicationRejectsMismatchAndPersistsExactHistory(t *testing.T) {
	s, admin, customer, ruleID := sourceLoggingReadyFixture(t)
	ctx := context.Background()
	if _, err := s.AdminSetRuleSourceLogging(ctx, admin, customer.TenantID, ruleID, 1, true); err != nil {
		t.Fatal(err)
	}
	first, err := s.ClaimConfig(ctx, "hk-01", "source-flow-agent")
	if err != nil || first == nil {
		t.Fatalf("claim: %v", err)
	}
	firstConfig := beginSourceFlowApplication(t, s, first)
	manifest, digest := sourceFlowManifestForConfig(t, firstConfig)
	bad := manifest
	bad.Rules = append([]sourceflow.AppliedRule(nil), manifest.Rules...)
	bad.Rules[0].TargetPort++
	if err = s.ReportConfig(ctx, "hk-01", first.ID, ConfigReport{LeaseToken: first.LeaseToken, Checksum: first.Checksum, Outcome: "applied", SourceFlowManifest: &bad, SourceFlowManifestDigest: digest}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("mismatched manifest: %v", err)
	}
	var state string
	if err = s.pool.QueryRow(ctx, `SELECT state FROM config_outbox WHERE id=$1`, first.ID).Scan(&state); err != nil || state != "applying" {
		t.Fatalf("bad manifest advanced state=%q err=%v", state, err)
	}
	if err = s.ReportConfig(ctx, "hk-01", first.ID, ConfigReport{LeaseToken: first.LeaseToken, Checksum: first.Checksum, Outcome: "applied", SourceFlowManifest: &manifest, SourceFlowManifestDigest: digest}); err != nil {
		t.Fatal(err)
	}
	if err = s.ReportConfig(ctx, "hk-01", first.ID, ConfigReport{LeaseToken: first.LeaseToken, Checksum: first.Checksum, Outcome: "applied", SourceFlowManifest: &bad, SourceFlowManifestDigest: digest}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("changed applied replay accepted: %v", err)
	}
	var appliedEnabled bool
	var appliedVersion, appliedGeneration int64
	if err = s.pool.QueryRow(ctx, `SELECT source_logging_applied_enabled,source_logging_applied_version,source_logging_applied_generation FROM relay_rules WHERE id=$1`, ruleID).Scan(&appliedEnabled, &appliedVersion, &appliedGeneration); err != nil || !appliedEnabled || appliedVersion != 2 || appliedGeneration != first.Generation {
		t.Fatalf("first applied state enabled=%v version=%d generation=%d err=%v", appliedEnabled, appliedVersion, appliedGeneration, err)
	}
	var firstRows int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM applied_source_flow_rules WHERE node_id='hk-01' AND config_generation=$1 AND manifest_digest=$2`, first.Generation, digest).Scan(&firstRows); err != nil || firstRows != 1 {
		t.Fatalf("first manifest rows=%d err=%v", firstRows, err)
	}

	if _, err = s.AdminSetRuleSourceLogging(ctx, admin, customer.TenantID, ruleID, 2, false); err != nil {
		t.Fatal(err)
	}
	disabled, _ := s.ClaimConfig(ctx, "hk-01", "source-flow-agent")
	disabledConfig := applySourceFlowJob(t, s, disabled)
	if disabledConfig.SchemaVersion != 2 {
		t.Fatalf("disable schema=%d", disabledConfig.SchemaVersion)
	}
	if err = s.pool.QueryRow(ctx, `SELECT source_logging_applied_enabled,source_logging_applied_version,source_logging_applied_generation FROM relay_rules WHERE id=$1`, ruleID).Scan(&appliedEnabled, &appliedVersion, &appliedGeneration); err != nil || appliedEnabled || appliedVersion != 3 || appliedGeneration != disabled.Generation {
		t.Fatalf("disabled applied state enabled=%v version=%d generation=%d err=%v", appliedEnabled, appliedVersion, appliedGeneration, err)
	}

	if _, err = s.AdminSetRuleSourceLogging(ctx, admin, customer.TenantID, ruleID, 3, true); err != nil {
		t.Fatal(err)
	}
	reenabled, _ := s.ClaimConfig(ctx, "hk-01", "source-flow-agent")
	applySourceFlowJob(t, s, reenabled)
	if err = s.pool.QueryRow(ctx, `SELECT source_logging_applied_enabled,source_logging_applied_version,source_logging_applied_generation FROM relay_rules WHERE id=$1`, ruleID).Scan(&appliedEnabled, &appliedVersion, &appliedGeneration); err != nil || !appliedEnabled || appliedVersion != 4 || appliedGeneration != reenabled.Generation {
		t.Fatalf("reenabled state enabled=%v version=%d generation=%d err=%v", appliedEnabled, appliedVersion, appliedGeneration, err)
	}

	// A later unacknowledged edit and retirement cannot rewrite the immutable
	// manifest used to validate delayed WAL events from the first generation.
	if _, err = s.UpdateRule(ctx, customer, ruleID, RuleUpdateInput{Name: "later edit", TargetHost: "198.51.100.99", TargetPort: 9443, BandwidthMbps: 100, DesiredVersion: 4}); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteRule(ctx, customer, ruleID); err != nil {
		t.Fatal(err)
	}
	var retainedTarget string
	if err = s.pool.QueryRow(ctx, `SELECT host(target_ip) FROM applied_source_flow_rules WHERE node_id='hk-01' AND config_generation=$1`, first.Generation).Scan(&retainedTarget); err != nil || retainedTarget != "198.51.100.20" {
		t.Fatalf("historical target=%q err=%v", retainedTarget, err)
	}
}

func TestSourceLoggingAppliedRequiresManifestOnlyForSchema3AppliedACK(t *testing.T) {
	s, admin, customer, ruleID := sourceLoggingReadyFixture(t)
	ctx := context.Background()
	if _, err := s.AdminSetRuleSourceLogging(ctx, admin, customer.TenantID, ruleID, 1, true); err != nil {
		t.Fatal(err)
	}
	job, _ := s.ClaimConfig(ctx, "hk-01", "source-flow-agent")
	beginSourceFlowApplication(t, s, job)
	if err := s.ReportConfig(ctx, "hk-01", job.ID, ConfigReport{LeaseToken: job.LeaseToken, Checksum: job.Checksum, Outcome: "applied"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("schema 3 applied without manifest: %v", err)
	}
}

func TestSourceFlowManifestRejectedOutsideAppliedReport(t *testing.T) {
	s, admin, customer, ruleID := sourceLoggingReadyFixture(t)
	ctx := context.Background()
	if _, err := s.AdminSetRuleSourceLogging(ctx, admin, customer.TenantID, ruleID, 1, true); err != nil {
		t.Fatal(err)
	}
	job, _ := s.ClaimConfig(ctx, "hk-01", "source-flow-agent")
	var cfg NodeConfig
	if err := json.Unmarshal(job.Payload, &cfg); err != nil {
		t.Fatal(err)
	}
	manifest, digest := sourceFlowManifestForConfig(t, cfg)
	report := ConfigReport{LeaseToken: job.LeaseToken, Checksum: job.Checksum, Outcome: "applying", SourceFlowManifest: &manifest, SourceFlowManifestDigest: digest}
	if err := s.BeginApplication(ctx, "hk-01", job.ID, report); !errors.Is(err, ErrInvalid) {
		t.Fatalf("begin accepted source-flow manifest: %v", err)
	}
}
