package platform

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func applyLatestConfig(t *testing.T, s *Store, nodeID string) NodeConfig {
	t.Helper()
	ctx := context.Background()
	job, err := s.ClaimConfig(ctx, nodeID, "retirement-test")
	if err != nil || job == nil {
		t.Fatalf("claim config: %+v %v", job, err)
	}
	report := ConfigReport{LeaseToken: job.LeaseToken, Checksum: job.Checksum, Outcome: "applying"}
	if err = s.BeginApplication(ctx, nodeID, job.ID, report); err != nil {
		t.Fatal(err)
	}
	report.Outcome = "applied"
	if err = s.ReportConfig(ctx, nodeID, job.ID, report); err != nil {
		t.Fatal(err)
	}
	var cfg NodeConfig
	if err = json.Unmarshal(job.Payload, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func dispatchedDeletedRule(t *testing.T, protocol string) (*Store, Identity, Rule, NodeConfig) {
	t.Helper()
	s := testStore(t)
	s.allowUnentitledRules = true
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, "UPDATE nodes SET listen_address='198.51.100.10' WHERE id='hk-01'"); err != nil {
		t.Fatal(err)
	}
	who, _ := register(t, s, "retirement-"+protocol+"@example.com")
	rule, err := s.CreateRule(ctx, who, "retirement-create-"+protocol, ruleInput(protocol))
	if err != nil {
		t.Fatal(err)
	}
	applyLatestConfig(t, s, "hk-01")
	if err = s.DeleteRule(ctx, who, rule.ID); err != nil {
		t.Fatal(err)
	}
	removed := applyLatestConfig(t, s, "hk-01")
	return s, who, rule, removed
}

func TestDeletedDispatchedRuleDeliversAllocationBoundQuarantine(t *testing.T) {
	s, _, rule, removed := dispatchedDeletedRule(t, "both")
	if removed.SchemaVersion != 2 || len(removed.Rules) != 0 || len(removed.Retirements) != 2 {
		t.Fatalf("removal snapshot: %+v", removed)
	}
	seen := map[string]bool{}
	for _, item := range removed.Retirements {
		if item.RuleID != rule.ID || !upgradeIDPattern.MatchString(item.ID) || !upgradeIDPattern.MatchString(item.AllocationID) || item.QuarantineGeneration != removed.Generation || (item.Protocol != "tcp" && item.Protocol != "udp") {
			t.Fatalf("invalid retirement snapshot: %+v", item)
		}
		seen[item.Protocol] = true
	}
	if !seen["tcp"] || !seen["udp"] {
		t.Fatalf("missing protocol retirement: %v", seen)
	}
	var queued, held int
	if err := s.pool.QueryRow(context.Background(), "SELECT count(*) FROM endpoint_retirements WHERE rule_id=$1 AND state='queued'", rule.ID).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(context.Background(), "SELECT count(*) FROM endpoint_allocations WHERE rule_id=$1", rule.ID).Scan(&held); err != nil {
		t.Fatal(err)
	}
	if queued != 2 || held != 2 {
		t.Fatalf("unsafe removal queued=%d held=%d", queued, held)
	}
}

func validRetirementProof(claim *RetirementClaim) RetirementProof {
	return RetirementProof{
		SchemaVersion:        1,
		TaskID:               claim.ID,
		AllocationID:         claim.AllocationID,
		NodeID:               claim.NodeID,
		Address:              claim.Address,
		Protocol:             claim.Protocol,
		Port:                 claim.Port,
		QuarantineGeneration: claim.QuarantineGeneration,
		GuardBeforeDigest:    strings.Repeat("a", 64),
		GuardAfterDigest:     strings.Repeat("b", 64),
		ConntrackVersion:     "conntrack v1.4.8",
		FilterVersion:        "reladock-conntrack-v1",
		ZeroMatches:          true,
		ForwardGuardRemoved:  true,
		KernelBootID:         newID(),
		JournalSequence:      7,
		GoodDigest:           strings.Repeat("c", 64),
		ObservedAt:           time.Now().UTC(),
	}
}

func TestRetirementClaimBeginReleaseAndReplay(t *testing.T) {
	s, who, rule, _ := dispatchedDeletedRule(t, "tcp")
	ctx := context.Background()
	fingerprint := strings.Repeat("d", 64)
	if _, err := s.pool.Exec(ctx, "UPDATE nodes SET certificate_fingerprint=$2 WHERE id=$1", "hk-01", fingerprint); err != nil {
		t.Fatal(err)
	}
	claim, err := s.ClaimRetirement(ctx, "hk-01", fingerprint)
	if err != nil || claim == nil || len(claim.LeaseToken) != 64 {
		t.Fatalf("claim: %+v %v", claim, err)
	}
	if duplicate, err := s.ClaimRetirement(ctx, "hk-01", fingerprint); err != nil || duplicate != nil {
		t.Fatalf("concurrent claim: %+v %v", duplicate, err)
	}
	if err = s.BeginRetirementPurge(ctx, "hk-01", fingerprint, claim.ID, strings.Repeat("e", 64)); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong lease began purge: %v", err)
	}
	if err = s.BeginRetirementPurge(ctx, "hk-01", fingerprint, claim.ID, claim.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if err = s.BeginRetirementPurge(ctx, "hk-01", fingerprint, claim.ID, claim.LeaseToken); err != nil {
		t.Fatalf("begin replay: %v", err)
	}
	if job, err := s.ClaimConfig(ctx, "hk-01", "retirement-test", fingerprint); err != nil || job != nil {
		t.Fatalf("config crossed purge barrier: %+v %v", job, err)
	}
	proof := validRetirementProof(claim)
	report := RetirementReport{LeaseToken: claim.LeaseToken, Proof: proof}
	if err = s.ReportRetirement(ctx, "hk-01", fingerprint, claim.ID, report); err != nil {
		t.Fatal(err)
	}
	if err = s.ReportRetirement(ctx, "hk-01", fingerprint, claim.ID, report); err != nil {
		t.Fatalf("report replay: %v", err)
	}
	changed := report
	changed.Proof.JournalSequence++
	if err = s.ReportRetirement(ctx, "hk-01", fingerprint, claim.ID, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed replay: %v", err)
	}
	var endpoints int
	var state, digest string
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM endpoint_allocations WHERE rule_id=$1", rule.ID).Scan(&endpoints); err != nil {
		t.Fatal(err)
	}
	if err = s.pool.QueryRow(ctx, "SELECT state,proof_digest FROM endpoint_retirements WHERE id=$1", claim.ID).Scan(&state, &digest); err != nil {
		t.Fatal(err)
	}
	if endpoints != 0 || state != "released" || !fingerprintPattern.MatchString(digest) {
		t.Fatalf("release endpoints=%d state=%q digest=%q", endpoints, state, digest)
	}
	replacement, err := s.CreateRule(ctx, who, "retirement-replacement", ruleInput("tcp"))
	if err != nil || replacement.ListenPort != rule.ListenPort {
		t.Fatalf("released port not reusable: %+v %v", replacement, err)
	}
	var predecessor string
	if err = s.pool.QueryRow(ctx, "SELECT predecessor_retirement_id::text FROM endpoint_allocations WHERE rule_id=$1", replacement.ID).Scan(&predecessor); err != nil || predecessor != claim.ID {
		t.Fatalf("predecessor lineage=%q err=%v", predecessor, err)
	}
	replacementConfig := applyLatestConfig(t, s, "hk-01")
	if len(replacementConfig.Allocations) != 1 || replacementConfig.Allocations[0].PredecessorRetirementID != claim.ID {
		t.Fatalf("replacement snapshot lineage: %+v", replacementConfig.Allocations)
	}
	var consumedBy string
	if err = s.pool.QueryRow(ctx, "SELECT consumed_by_allocation_id::text FROM endpoint_retirements WHERE id=$1", claim.ID).Scan(&consumedBy); err != nil || consumedBy != replacementConfig.Allocations[0].ID {
		t.Fatalf("quarantine consumption=%q err=%v", consumedBy, err)
	}
}

