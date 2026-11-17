package platform

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func validAgentHeartbeat(now time.Time) AgentHeartbeat {
	cpu, memory, disk, conntrack := 20, 30, 40, 10
	return AgentHeartbeat{
		Version:             "v0.8.0",
		Mode:                "validation",
		StartedAt:           now.Add(-time.Minute),
		CertificateNotAfter: now.Add(20 * 24 * time.Hour),
		RotationState:       "idle",
		Resources: ResourceObservation{
			SampledAt:        now,
			CPUPercent:       &cpu,
			MemoryPercent:    &memory,
			DiskPercent:      &disk,
			ConntrackPercent: &conntrack,
			DataplaneState:   "not-applicable",
		},
	}
}

func TestAgentHeartbeatPersistsOnlyAfterExactCredentialCheck(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	fingerprint := strings.Repeat("a", 64)
	if _, err := s.pool.Exec(ctx, "UPDATE nodes SET certificate_fingerprint=$1 WHERE id='hk-01'", fingerprint); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	heartbeat := validAgentHeartbeat(now)
	if err := s.ReportAgentHeartbeat(ctx, "hk-01", strings.Repeat("b", 64), heartbeat); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong credential: %v", err)
	}
	var before *time.Time
	if err := s.pool.QueryRow(ctx, "SELECT agent_heartbeat_at FROM nodes WHERE id='hk-01'").Scan(&before); err != nil || before != nil {
		t.Fatalf("unauthorized heartbeat persisted: %v %v", before, err)
	}
	if err := s.ReportAgentHeartbeat(ctx, "hk-01", fingerprint, heartbeat); err != nil {
		t.Fatal(err)
	}
	var version, mode, rotation, code string
	var started, expires, contacted time.Time
	if err := s.pool.QueryRow(ctx, `SELECT agent_version,agent_mode,agent_started_at,agent_certificate_not_after,agent_rotation_state,agent_rotation_error_code,agent_heartbeat_at FROM nodes WHERE id='hk-01'`).Scan(&version, &mode, &started, &expires, &rotation, &code, &contacted); err != nil {
		t.Fatal(err)
	}
	if version != heartbeat.Version || mode != heartbeat.Mode || !started.Equal(heartbeat.StartedAt) || !expires.Equal(heartbeat.CertificateNotAfter) || rotation != "idle" || code != "" || contacted.Before(now) {
		t.Fatalf("persisted heartbeat mismatch: %q %q %v %v %q %q %v", version, mode, started, expires, rotation, code, contacted)
	}
}

func TestSourceFlowCapabilityUsesOnlyAuthenticatedLatestHeartbeat(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	fingerprint := strings.Repeat("c", 64)
	if err := s.EnrollNode(ctx, "hk-01", fingerprint); err != nil {
		t.Fatal(err)
	}
	heartbeat := validAgentHeartbeat(time.Now().UTC())
	heartbeat.SourceFlowSupported = true
	if err := s.ReportAgentHeartbeat(ctx, "hk-01", strings.Repeat("d", 64), heartbeat); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong credential capability: %v", err)
	}
	var supported bool
	if err := s.pool.QueryRow(ctx, `SELECT agent_source_flow_supported FROM nodes WHERE id='hk-01'`).Scan(&supported); err != nil || supported {
		t.Fatalf("unauthorized capability persisted=%v err=%v", supported, err)
	}
	if err := s.ReportAgentHeartbeat(ctx, "hk-01", fingerprint, heartbeat); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT agent_source_flow_supported FROM nodes WHERE id='hk-01'`).Scan(&supported); err != nil || !supported {
		t.Fatalf("authenticated capability persisted=%v err=%v", supported, err)
	}
	heartbeat.SourceFlowSupported = false
	if err := s.ReportAgentHeartbeat(ctx, "hk-01", fingerprint, heartbeat); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT agent_source_flow_supported FROM nodes WHERE id='hk-01'`).Scan(&supported); err != nil || supported {
		t.Fatalf("latest heartbeat did not clear capability=%v err=%v", supported, err)
	}
}

