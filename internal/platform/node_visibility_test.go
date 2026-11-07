package platform

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestAdminNodeVisibilityIsAuditedAndIndependentFromOperationalRevision(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)

	nodes, err := s.AdminNodes(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	var before ManagedNode
	for _, node := range nodes {
		if node.ID == "hk-01" {
			before = node
			break
		}
	}
	if before.ID == "" || before.Hidden || before.VisibilityRevision != 1 {
		t.Fatalf("unexpected initial visibility: %+v", before)
	}

	if err = s.AdminSetNodeVisibility(ctx, admin, before.ID, before.VisibilityRevision, true); err != nil {
		t.Fatal(err)
	}
	nodes, err = s.AdminNodes(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	var hidden ManagedNode
	for _, node := range nodes {
		if node.ID == before.ID {
			hidden = node
			break
		}
	}
	if !hidden.Hidden || hidden.VisibilityRevision != 2 || hidden.Revision != before.Revision {
		t.Fatalf("visibility changed operational state: before=%+v after=%+v", before, hidden)
	}
	if err = s.AdminSetNodeVisibility(ctx, admin, before.ID, before.VisibilityRevision, false); err != ErrConflict {
		t.Fatalf("stale visibility revision accepted: %v", err)
	}
	if err = s.AdminSetNodeVisibility(ctx, admin, before.ID, hidden.VisibilityRevision, false); err != nil {
		t.Fatal(err)
	}

	var hiddenValue bool
	var visibilityRevision, nodeRevision, audits int64
	if err = s.pool.QueryRow(ctx, `SELECT v.hidden,v.revision,n.revision,(SELECT count(*) FROM platform_audit_logs WHERE resource_id=n.id AND action IN ('node.hide','node.unhide')) FROM nodes n JOIN platform_node_visibility v ON v.node_id=n.id WHERE n.id=$1`, before.ID).Scan(&hiddenValue, &visibilityRevision, &nodeRevision, &audits); err != nil {
		t.Fatal(err)
	}
	if hiddenValue || visibilityRevision != 3 || nodeRevision != before.Revision || audits != 2 {
		t.Fatalf("unexpected restored visibility hidden=%v visibilityRevision=%d nodeRevision=%d audits=%d", hiddenValue, visibilityRevision, nodeRevision, audits)
	}
}

func TestAdminNodeVisibilityRejectsTenantsAndRollsBackWhenAuditFails(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	tenant, _ := register(t, s, "visibility-tenant@example.com")

	if err := s.AdminSetNodeVisibility(ctx, tenant, "hk-01", 1, true); err != ErrForbidden {
		t.Fatalf("tenant changed platform visibility: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `ALTER TABLE platform_audit_logs ADD CONSTRAINT reject_node_visibility_audit CHECK(action NOT IN ('node.hide','node.unhide')) NOT VALID`); err != nil {
		t.Fatal(err)
	}
	if err := s.AdminSetNodeVisibility(ctx, admin, "hk-01", 1, true); err == nil {
		t.Fatal("visibility committed despite audit failure")
	}
	var rows int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM platform_node_visibility WHERE node_id='hk-01'").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("visibility row survived audit rollback: %d", rows)
	}
}

func TestAdminNodeVisibilityPreservesDrainAttestationAndRetirementEligibility(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	if _, err := s.pool.Exec(ctx, `UPDATE nodes SET status='maintenance',revision=1 WHERE id='hk-01'`); err != nil {
		t.Fatal(err)
	}
	if err := s.StartNodeDraining(ctx, admin, "hk-01", 1); err != nil {
		t.Fatal(err)
	}
	fingerprint := strings.Repeat("a", 64)
	boot := newID()
	started := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond).Add(789 * time.Nanosecond)
	if _, err := s.pool.Exec(ctx, `UPDATE nodes SET desired_generation=1,applied_generation=1,certificate_fingerprint=$1,agent_kernel_boot_id=$2,agent_started_at=$3,agent_version='v0.9.0',agent_rotation_state='idle' WHERE id='hk-01'`, fingerprint, boot, started); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE config_outbox SET state='applied' WHERE node_id='hk-01' AND generation=1`); err != nil {
		t.Fatal(err)
	}
	count, digest := drainDigest(t, nil)
	attestation := DrainAttestation{SchemaVersion: 1, NodeID: "hk-01", Generation: 1, CertificateFingerprint: fingerprint, KernelBootID: boot, AgentStartedAt: started, AgentVersion: "v0.9.0", JournalSequence: 1, GoodDigest: strings.Repeat("b", 64), QuarantineCount: count, QuarantineDigest: digest, NoPendingDataplane: true, NoRetirementJournal: true, UsageWALDrained: true, UpdaterStable: true, ObservedAt: time.Now().UTC()}
	if err := s.ReportNodeDrainAttestation(ctx, "hk-01", fingerprint, attestation); err != nil {
		t.Fatal(err)
	}

	node := func() ManagedNode {
		t.Helper()
		nodes, err := s.AdminNodes(ctx, admin)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range nodes {
			if item.ID == "hk-01" {
				return item
			}
		}
		t.Fatal("hk-01 missing")
		return ManagedNode{}
	}
	before := node()
	if !before.RetirementEligible || before.Revision != 2 || before.VisibilityRevision != 1 {
		t.Fatalf("invalid precondition: %+v", before)
	}
	if err := s.AdminSetNodeVisibility(ctx, admin, before.ID, before.VisibilityRevision, true); err != nil {
		t.Fatal(err)
	}
	after := node()
	if !after.Hidden || !after.RetirementEligible || after.Revision != before.Revision {
		t.Fatalf("hide changed retirement evidence: before=%+v after=%+v", before, after)
	}
	if err := s.AdminSetNodeVisibility(ctx, admin, after.ID, after.VisibilityRevision, false); err != nil {
		t.Fatal(err)
	}
	var live int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM node_drain_attestations WHERE node_id='hk-01' AND consumed_at IS NULL AND node_revision=2`).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if restored := node(); restored.Hidden || !restored.RetirementEligible || restored.Revision != before.Revision || live != 1 {
		t.Fatalf("restore changed retirement evidence: node=%+v liveAttestations=%d", restored, live)
	}
}
