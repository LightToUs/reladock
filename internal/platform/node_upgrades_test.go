package platform

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func upgradeTarget() ReleaseTarget {
	return ReleaseTarget{Version: "v0.9.0", AMD64SHA256: strings.Repeat("a", 64), ARM64SHA256: strings.Repeat("b", 64)}
}

func readyUpgradeNode(t *testing.T, s *Store) (Identity, string, int64) {
	t.Helper()
	who, _ := adminIdentity(t, s)
	fingerprint := strings.Repeat("c", 64)
	if _, err := s.pool.Exec(context.Background(), `UPDATE nodes SET status='ready',certificate_fingerprint=$2,agent_version='v0.8.0',agent_heartbeat_at=now(),last_seen_at=now() WHERE id=$1`, "hk-01", fingerprint); err != nil {
		t.Fatal(err)
	}
	var revision int64
	if err := s.pool.QueryRow(context.Background(), "SELECT revision FROM nodes WHERE id='hk-01'").Scan(&revision); err != nil {
		t.Fatal(err)
	}
	return who, fingerprint, revision
}

func TestAgentUpgradeRequestIsAuthorizedTransactionalAndIdempotent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin, _, revision := readyUpgradeNode(t, s)
	owner, _ := register(t, s, "ordinary-owner@example.com")

	if _, err := s.RequestAgentUpgrade(ctx, owner, "hk-01", revision, upgradeTarget()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("ordinary owner requested upgrade: %v", err)
	}
	first, err := s.RequestAgentUpgrade(ctx, admin, "hk-01", revision, upgradeTarget())
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == "" || first.State != "queued" || first.PreviousVersion != "v0.8.0" || first.TargetVersion != "v0.9.0" {
		t.Fatalf("unexpected task: %+v", first)
	}
	replayed, err := s.RequestAgentUpgrade(ctx, admin, "hk-01", revision, upgradeTarget())
	if err != nil || replayed.ID != first.ID {
		t.Fatalf("request replay created a new task: %+v %v", replayed, err)
	}
	var tasks, audits int
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM node_agent_upgrades").Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM platform_audit_logs WHERE action='node.agent-upgrade-request'").Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if tasks != 1 || audits != 1 {
		t.Fatalf("idempotent request persisted tasks=%d audits=%d", tasks, audits)
	}
}

func TestAgentUpgradeRequestEnforcesNodeAndGlobalGates(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*Store)
		rev   int64
		want  error
	}{
		{name: "revision conflict", rev: 99, want: ErrConflict},
		{name: "offline", setup: func(s *Store) {
			_, _ = s.pool.Exec(context.Background(), "UPDATE nodes SET agent_heartbeat_at=now()-interval '10 minutes'")
		}, want: ErrConflict},
		{name: "maintenance", setup: func(s *Store) { _, _ = s.pool.Exec(context.Background(), "UPDATE nodes SET status='maintenance'") }, want: ErrConflict},
		{name: "unknown current", setup: func(s *Store) { _, _ = s.pool.Exec(context.Background(), "UPDATE nodes SET agent_version='dev'") }, want: ErrConflict},
		{name: "non-increasing", setup: func(s *Store) { _, _ = s.pool.Exec(context.Background(), "UPDATE nodes SET agent_version='v0.9.0'") }, want: ErrConflict},
		{name: "cross-major", setup: func(s *Store) { _, _ = s.pool.Exec(context.Background(), "UPDATE nodes SET agent_version='v1.0.0'") }, want: ErrConflict},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			admin, _, revision := readyUpgradeNode(t, s)
			if test.setup != nil {
				test.setup(s)
			}
			if test.rev != 0 {
				revision = test.rev
			}
			if _, err := s.RequestAgentUpgrade(ctx, admin, "hk-01", revision, upgradeTarget()); !errors.Is(err, test.want) {
				t.Fatalf("gate error = %v, want %v", err, test.want)
			}
			var count int
			_ = s.pool.QueryRow(ctx, "SELECT count(*) FROM node_agent_upgrades").Scan(&count)
			if count != 0 {
				t.Fatalf("rejected request persisted %d tasks", count)
			}
		})
	}

	s := testStore(t)
	ctx := context.Background()
	admin, _, revision := readyUpgradeNode(t, s)
	if _, err := s.RequestAgentUpgrade(ctx, admin, "hk-01", revision, upgradeTarget()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO nodes(id,name,region,status,listen_address,capacity_mbps,revision,agent_version,agent_heartbeat_at) VALUES('sg-01','Singapore','Singapore','ready','0.0.0.0',1000,1,'v0.8.0',now())`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RequestAgentUpgrade(ctx, admin, "sg-01", 1, upgradeTarget()); !errors.Is(err, ErrConflict) {
		t.Fatalf("second global active task accepted: %v", err)
	}
}

func TestAgentUpgradeAuditFailureRollsBackRequest(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin, _, revision := readyUpgradeNode(t, s)
	if _, err := s.pool.Exec(ctx, `ALTER TABLE platform_audit_logs ADD CONSTRAINT reject_upgrade_audit CHECK(action<>'node.agent-upgrade-request') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RequestAgentUpgrade(ctx, admin, "hk-01", revision, upgradeTarget()); err == nil {
		t.Fatal("audit failure accepted")
	}
	var tasks int
	var currentRevision int64
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM node_agent_upgrades").Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, "SELECT revision FROM nodes WHERE id='hk-01'").Scan(&currentRevision); err != nil {
		t.Fatal(err)
	}
	if tasks != 0 || currentRevision != revision {
		t.Fatalf("failed audit leaked task=%d revision=%d", tasks, currentRevision)
	}
}