func TestAgentHeartbeatCannotReduceJournalSequence(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	fingerprint := strings.Repeat("9", 64)
	if _, err := s.pool.Exec(ctx, "UPDATE nodes SET certificate_fingerprint=$1,agent_journal_sequence=5 WHERE id='hk-01'", fingerprint); err != nil {
		t.Fatal(err)
	}
	h := validAgentHeartbeat(time.Now().UTC())
	h.JournalSequence = 4
	if err := s.ReportAgentHeartbeat(ctx, "hk-01", fingerprint, h); !errors.Is(err, ErrConflict) {
		t.Fatalf("journal sequence rollback accepted: %v", err)
	}
	var sequence int64
	if err := s.pool.QueryRow(ctx, "SELECT agent_journal_sequence FROM nodes WHERE id='hk-01'").Scan(&sequence); err != nil || sequence != 5 {
		t.Fatalf("journal high-water changed to %d: %v", sequence, err)
	}
}

func TestManualCredentialReplacementClearsPriorHeartbeatFacts(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	old := strings.Repeat("1", 64)
	bootID := newID()
	if _, err := s.pool.Exec(ctx, "UPDATE nodes SET certificate_fingerprint=$1,agent_kernel_boot_id=$2,agent_journal_sequence=9 WHERE id='hk-01'", old, bootID); err != nil {
		t.Fatal(err)
	}
	h := validAgentHeartbeat(time.Now().UTC())
	h.RotationState = "error"
	h.RotationErrorCode = "renew-unavailable"
	h.KernelBootID = bootID
	h.JournalSequence = 9
	if err := s.ReportAgentHeartbeat(ctx, "hk-01", old, h); err != nil {
		t.Fatal(err)
	}
	if err := s.EnrollNode(ctx, "hk-01", strings.Repeat("2", 64)); err != nil {
		t.Fatal(err)
	}
	var observations int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM node_resource_latest WHERE node_id='hk-01'`).Scan(&observations); err != nil || observations != 0 {
		t.Fatalf("old identity resource observation survived replacement: %d %v", observations, err)
	}
	var heartbeat, expiry, errorSince *time.Time
	var version, mode, rotation, code string
	var persistedBootID *string
	var journalSequence int64
	if err := s.pool.QueryRow(ctx, `SELECT agent_heartbeat_at,agent_certificate_not_after,agent_rotation_error_since,agent_version,agent_mode,agent_rotation_state,agent_rotation_error_code,agent_kernel_boot_id::text,agent_journal_sequence FROM nodes WHERE id='hk-01'`).Scan(&heartbeat, &expiry, &errorSince, &version, &mode, &rotation, &code, &persistedBootID, &journalSequence); err != nil {
		t.Fatal(err)
	}
	if heartbeat != nil || expiry != nil || errorSince != nil || version != "" || mode != "" || rotation != "idle" || code != "" || persistedBootID != nil || journalSequence != 0 {
		t.Fatalf("old identity facts survived replacement: heartbeat=%v expiry=%v errorSince=%v version=%q mode=%q rotation=%q code=%q boot=%v sequence=%d", heartbeat, expiry, errorSince, version, mode, rotation, code, persistedBootID, journalSequence)
	}
}

func TestAgentHeartbeatRejectsUnboundedOrInconsistentValues(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	fingerprint := strings.Repeat("c", 64)
	if _, err := s.pool.Exec(ctx, "UPDATE nodes SET certificate_fingerprint=$1 WHERE id='hk-01'", fingerprint); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	tests := []struct {
		name   string
		mutate func(*AgentHeartbeat)
	}{
		{"empty version", func(h *AgentHeartbeat) { h.Version = "" }},
		{"long version", func(h *AgentHeartbeat) { h.Version = strings.Repeat("v", 81) }},
		{"unsafe version", func(h *AgentHeartbeat) { h.Version = "agent\nsecret" }},
		{"unknown mode", func(h *AgentHeartbeat) { h.Mode = "root" }},
		{"zero start", func(h *AgentHeartbeat) { h.StartedAt = time.Time{} }},
		{"future start", func(h *AgentHeartbeat) { h.StartedAt = now.Add(2 * time.Minute) }},
		{"zero expiry", func(h *AgentHeartbeat) { h.CertificateNotAfter = time.Time{} }},
		{"expired certificate", func(h *AgentHeartbeat) { h.CertificateNotAfter = now.Add(-time.Minute) }},
		{"unknown rotation", func(h *AgentHeartbeat) { h.RotationState = "working" }},
		{"idle with error", func(h *AgentHeartbeat) { h.RotationErrorCode = "renew-unavailable" }},
		{"error without code", func(h *AgentHeartbeat) { h.RotationState = "error" }},
		{"unknown code", func(h *AgentHeartbeat) { h.RotationState = "error"; h.RotationErrorCode = "raw-database-error" }},
		{"zero resource time", func(h *AgentHeartbeat) { h.Resources.SampledAt = time.Time{} }},
		{"future resource time", func(h *AgentHeartbeat) { h.Resources.SampledAt = now.Add(2 * time.Minute) }},
		{"stale resource time", func(h *AgentHeartbeat) { h.Resources.SampledAt = now.Add(-8 * 24 * time.Hour) }},
		{"resource percent above range", func(h *AgentHeartbeat) { value := 101; h.Resources.CPUPercent = &value }},
		{"resource percent below range", func(h *AgentHeartbeat) { value := -1; h.Resources.MemoryPercent = &value }},
		{"unknown dataplane state", func(h *AgentHeartbeat) { h.Resources.DataplaneState = "healthy" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := validAgentHeartbeat(now)
			tc.mutate(&h)
			if err := s.ReportAgentHeartbeat(ctx, "hk-01", fingerprint, h); !errors.Is(err, ErrInvalid) {
				t.Fatalf("invalid heartbeat accepted: %v", err)
			}
		})
	}
}

func TestAgentHeartbeatPersistsLatestResourceObservationAndConditionDurations(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	fingerprint := strings.Repeat("8", 64)
	if _, err := s.pool.Exec(ctx, "UPDATE nodes SET certificate_fingerprint=$1 WHERE id='hk-01'", fingerprint); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	h := validAgentHeartbeat(now)
	h.Mode = "apply"
	cpu, memory, disk := 95, 50, 86
	h.Resources.CPUPercent = &cpu
	h.Resources.MemoryPercent = &memory
	h.Resources.DiskPercent = &disk
	h.Resources.ConntrackPercent = nil
	h.Resources.DataplaneState = "drifted"
	h.Resources.SampledAt = now.Add(-10 * time.Second)
	if err := s.ReportAgentHeartbeat(ctx, "hk-01", fingerprint, h); err != nil {
		t.Fatal(err)
	}
	var sampled, received time.Time
	var storedCPU, storedMemory, storedDisk int
	var storedConntrack *int
	var skew int
	var cpuSince, diskSince, driftSince *time.Time
	if err := s.pool.QueryRow(ctx, `SELECT sampled_at,received_at,cpu_percent,memory_percent,disk_percent,conntrack_percent,clock_skew_seconds,cpu_high_since,disk_high_since,dataplane_drift_since FROM node_resource_latest WHERE node_id='hk-01'`).Scan(&sampled, &received, &storedCPU, &storedMemory, &storedDisk, &storedConntrack, &skew, &cpuSince, &diskSince, &driftSince); err != nil {
		t.Fatal(err)
	}
	if !sampled.Equal(h.Resources.SampledAt) || received.Before(now) || storedCPU != 95 || storedMemory != 50 || storedDisk != 86 || storedConntrack != nil || skew < 9 || skew > 12 || cpuSince == nil || diskSince == nil || driftSince == nil {
		t.Fatalf("persisted resource observation mismatch: sampled=%v received=%v values=%d/%d/%d/%v skew=%d since=%v/%v/%v", sampled, received, storedCPU, storedMemory, storedDisk, storedConntrack, skew, cpuSince, diskSince, driftSince)
	}
	firstCPU, firstDisk, firstDrift := *cpuSince, *diskSince, *driftSince
	h.Resources.SampledAt = time.Now().UTC()
	if err := s.ReportAgentHeartbeat(ctx, "hk-01", fingerprint, h); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT cpu_high_since,disk_high_since,dataplane_drift_since FROM node_resource_latest WHERE node_id='hk-01'`).Scan(&cpuSince, &diskSince, &driftSince); err != nil || cpuSince == nil || diskSince == nil || driftSince == nil || !cpuSince.Equal(firstCPU) || !diskSince.Equal(firstDisk) || !driftSince.Equal(firstDrift) {
		t.Fatalf("continuous condition duration reset: %v %v %v %v", cpuSince, diskSince, driftSince, err)
	}
	cpu, disk = 10, 20
	h.Resources.CPUPercent = &cpu
	h.Resources.DiskPercent = &disk
	h.Resources.DataplaneState = "in-sync"
	h.Resources.SampledAt = time.Now().UTC()
	if err := s.ReportAgentHeartbeat(ctx, "hk-01", fingerprint, h); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT cpu_high_since,disk_high_since,dataplane_drift_since FROM node_resource_latest WHERE node_id='hk-01'`).Scan(&cpuSince, &diskSince, &driftSince); err != nil || cpuSince != nil || diskSince != nil || driftSince != nil {
		t.Fatalf("recovered conditions retained duration: %v %v %v %v", cpuSince, diskSince, driftSince, err)
	}
}

func TestAgentHeartbeatErrorSinceIsStableAndClears(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	fingerprint := strings.Repeat("d", 64)
	if _, err := s.pool.Exec(ctx, "UPDATE nodes SET certificate_fingerprint=$1 WHERE id='hk-01'", fingerprint); err != nil {
		t.Fatal(err)
	}
	h := validAgentHeartbeat(time.Now().UTC())
	h.RotationState = "error"
	h.RotationErrorCode = "renew-unavailable"
	if err := s.ReportAgentHeartbeat(ctx, "hk-01", fingerprint, h); err != nil {
		t.Fatal(err)
	}
	var first time.Time
	if err := s.pool.QueryRow(ctx, "SELECT agent_rotation_error_since FROM nodes WHERE id='hk-01'").Scan(&first); err != nil {
		t.Fatal(err)
	}
	if err := s.ReportAgentHeartbeat(ctx, "hk-01", fingerprint, h); err != nil {
		t.Fatal(err)
	}
	var same time.Time
	if err := s.pool.QueryRow(ctx, "SELECT agent_rotation_error_since FROM nodes WHERE id='hk-01'").Scan(&same); err != nil || !same.Equal(first) {
		t.Fatalf("same error reset duration: %v %v", same, err)
	}
	h.RotationErrorCode = "activate-unavailable"
	if err := s.ReportAgentHeartbeat(ctx, "hk-01", fingerprint, h); err != nil {
		t.Fatal(err)
	}
	var changed time.Time
	if err := s.pool.QueryRow(ctx, "SELECT agent_rotation_error_since FROM nodes WHERE id='hk-01'").Scan(&changed); err != nil || changed.Before(same) {
		t.Fatalf("changed error did not restart duration: %v %v", changed, err)
	}
	h.RotationState = "idle"
	h.RotationErrorCode = ""
	if err := s.ReportAgentHeartbeat(ctx, "hk-01", fingerprint, h); err != nil {
		t.Fatal(err)
	}
	var cleared *time.Time
	if err := s.pool.QueryRow(ctx, "SELECT agent_rotation_error_since FROM nodes WHERE id='hk-01'").Scan(&cleared); err != nil || cleared != nil {
		t.Fatalf("recovered heartbeat retained error duration: %v %v", cleared, err)
	}
}

func TestAdminNodesExposeAgentHeartbeatWithoutCredentialMaterial(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	fingerprint := strings.Repeat("e", 64)
	if _, err := s.pool.Exec(ctx, "UPDATE nodes SET certificate_fingerprint=$1 WHERE id='hk-01'", fingerprint); err != nil {
		t.Fatal(err)
	}
	h := validAgentHeartbeat(time.Now().UTC())
	if err := s.ReportAgentHeartbeat(ctx, "hk-01", fingerprint, h); err != nil {
		t.Fatal(err)
	}
	nodes, err := s.AdminNodes(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	var found ManagedNode
	for _, node := range nodes {
		if node.ID == "hk-01" {
			found = node
		}
	}
	if found.ContactStatus != "online" || found.AgentMode != "validation" || found.AgentStartedAt == nil || found.CertificateNotAfter == nil || found.RotationState != "idle" {
		t.Fatalf("admin heartbeat facts: %+v", found)
	}
	if found.ResourceStatus != "normal" || found.ResourceObservedAt == nil || found.CPUPercent == nil || *found.CPUPercent != 20 || found.DataplaneState != "not-applicable" || found.ClockSkewSeconds == nil {
		t.Fatalf("admin resource facts: %+v", found)
	}
	raw, err := json.Marshal(found)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"BEGIN CERTIFICATE", "BEGIN PRIVATE KEY", `\"csr\"`, `\"certificate\"`, "privateKey"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("admin node leaked credential material %q: %s", secret, raw)
		}
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("schema v22 rerun: %v", err)
	}
	var version int
	var rotationsTable, upgradesTable, retirementsTable, attestationsTable, resourcesTable bool
	if err := s.pool.QueryRow(ctx, "SELECT max(version),(to_regclass('node_certificate_rotations') IS NOT NULL),(to_regclass('node_agent_upgrades') IS NOT NULL),(to_regclass('endpoint_retirements') IS NOT NULL),(to_regclass('node_drain_attestations') IS NOT NULL),(to_regclass('node_resource_latest') IS NOT NULL) FROM schema_migrations").Scan(&version, &rotationsTable, &upgradesTable, &retirementsTable, &attestationsTable, &resourcesTable); err != nil || version != SchemaVersion || !rotationsTable || !upgradesTable || !retirementsTable || !attestationsTable || !resourcesTable {
		t.Fatalf("schema version/tables: version=%d rotations=%v upgrades=%v retirements=%v attestations=%v resources=%v err=%v", version, rotationsTable, upgradesTable, retirementsTable, attestationsTable, resourcesTable, err)
	}
}

