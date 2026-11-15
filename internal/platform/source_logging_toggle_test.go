package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestDisabledSourceLoggingFieldIsOmittedForStrictLegacyAgent(t *testing.T) {
	cfg := NodeConfig{SchemaVersion: 2, NodeID: "hk-01", Generation: 1, ListenAddress: "203.0.113.10", Rules: []ConfigRule{{ID: newID(), TenantID: newID(), RuleInput: RuleInput{Name: "relay", Protocol: "tcp", TargetHost: "198.51.100.20", TargetPort: 443, BandwidthMbps: 100, NodeID: "hk-01"}, ListenPort: 24443, Version: 1, Status: "pending-agent"}}}
	payload, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(payload, []byte("sourceLoggingEnabled")) {
		t.Fatalf("disabled extension leaked into legacy snapshot: %s", payload)
	}
	type legacyRule struct {
		ID, TenantID, Name, Protocol, TargetHost, NodeID string
		TargetPort, BandwidthMbps, ListenPort            int
		Version                                          int64
		Status                                           string
	}
	type legacyConfig struct {
		SchemaVersion int
		NodeID        string
		Generation    int64
		ListenAddress string
		Rules         []legacyRule
		Allocations   []ConfigAllocation
		Retirements   []ConfigRetirement
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var decoded legacyConfig
	if err = decoder.Decode(&decoded); err != nil || decoder.Decode(new(any)) != io.EOF {
		t.Fatalf("strict legacy decode failed: %v payload=%s", err, payload)
	}
}

func sourceLoggingReadyFixture(t *testing.T) (*Store, Identity, Identity, string) {
	t.Helper()
	s, customer, ruleID, _ := sourceFlowFixture(t)
	s.allowUnentitledRules = true
	s.sourceFlowRetentionDays = 180
	admin, _ := adminIdentity(t, s)
	fingerprint := strings.Repeat("a", 64)
	if err := s.EnrollNode(context.Background(), "hk-01", fingerprint); err != nil {
		t.Fatal(err)
	}
	heartbeat := validAgentHeartbeat(time.Now().UTC())
	heartbeat.Mode = "apply"
	heartbeat.Resources.DataplaneState = "in-sync"
	heartbeat.SourceFlowSupported = true
	if err := s.ReportAgentHeartbeat(context.Background(), "hk-01", fingerprint, heartbeat); err != nil {
		t.Fatal(err)
	}
	return s, admin, customer, ruleID
}

func latestConfig(t *testing.T, s *Store, nodeID string) NodeConfig {
	t.Helper()
	var payload []byte
	if err := s.pool.QueryRow(context.Background(), `SELECT payload FROM config_outbox WHERE node_id=$1 ORDER BY generation DESC LIMIT 1`, nodeID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var cfg NodeConfig
	if err := json.Unmarshal(payload, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestAdminSetRuleSourceLoggingIsVersionedAuditedAndUsesSchema3(t *testing.T) {
	s, admin, customer, ruleID := sourceLoggingReadyFixture(t)
	ctx := context.Background()

	rule, err := s.AdminSetRuleSourceLogging(ctx, admin, customer.TenantID, ruleID, 1, true)
	if err != nil {
		t.Fatal(err)
	}
	if !rule.SourceLoggingEnabled || rule.DesiredVersion != 2 || rule.SourceLoggingAppliedEnabled || rule.SourceLoggingAppliedVersion != 0 {
		t.Fatalf("enabled rule: %+v", rule)
	}
	cfg := latestConfig(t, s, "hk-01")
	if cfg.SchemaVersion != 3 || len(cfg.Rules) != 1 || !cfg.Rules[0].SourceLoggingEnabled {
		t.Fatalf("enabled snapshot: %+v", cfg)
	}
	var action string
	if err := s.pool.QueryRow(ctx, `SELECT action FROM platform_audit_logs WHERE resource_id=$1 ORDER BY id DESC LIMIT 1`, ruleID).Scan(&action); err != nil || action != "rule.source-logging.enable" {
		t.Fatalf("audit action=%q err=%v", action, err)
	}

	// A repeat against the current version is a no-op and does not enqueue.
	var before int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM config_outbox WHERE node_id='hk-01'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	replayed, err := s.AdminSetRuleSourceLogging(ctx, admin, customer.TenantID, ruleID, 2, true)
	if err != nil || replayed.DesiredVersion != 2 {
		t.Fatalf("no-op replay: %+v %v", replayed, err)
	}
	var after int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM config_outbox WHERE node_id='hk-01'`).Scan(&after); err != nil || after != before {
		t.Fatalf("no-op enqueued before=%d after=%d err=%v", before, after, err)
	}
	if _, err = s.AdminSetRuleSourceLogging(ctx, admin, customer.TenantID, ruleID, 1, false); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale desired version: %v", err)
	}

	rule, err = s.AdminSetRuleSourceLogging(ctx, admin, customer.TenantID, ruleID, 2, false)
	if err != nil || rule.SourceLoggingEnabled || rule.DesiredVersion != 3 {
		t.Fatalf("disabled rule: %+v %v", rule, err)
	}
	cfg = latestConfig(t, s, "hk-01")
	if cfg.SchemaVersion != 2 || len(cfg.Rules) != 1 || cfg.Rules[0].SourceLoggingEnabled {
		t.Fatalf("disabled-only snapshot should remain schema 2: %+v", cfg)
	}
	if err = s.pool.QueryRow(ctx, `SELECT action FROM platform_audit_logs WHERE resource_id=$1 ORDER BY id DESC LIMIT 1`, ruleID).Scan(&action); err != nil || action != "rule.source-logging.disable" {
		t.Fatalf("disable audit action=%q err=%v", action, err)
	}
}

func TestAdminSetRuleSourceLoggingRejectsUnauthorizedUnavailableAndInvalidRequests(t *testing.T) {
	s, admin, customer, ruleID := sourceLoggingReadyFixture(t)
	ctx := context.Background()

	if _, err := s.AdminSetRuleSourceLogging(ctx, customer, customer.TenantID, ruleID, 1, true); !errors.Is(err, ErrForbidden) {
		t.Fatalf("tenant toggled source logging: %v", err)
	}
	for _, tc := range []struct {
		name    string
		tenant  string
		rule    string
		version int64
	}{
		{"missing tenant", "", ruleID, 1},
		{"bad rule", customer.TenantID, "not-a-uuid", 1},
		{"zero version", customer.TenantID, ruleID, 0},
	} {
		if _, err := s.AdminSetRuleSourceLogging(ctx, admin, tc.tenant, tc.rule, tc.version, true); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: %v", tc.name, err)
		}
	}

	s.sourceFlowRetentionDays = 0
	if _, err := s.AdminSetRuleSourceLogging(ctx, admin, customer.TenantID, ruleID, 1, true); !errors.Is(err, ErrSourceFlowRetentionUnavailable) {
		t.Fatalf("missing retention accepted: %v", err)
	}
	s.sourceFlowRetentionDays = 180
	if _, err := s.pool.Exec(ctx, `UPDATE nodes SET agent_source_flow_supported=false WHERE id='hk-01'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdminSetRuleSourceLogging(ctx, admin, customer.TenantID, ruleID, 1, true); !errors.Is(err, ErrSourceFlowAgentUnsupported) {
		t.Fatalf("unsupported Agent accepted: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE nodes SET agent_source_flow_supported=true,agent_mode='validation' WHERE id='hk-01'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdminSetRuleSourceLogging(ctx, admin, customer.TenantID, ruleID, 1, true); !errors.Is(err, ErrSourceFlowAgentUnsupported) {
		t.Fatalf("validation Agent accepted: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE nodes SET agent_mode='apply',agent_heartbeat_at=now()-interval '10 minutes' WHERE id='hk-01'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdminSetRuleSourceLogging(ctx, admin, customer.TenantID, ruleID, 1, true); !errors.Is(err, ErrSourceFlowAgentOffline) {
		t.Fatalf("offline Agent accepted: %v", err)
	}
}

func TestAdminSetRuleSourceLoggingRollsBackWhenAuditFails(t *testing.T) {
	s, admin, customer, ruleID := sourceLoggingReadyFixture(t)
	ctx := context.Background()
	var outboxBefore int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM config_outbox WHERE node_id='hk-01'`).Scan(&outboxBefore); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `CREATE FUNCTION reject_source_logging_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action LIKE 'rule.source-logging.%' THEN RAISE EXCEPTION 'audit unavailable'; END IF; RETURN NEW; END; $$; CREATE TRIGGER reject_source_logging_audit BEFORE INSERT ON platform_audit_logs FOR EACH ROW EXECUTE FUNCTION reject_source_logging_audit()`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdminSetRuleSourceLogging(ctx, admin, customer.TenantID, ruleID, 1, true); err == nil {
		t.Fatal("audit failure did not roll back mutation")
	}
	var enabled bool
	var version int64
	var outbox int
	if err := s.pool.QueryRow(ctx, `SELECT source_logging_enabled,desired_version FROM relay_rules WHERE id=$1`, ruleID).Scan(&enabled, &version); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM config_outbox WHERE node_id='hk-01'`).Scan(&outbox); err != nil {
		t.Fatal(err)
	}
	if enabled || version != 1 || outbox != outboxBefore {
		t.Fatalf("rollback enabled=%v version=%d outbox before=%d after=%d", enabled, version, outboxBefore, outbox)
	}
}

func TestAdminSetRuleSourceLoggingSerializesOnNodeLock(t *testing.T) {
	s, admin, customer, ruleID := sourceLoggingReadyFixture(t)
	ctx := context.Background()
	blocker, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if err = lockNode(ctx, blocker, "hk-01"); err != nil {
		t.Fatal(err)
	}
	blockedCtx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	if _, err = s.AdminSetRuleSourceLogging(blockedCtx, admin, customer.TenantID, ruleID, 1, true); err == nil {
		t.Fatal("toggle bypassed the node mutation lock")
	}
	var enabled bool
	if queryErr := s.pool.QueryRow(ctx, `SELECT source_logging_enabled FROM relay_rules WHERE id=$1`, ruleID).Scan(&enabled); queryErr != nil || enabled {
		t.Fatalf("blocked toggle changed state enabled=%v err=%v", enabled, queryErr)
	}
	if err = blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdminSetRuleSourceLogging(ctx, admin, customer.TenantID, ruleID, 1, true); err != nil {
		t.Fatalf("toggle did not recover after lock release: %v", err)
	}
}

func TestSourceLoggingSnapshotEnabledUsesSchema3(t *testing.T) {
	s, admin, customer, ruleID := sourceLoggingReadyFixture(t)
	if _, err := s.AdminSetRuleSourceLogging(context.Background(), admin, customer.TenantID, ruleID, 1, true); err != nil {
		t.Fatal(err)
	}
	cfg := latestConfig(t, s, "hk-01")
	if cfg.SchemaVersion != 3 || !cfg.Rules[0].SourceLoggingEnabled {
		t.Fatalf("snapshot: %+v", cfg)
	}
}