func TestAgentUpgradeReportAuditFailureRollsBackTransition(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin, fingerprint, revision := readyUpgradeNode(t, s)
	task, err := s.RequestAgentUpgrade(ctx, admin, "hk-01", revision, upgradeTarget())
	if err != nil {
		t.Fatal(err)
	}
	claim, err := s.ClaimAgentUpgrade(ctx, "hk-01", fingerprint, "amd64", "v0.8.0")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `ALTER TABLE platform_audit_logs ADD CONSTRAINT reject_upgrade_report_audit CHECK(action<>'node.agent-upgrade-installing') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReportAgentUpgrade(ctx, "hk-01", fingerprint, UpgradeReport{ID: task.ID, LeaseToken: claim.LeaseToken, State: "installing"}); err == nil {
		t.Fatal("report audit failure accepted")
	}
	var state string
	if err = s.pool.QueryRow(ctx, "SELECT state FROM node_agent_upgrades WHERE id=$1", task.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "claimed" {
		t.Fatalf("failed audit persisted state %q", state)
	}
}

func TestAgentUpgradeClaimSelectsArchitectureAndProtectsLease(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin, fingerprint, revision := readyUpgradeNode(t, s)
	task, err := s.RequestAgentUpgrade(ctx, admin, "hk-01", revision, upgradeTarget())
	if err != nil {
		t.Fatal(err)
	}
	if claim, err := s.ClaimAgentUpgrade(ctx, "hk-01", strings.Repeat("d", 64), "amd64", "v0.8.0"); !errors.Is(err, ErrUnauthorized) || claim != nil {
		t.Fatalf("wrong identity claimed task: %+v %v", claim, err)
	}
	claim, err := s.ClaimAgentUpgrade(ctx, "hk-01", fingerprint, "arm64", "v0.8.0")
	if err != nil {
		t.Fatal(err)
	}
	if claim == nil || claim.ID != task.ID || claim.SHA256 != strings.Repeat("b", 64) || claim.LeaseToken == "" {
		t.Fatalf("bad architecture claim: %+v", claim)
	}
	var digest string
	if err = s.pool.QueryRow(ctx, "SELECT lease_token_hash FROM node_agent_upgrades WHERE id=$1", task.ID).Scan(&digest); err != nil {
		t.Fatal(err)
	}
	if digest == claim.LeaseToken || len(digest) != 64 {
		t.Fatal("plaintext or malformed lease persisted")
	}
	if _, err = s.pool.Exec(ctx, "UPDATE node_agent_upgrades SET lease_expires_at=now()-interval '1 second' WHERE id=$1", task.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := s.ClaimAgentUpgrade(ctx, "hk-01", fingerprint, "arm64", "v0.8.0")
	if err != nil || reclaimed.ID != task.ID || reclaimed.LeaseToken == claim.LeaseToken {
		t.Fatalf("expired pre-install lease was not safely replaced: %+v %v", reclaimed, err)
	}
	if _, err = s.ReportAgentUpgrade(ctx, "hk-01", fingerprint, UpgradeReport{ID: task.ID, LeaseToken: reclaimed.LeaseToken, State: "installing"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, "UPDATE node_agent_upgrades SET lease_expires_at=now()-interval '1 second' WHERE id=$1", task.ID); err != nil {
		t.Fatal(err)
	}
	if second, err := s.ClaimAgentUpgrade(ctx, "hk-01", fingerprint, "arm64", "v0.8.0"); err != nil || second != nil {
		t.Fatalf("installing task was reclaimed: %+v %v", second, err)
	}
}

func TestAgentUpgradeReportsFollowExplicitEdgesAndHeartbeatGate(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin, fingerprint, revision := readyUpgradeNode(t, s)
	task, err := s.RequestAgentUpgrade(ctx, admin, "hk-01", revision, upgradeTarget())
	if err != nil {
		t.Fatal(err)
	}
	claim, err := s.ClaimAgentUpgrade(ctx, "hk-01", fingerprint, "amd64", "v0.8.0")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReportAgentUpgrade(ctx, "hk-01", fingerprint, UpgradeReport{ID: task.ID, LeaseToken: strings.Repeat("0", 64), State: "installing"}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong lease accepted: %v", err)
	}
	if _, err = s.ReportAgentUpgrade(ctx, "hk-01", fingerprint, UpgradeReport{ID: task.ID, LeaseToken: claim.LeaseToken, State: "verifying"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("skipped state accepted: %v", err)
	}
	for _, state := range []string{"installing", "verifying"} {
		if _, err = s.ReportAgentUpgrade(ctx, "hk-01", fingerprint, UpgradeReport{ID: task.ID, LeaseToken: claim.LeaseToken, State: state}); err != nil {
			t.Fatalf("report %s: %v", state, err)
		}
	}
	if _, err = s.ReportAgentUpgrade(ctx, "hk-01", fingerprint, UpgradeReport{ID: task.ID, LeaseToken: claim.LeaseToken, State: "succeeded"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("success without target heartbeat accepted: %v", err)
	}
	if _, err = s.pool.Exec(ctx, "UPDATE nodes SET agent_version='v0.9.0',agent_heartbeat_at=now() WHERE id='hk-01'"); err != nil {
		t.Fatal(err)
	}
	finished, err := s.ReportAgentUpgrade(ctx, "hk-01", fingerprint, UpgradeReport{ID: task.ID, LeaseToken: claim.LeaseToken, State: "succeeded"})
	if err != nil || finished.State != "succeeded" || finished.CompletedAt == nil {
		t.Fatalf("valid success rejected: %+v %v", finished, err)
	}
	replayed, err := s.ReportAgentUpgrade(ctx, "hk-01", fingerprint, UpgradeReport{ID: task.ID, LeaseToken: claim.LeaseToken, State: "succeeded"})
	if err != nil || replayed.State != "succeeded" {
		t.Fatalf("exact terminal replay rejected: %+v %v", replayed, err)
	}
	if _, err = s.ReportAgentUpgrade(ctx, "hk-01", fingerprint, UpgradeReport{ID: task.ID, LeaseToken: claim.LeaseToken, State: "rolled-back", ErrorCode: "health-timeout"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("terminal task changed: %v", err)
	}
}

func TestAgentUpgradeRejectsUnboundedErrorsAndSupportsSafeCancellation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin, fingerprint, revision := readyUpgradeNode(t, s)
	task, err := s.RequestAgentUpgrade(ctx, admin, "hk-01", revision, upgradeTarget())
	if err != nil {
		t.Fatal(err)
	}
	claim, err := s.ClaimAgentUpgrade(ctx, "hk-01", fingerprint, "amd64", "v0.8.0")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReportAgentUpgrade(ctx, "hk-01", fingerprint, UpgradeReport{ID: task.ID, LeaseToken: claim.LeaseToken, State: "failed", ErrorCode: "raw database error"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unbounded error accepted: %v", err)
	}
	var nodeRevision int64
	if err = s.pool.QueryRow(ctx, "SELECT revision FROM nodes WHERE id='hk-01'").Scan(&nodeRevision); err != nil {
		t.Fatal(err)
	}
	cancelled, err := s.CancelAgentUpgrade(ctx, admin, "hk-01", nodeRevision)
	if err != nil || cancelled.State != "cancelled" {
		t.Fatalf("safe cancellation failed: %+v %v", cancelled, err)
	}
}

func TestAdminNodesExposeOnlyLatestPublicUpgradeFacts(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin, _, revision := readyUpgradeNode(t, s)
	task, err := s.RequestAgentUpgrade(ctx, admin, "hk-01", revision, upgradeTarget())
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := s.AdminNodes(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].Upgrade == nil || nodes[0].Upgrade.ID != task.ID || nodes[0].Upgrade.TargetVersion != "v0.9.0" || nodes[0].Upgrade.State != "queued" {
		t.Fatalf("upgrade summary missing: %+v", nodes)
	}
	if nodes[0].Upgrade.ErrorCode != "" || nodes[0].Upgrade.UpdatedAt.Before(time.Now().Add(-time.Minute)) {
		t.Fatalf("unexpected public summary: %+v", nodes[0].Upgrade)
	}
}