func TestRetirementClaimBlockedByApplyingConfigAndUpgrade(t *testing.T) {
	s, _, _, _ := dispatchedDeletedRule(t, "udp")
	ctx := context.Background()
	fingerprint := strings.Repeat("f", 64)
	if _, err := s.pool.Exec(ctx, "UPDATE nodes SET certificate_fingerprint=$2 WHERE id=$1", "hk-01", fingerprint); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO node_agent_upgrades(id,node_id,target_version,amd64_sha256,arm64_sha256,previous_version,state) VALUES($1,'hk-01','v0.9.1',$2,$2,'v0.9.0','queued')`, newID(), strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if claim, err := s.ClaimRetirement(ctx, "hk-01", fingerprint); err != nil || claim != nil {
		t.Fatalf("retirement crossed upgrade barrier: %+v %v", claim, err)
	}
	if claim, err := s.ClaimAgentUpgrade(ctx, "hk-01", fingerprint, "amd64", "v0.9.0"); err != nil || claim != nil {
		t.Fatalf("upgrade crossed retirement barrier: %+v %v", claim, err)
	}
}

func TestRetirementReleaseRollsBackWithAuditFailure(t *testing.T) {
	s, _, rule, _ := dispatchedDeletedRule(t, "tcp")
	ctx := context.Background()
	fingerprint := strings.Repeat("9", 64)
	if _, err := s.pool.Exec(ctx, "UPDATE nodes SET certificate_fingerprint=$2 WHERE id=$1", "hk-01", fingerprint); err != nil {
		t.Fatal(err)
	}
	claim, err := s.ClaimRetirement(ctx, "hk-01", fingerprint)
	if err != nil || claim == nil {
		t.Fatalf("claim: %+v %v", claim, err)
	}
	if err = s.BeginRetirementPurge(ctx, "hk-01", fingerprint, claim.ID, claim.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `ALTER TABLE platform_audit_logs ADD CONSTRAINT reject_retirement_release CHECK (action<>'node.retirement-released') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	err = s.ReportRetirement(ctx, "hk-01", fingerprint, claim.ID, RetirementReport{LeaseToken: claim.LeaseToken, Proof: validRetirementProof(claim)})
	if err == nil {
		t.Fatal("release committed without audit")
	}
	var endpoints int
	var state string
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM endpoint_allocations WHERE rule_id=$1", rule.ID).Scan(&endpoints); err != nil {
		t.Fatal(err)
	}
	if err = s.pool.QueryRow(ctx, "SELECT state FROM endpoint_retirements WHERE id=$1", claim.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if endpoints != 1 || state != "purging" {
		t.Fatalf("audit rollback endpoints=%d state=%q", endpoints, state)
	}
}
