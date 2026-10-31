package platform

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDrainQuarantineDigestBindsLargeCompleteSetToBoundedAttestation(t *testing.T) {
	values := make([]DrainQuarantine, 1000)
	for i := range values {
		values[i] = DrainQuarantine{RetirementID: newID(), AllocationID: newID(), Address: "192.0.2.1", Protocol: "tcp", Port: 20000 + i}
		values[i].GuardDigest = DrainGuardDigest(values[i])
	}
	count, digest := drainDigest(t, values)
	value := DrainAttestation{SchemaVersion: 1, NodeID: "hk-01", Generation: 1, CertificateFingerprint: strings.Repeat("a", 64), KernelBootID: newID(), AgentStartedAt: time.Now().UTC(), AgentVersion: "v0.9.0", JournalSequence: 1, GoodDigest: strings.Repeat("b", 64), QuarantineCount: count, QuarantineDigest: digest, NoPendingDataplane: true, NoRetirementJournal: true, UsageWALDrained: true, UpdaterStable: true, ObservedAt: time.Now().UTC()}
	raw, err := json.Marshal(value)
	if err != nil || count != len(values) || len(raw) >= 2048 {
		t.Fatalf("unbounded attestation count=%d bytes=%d err=%v", count, len(raw), err)
	}
	values[500].Port++
	values[500].GuardDigest = DrainGuardDigest(values[500])
	_, changed := drainDigest(t, values)
	if changed == digest {
		t.Fatal("complete quarantine set change did not alter digest")
	}
}

func retirementAdmin(t *testing.T, store *Store) Identity {
	t.Helper()
	who, _ := register(t, store, "node-retirement@example.com")
	if err := store.SetPlatformAdmin(context.Background(), who.Email, true); err != nil {
		t.Fatal(err)
	}
	who, _, err := store.Login(context.Background(), who.Email, "long-test-password-42")
	if err != nil {
		t.Fatal(err)
	}
	return who
}

func drainDigest(t *testing.T, values []DrainQuarantine) (int, string) {
	t.Helper()
	count, digest, err := DrainQuarantineDigest(values)
	if err != nil {
		t.Fatal(err)
	}
	return count, digest
}