func TestPortRangeCapabilityUsesLatestAuthenticatedHeartbeat(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	fingerprint := strings.Repeat("d", 64)
	if _, err := s.pool.Exec(ctx, "UPDATE nodes SET certificate_fingerprint=$1 WHERE id='hk-01'", fingerprint); err != nil {
		t.Fatal(err)
	}
	heartbeat := validAgentHeartbeat(time.Now().UTC())
	heartbeat.PortRangeSupported = true
	if err := s.ReportAgentHeartbeat(ctx, "hk-01", fingerprint, heartbeat); err != nil {
		t.Fatal(err)
	}
	var supported bool
	if err := s.pool.QueryRow(ctx, "SELECT agent_port_range_supported FROM nodes WHERE id='hk-01'").Scan(&supported); err != nil || !supported {
		t.Fatalf("range capability supported=%v err=%v", supported, err)
	}
	heartbeat.PortRangeSupported = false
	if err := s.ReportAgentHeartbeat(ctx, "hk-01", fingerprint, heartbeat); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, "SELECT agent_port_range_supported FROM nodes WHERE id='hk-01'").Scan(&supported); err != nil || supported {
		t.Fatalf("latest heartbeat did not clear range capability=%v err=%v", supported, err)
	}
}

func TestApplyModeTransitionRedeliversCurrentValidatedGenerationOnce(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	fingerprint := strings.Repeat("f", 64)
	if _, err := s.pool.Exec(ctx, `UPDATE nodes SET certificate_fingerprint=$1,agent_mode='validation',validated_generation=desired_generation,applied_generation=0 WHERE id='hk-01'`, fingerprint); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE config_outbox SET state='validated' WHERE node_id='hk-01' AND generation=(SELECT desired_generation FROM nodes WHERE id='hk-01')`); err != nil {
		t.Fatal(err)
	}
	var beforeGeneration int64
	var beforeJobs int
	if err := s.pool.QueryRow(ctx, `SELECT desired_generation FROM nodes WHERE id='hk-01'`).Scan(&beforeGeneration); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM config_outbox WHERE node_id='hk-01'`).Scan(&beforeJobs); err != nil {
		t.Fatal(err)
	}
	h := validAgentHeartbeat(time.Now().UTC())
	h.Mode = "apply"
	h.Resources.DataplaneState = "in-sync"
	if err := s.ReportAgentHeartbeat(ctx, "hk-01", fingerprint, h); err != nil {
		t.Fatal(err)
	}
	var generation int64
	var state string
	if err := s.pool.QueryRow(ctx, `SELECT n.desired_generation,o.state FROM nodes n JOIN config_outbox o ON o.node_id=n.id AND o.generation=n.desired_generation WHERE n.id='hk-01'`).Scan(&generation, &state); err != nil {
		t.Fatal(err)
	}
	if generation != beforeGeneration+1 || state != "queued" {
		t.Fatalf("apply transition generation/state = %d/%q, want %d/queued", generation, state, beforeGeneration+1)
	}
	if err := s.ReportAgentHeartbeat(ctx, "hk-01", fingerprint, h); err != nil {
		t.Fatal(err)
	}
	var afterJobs int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM config_outbox WHERE node_id='hk-01'`).Scan(&afterJobs); err != nil {
		t.Fatal(err)
	}
	if afterJobs != beforeJobs+1 {
		t.Fatalf("repeated apply heartbeat created %d jobs, want %d", afterJobs, beforeJobs+1)
	}
}

func TestApplyModeTransitionDoesNotRedeliverNonCurrentValidatedWork(t *testing.T) {
	tests := []struct {
		name        string
		mode        string
		outboxState string
	}{
		{name: "validation heartbeat", mode: "validation", outboxState: "validated"},
		{name: "failed current generation", mode: "apply", outboxState: "failed"},
		{name: "already applied", mode: "apply", outboxState: "applied"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			fingerprint := strings.Repeat("e", 64)
			var desired int64
			if err := s.pool.QueryRow(ctx, `SELECT desired_generation FROM nodes WHERE id='hk-01'`).Scan(&desired); err != nil {
				t.Fatal(err)
			}
			applied := int64(0)
			if tc.outboxState == "applied" {
				applied = desired
			}
			if _, err := s.pool.Exec(ctx, `UPDATE nodes SET certificate_fingerprint=$1,agent_mode='validation',validated_generation=$2,applied_generation=$3 WHERE id='hk-01'`, fingerprint, desired, applied); err != nil {
				t.Fatal(err)
			}
			if _, err := s.pool.Exec(ctx, `UPDATE config_outbox SET state=$2 WHERE node_id='hk-01' AND generation=$1`, desired, tc.outboxState); err != nil {
				t.Fatal(err)
			}
			var jobsBefore int
			if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM config_outbox WHERE node_id='hk-01'`).Scan(&jobsBefore); err != nil {
				t.Fatal(err)
			}
			h := validAgentHeartbeat(time.Now().UTC())
			h.Mode = tc.mode
			if tc.mode == "apply" {
				h.Resources.DataplaneState = "in-sync"
			}
			if err := s.ReportAgentHeartbeat(ctx, "hk-01", fingerprint, h); err != nil {
				t.Fatal(err)
			}
			var jobsAfter int
			var generationAfter int64
			if err := s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM config_outbox WHERE node_id='hk-01'),desired_generation FROM nodes WHERE id='hk-01'`).Scan(&jobsAfter, &generationAfter); err != nil {
				t.Fatal(err)
			}
			if jobsAfter != jobsBefore || generationAfter != desired {
				t.Fatalf("unexpected redelivery jobs/generation=%d/%d want %d/%d", jobsAfter, generationAfter, jobsBefore, desired)
			}
		})
	}
}

func TestLateValidationAfterApplyHeartbeatRedeliversExactlyOnce(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	fingerprint := strings.Repeat("7", 64)
	if _, err := s.pool.Exec(ctx, `UPDATE nodes SET certificate_fingerprint=$1,agent_mode='validation' WHERE id='hk-01'`, fingerprint); err != nil {
		t.Fatal(err)
	}
	job, err := s.ClaimConfig(ctx, "hk-01", "transition-race", fingerprint)
	if err != nil || job == nil {
		t.Fatalf("claim: job=%+v err=%v", job, err)
	}
	h := validAgentHeartbeat(time.Now().UTC())
	h.Mode = "apply"
	h.Resources.DataplaneState = "in-sync"
	if err = s.ReportAgentHeartbeat(ctx, "hk-01", fingerprint, h); err != nil {
		t.Fatal(err)
	}
	if err = s.ReportConfig(ctx, "hk-01", job.ID, ConfigReport{LeaseToken: job.LeaseToken, Checksum: job.Checksum, Outcome: "validated"}, fingerprint); err != nil {
		t.Fatal(err)
	}
	var desired, validated int64
	var jobs int
	if err = s.pool.QueryRow(ctx, `SELECT desired_generation,validated_generation,(SELECT count(*) FROM config_outbox WHERE node_id='hk-01') FROM nodes WHERE id='hk-01'`).Scan(&desired, &validated, &jobs); err != nil {
		t.Fatal(err)
	}
	if desired != job.Generation+1 || validated != job.Generation {
		t.Fatalf("late validation generations desired/validated=%d/%d want %d/%d", desired, validated, job.Generation+1, job.Generation)
	}
	if err = s.ReportAgentHeartbeat(ctx, "hk-01", fingerprint, h); err != nil {
		t.Fatal(err)
	}
	var after int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM config_outbox WHERE node_id='hk-01'`).Scan(&after); err != nil || after != jobs {
		t.Fatalf("repeated heartbeat redelivered late validation: before=%d after=%d err=%v", jobs, after, err)
	}
}