func TestNodeDrainingIsIrreversibleAndFinalizationIsFullyGated(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	admin := retirementAdmin(t, store)
	if _, err := store.pool.Exec(ctx, `UPDATE nodes SET status='maintenance',revision=1 WHERE id='hk-01'`); err != nil {
		t.Fatal(err)
	}
	if err := store.StartNodeDraining(ctx, admin, "hk-01", 1); err != nil {
		t.Fatal(err)
	}
	if err := store.AdminNodeAction(ctx, admin, "hk-01", 2, "enable", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("draining reversed: %v", err)
	}
	if err := store.FinalizeNodeRetirement(ctx, admin, "hk-01", 2); !errors.Is(err, ErrConflict) {
		t.Fatalf("finalized without gates: %v", err)
	}

	fingerprint := strings.Repeat("a", 64)
	boot := newID()
	started := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond).Add(789 * time.Nanosecond)
	if _, err := store.pool.Exec(ctx, `UPDATE nodes SET desired_generation=1,applied_generation=1,certificate_fingerprint=$1,agent_kernel_boot_id=$2,agent_started_at=$3,agent_version='v0.9.0',agent_rotation_state='idle' WHERE id='hk-01'`, fingerprint, boot, started); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE config_outbox SET state='applied' WHERE node_id='hk-01' AND generation=1`); err != nil {
		t.Fatal(err)
	}
	count, digest := drainDigest(t, nil)
	value := DrainAttestation{SchemaVersion: 1, NodeID: "hk-01", Generation: 1, CertificateFingerprint: fingerprint, KernelBootID: boot, AgentStartedAt: started, AgentVersion: "v0.9.0", JournalSequence: 1, GoodDigest: strings.Repeat("b", 64), QuarantineCount: count, QuarantineDigest: digest, NoPendingDataplane: true, NoRetirementJournal: true, UsageWALDrained: true, UpdaterStable: true, ObservedAt: time.Now().UTC()}
	if err := store.ReportNodeDrainAttestation(ctx, "hk-01", fingerprint, value); err != nil {
		t.Fatal(err)
	}
	if err := store.FinalizeNodeRetirement(ctx, admin, "hk-01", 2); err != nil {
		t.Fatal(err)
	}
	var status string
	var certificate *string
	var revision int64
	if err := store.pool.QueryRow(ctx, `SELECT status,certificate_fingerprint,revision FROM nodes WHERE id='hk-01'`).Scan(&status, &certificate, &revision); err != nil {
		t.Fatal(err)
	}
	if status != "retired" || certificate != nil || revision != 3 {
		t.Fatalf("final state %s %v %d", status, certificate, revision)
	}
	if err := store.AuthenticateNode(ctx, "hk-01", fingerprint); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("retired certificate remained valid")
	}
}

func TestNodeDrainRequestedOnlyForDrainingLifecycle(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if requested, err := store.NodeDrainRequested(ctx, "hk-01"); err != nil || requested {
		t.Fatalf("unprovisioned node requested drain: %v %v", requested, err)
	}
	if _, err := store.pool.Exec(ctx, "UPDATE nodes SET status='draining' WHERE id='hk-01'"); err != nil {
		t.Fatal(err)
	}
	if requested, err := store.NodeDrainRequested(ctx, "hk-01"); err != nil || !requested {
		t.Fatalf("draining node did not request proof: %v %v", requested, err)
	}
	if _, err := store.pool.Exec(ctx, "UPDATE nodes SET status='retired' WHERE id='hk-01'"); err != nil {
		t.Fatal(err)
	}
	if requested, err := store.NodeDrainRequested(ctx, "hk-01"); err != nil || requested {
		t.Fatalf("retired node requested proof: %v %v", requested, err)
	}
}

func TestDrainAttestationRequiresExactReusableQuarantineSetAndCurrentRuntime(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	fingerprint := strings.Repeat("c", 64)
	boot := newID()
	started := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	if _, err := store.pool.Exec(ctx, `UPDATE nodes SET status='draining',desired_generation=1,applied_generation=1,certificate_fingerprint=$1,agent_kernel_boot_id=$2,agent_started_at=$3,agent_version='v0.9.0' WHERE id='hk-01'`, fingerprint, boot, started); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE config_outbox SET state='applied' WHERE node_id='hk-01' AND generation=1`); err != nil {
		t.Fatal(err)
	}
	retirementID, allocationID, ruleID, tenantID := newID(), newID(), newID(), newID()
	if _, err := store.pool.Exec(ctx, `INSERT INTO tenants(id,name) VALUES($1,'x')`, tenantID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO relay_rules(id,tenant_id,node_id,name,protocol,target_host,target_port,bandwidth_mbps,listen_port,status,ever_dispatched) VALUES($2,$1,'hk-01','x','tcp','198.51.100.2',443,1,20001,'deleted',true)`, tenantID, ruleID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO endpoint_retirements(id,allocation_id,node_id,rule_id,tenant_id,listen_address,protocol,listen_port,quarantine_generation,state,proof,proof_digest,released_at) VALUES($3,$4,'hk-01',$2,$1,'0.0.0.0','tcp',20001,1,'released','{}',repeat('a',64),now())`, tenantID, ruleID, retirementID, allocationID); err != nil {
		t.Fatal(err)
	}
	item := DrainQuarantine{RetirementID: retirementID, AllocationID: allocationID, Address: "0.0.0.0", Protocol: "tcp", Port: 20001}
	item.GuardDigest = DrainGuardDigest(item)
	base := DrainAttestation{SchemaVersion: 1, NodeID: "hk-01", Generation: 1, CertificateFingerprint: fingerprint, KernelBootID: boot, AgentStartedAt: started, AgentVersion: "v0.9.0", JournalSequence: 2, GoodDigest: strings.Repeat("d", 64), QuarantineCount: 1, QuarantineDigest: strings.Repeat("e", 64), NoPendingDataplane: true, NoRetirementJournal: true, UsageWALDrained: true, UpdaterStable: true, ObservedAt: time.Now().UTC()}
	if err := store.ReportNodeDrainAttestation(ctx, "hk-01", fingerprint, base); !errors.Is(err, ErrInvalid) {
		t.Fatalf("wildcard quarantine accepted: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE endpoint_retirements SET listen_address='192.0.2.1' WHERE id=$1`, retirementID); err != nil {
		t.Fatal(err)
	}
	item.Address = "192.0.2.1"
	item.GuardDigest = DrainGuardDigest(item)
	base.QuarantineCount, base.QuarantineDigest = drainDigest(t, []DrainQuarantine{item})
	if err := store.ReportNodeDrainAttestation(ctx, "hk-01", fingerprint, base); err != nil {
		t.Fatal(err)
	}
	if err := store.ReportAgentHeartbeat(ctx, "hk-01", fingerprint, AgentHeartbeat{Version: "v0.9.0", Mode: "apply", StartedAt: started, CertificateNotAfter: time.Now().Add(time.Hour), RotationState: "idle", KernelBootID: newID(), JournalSequence: base.JournalSequence}); err != nil {
		t.Fatal(err)
	}
	var live int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM node_drain_attestations WHERE node_id='hk-01' AND consumed_at IS NULL`).Scan(&live); err != nil || live != 0 {
		t.Fatalf("runtime change retained attestation count=%d err=%v", live, err)
	}
	base.KernelBootID = boot
	base.QuarantineCount, base.QuarantineDigest = drainDigest(t, nil)
	if err := store.ReportNodeDrainAttestation(ctx, "hk-01", fingerprint, base); !errors.Is(err, ErrConflict) {
		t.Fatalf("missing quarantine accepted: %v", err)
	}
}

func TestDrainAttestationRequiresAllFinalGatesAndCurrentNodeRevision(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	admin := retirementAdmin(t, store)
	fingerprint := strings.Repeat("e", 64)
	boot := newID()
	started := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	if _, err := store.pool.Exec(ctx, `UPDATE nodes SET status='draining',revision=2,desired_generation=1,applied_generation=1,certificate_fingerprint=$1,agent_kernel_boot_id=$2,agent_started_at=$3,agent_version='v0.9.0',agent_rotation_state='idle' WHERE id='hk-01'`, fingerprint, boot, started); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE config_outbox SET state='applied' WHERE node_id='hk-01' AND generation=1`); err != nil {
		t.Fatal(err)
	}
	count, digest := drainDigest(t, nil)
	value := DrainAttestation{SchemaVersion: 1, NodeID: "hk-01", Generation: 1, CertificateFingerprint: fingerprint, KernelBootID: boot, AgentStartedAt: started, AgentVersion: "v0.9.0", JournalSequence: 1, GoodDigest: strings.Repeat("f", 64), QuarantineCount: count, QuarantineDigest: digest, NoPendingDataplane: true, NoRetirementJournal: true, UsageWALDrained: true, UpdaterStable: true, ObservedAt: time.Now().UTC()}
	if _, err := store.pool.Exec(ctx, `INSERT INTO node_agent_upgrades(id,node_id,target_version,previous_version,state,amd64_sha256,arm64_sha256) VALUES($1,'hk-01','v0.9.0','v0.8.0','queued',$2,$2)`, newID(), strings.Repeat("1", 64)); err != nil {
		t.Fatal(err)
	}
	if err := store.ReportNodeDrainAttestation(ctx, "hk-01", fingerprint, value); !errors.Is(err, ErrConflict) {
		t.Fatalf("attestation accepted while upgrade active: %v", err)
	}
	if _, err := store.pool.Exec(ctx, "UPDATE node_agent_upgrades SET state='cancelled',completed_at=now()"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, "UPDATE nodes SET revision=revision+1 WHERE id='hk-01'"); err != nil {
		t.Fatal(err)
	}
	if err := store.ReportNodeDrainAttestation(ctx, "hk-01", fingerprint, value); err != nil {
		nodes, _ := store.AdminNodes(ctx, admin)
		t.Fatalf("attestation rejected: %v nodes=%+v", err, nodes)
	}
	if _, err := store.pool.Exec(ctx, "UPDATE nodes SET revision=revision+1 WHERE id='hk-01'"); err != nil {
		t.Fatal(err)
	}
	if err := store.FinalizeNodeRetirement(ctx, admin, "hk-01", 4); !errors.Is(err, ErrConflict) {
		t.Fatalf("old revision attestation finalized node: %v", err)
	}
}
