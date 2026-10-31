package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/netip"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

const DrainAttestationFreshness = 2 * time.Minute

type DrainQuarantine struct {
	RetirementID string `json:"retirementId"`
	AllocationID string `json:"allocationId"`
	Address      string `json:"address"`
	Protocol     string `json:"protocol"`
	Port         int    `json:"port"`
	GuardDigest  string `json:"guardDigest"`
}

type DrainAttestation struct {
	SchemaVersion          int       `json:"schemaVersion"`
	NodeID                 string    `json:"nodeId"`
	Generation             int64     `json:"generation"`
	CertificateFingerprint string    `json:"certificateFingerprint"`
	KernelBootID           string    `json:"kernelBootId"`
	AgentStartedAt         time.Time `json:"agentStartedAt"`
	AgentVersion           string    `json:"agentVersion"`
	JournalSequence        int64     `json:"journalSequence"`
	GoodDigest             string    `json:"goodDigest"`
	QuarantineCount        int       `json:"quarantineCount"`
	QuarantineDigest       string    `json:"quarantineDigest"`
	NoPendingDataplane     bool      `json:"noPendingDataplane"`
	NoRetirementJournal    bool      `json:"noRetirementJournal"`
	UsageWALDrained        bool      `json:"usageWalDrained"`
	UpdaterStable          bool      `json:"updaterStable"`
	ObservedAt             time.Time `json:"observedAt"`
}

func DrainGuardDigest(value DrainQuarantine) string {
	value.GuardDigest = ""
	raw, _ := json.Marshal(struct {
		Schema int             `json:"schema"`
		Guard  DrainQuarantine `json:"guard"`
	}{1, value})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func normalizeDrainQuarantines(values []DrainQuarantine) ([]DrainQuarantine, error) {
	result := make([]DrainQuarantine, len(values))
	copy(result, values)
	seenRetirement, seenAllocation := map[string]bool{}, map[string]bool{}
	for i := range result {
		value := &result[i]
		address, err := netip.ParseAddr(value.Address)
		if err != nil || !address.Is4() || !address.IsGlobalUnicast() || address.IsLoopback() || address.String() != value.Address || !upgradeIDPattern.MatchString(value.RetirementID) || !upgradeIDPattern.MatchString(value.AllocationID) || seenRetirement[value.RetirementID] || seenAllocation[value.AllocationID] || (value.Protocol != "tcp" && value.Protocol != "udp") || value.Port < 20000 || value.Port > 60000 || !fingerprintPattern.MatchString(value.GuardDigest) || value.GuardDigest != DrainGuardDigest(*value) {
			return nil, ErrInvalid
		}
		seenRetirement[value.RetirementID], seenAllocation[value.AllocationID] = true, true
	}
	sort.Slice(result, func(i, j int) bool { return result[i].RetirementID < result[j].RetirementID })
	return result, nil
}

func DrainQuarantineDigest(values []DrainQuarantine) (int, string, error) {
	normalized, err := normalizeDrainQuarantines(values)
	if err != nil {
		return 0, "", err
	}
	raw, err := json.Marshal(normalized)
	if err != nil {
		return 0, "", err
	}
	sum := sha256.Sum256(raw)
	return len(normalized), hex.EncodeToString(sum[:]), nil
}

func expectedDrainQuarantines(ctx context.Context, tx pgx.Tx, nodeID string) ([]DrainQuarantine, error) {
	rows, err := tx.Query(ctx, `SELECT id::text,allocation_id::text,host(listen_address),protocol,listen_port FROM endpoint_retirements WHERE node_id=$1 AND state='released' AND quarantine_consumed_at IS NULL ORDER BY id`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []DrainQuarantine{}
	for rows.Next() {
		var item DrainQuarantine
		if err = rows.Scan(&item.RetirementID, &item.AllocationID, &item.Address, &item.Protocol, &item.Port); err != nil {
			return nil, err
		}
		item.GuardDigest = DrainGuardDigest(item)
		result = append(result, item)
	}
	return result, rows.Err()
}

func validDrainAttestation(value DrainAttestation, now time.Time) bool {
	return value.SchemaVersion == 1 && nodeIDPattern.MatchString(value.NodeID) && value.Generation >= 0 && fingerprintPattern.MatchString(value.CertificateFingerprint) && upgradeIDPattern.MatchString(value.KernelBootID) && !value.AgentStartedAt.IsZero() && agentVersionPattern.MatchString(value.AgentVersion) && value.JournalSequence >= 1 && fingerprintPattern.MatchString(value.GoodDigest) && value.QuarantineCount >= 0 && fingerprintPattern.MatchString(value.QuarantineDigest) && value.NoPendingDataplane && value.NoRetirementJournal && value.UsageWALDrained && value.UpdaterStable && !value.ObservedAt.IsZero() && !value.ObservedAt.Before(now.Add(-DrainAttestationFreshness)) && !value.ObservedAt.After(now.Add(30*time.Second))
}

func (s *Store) StartNodeDraining(ctx context.Context, who Identity, nodeID string, revision int64) error {
	if !nodeIDPattern.MatchString(nodeID) || revision < 1 {
		return ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = platformLock(ctx, tx); err != nil {
		return err
	}
	if err = authorizePlatform(ctx, tx, who); err != nil {
		return err
	}
	if err = lockNode(ctx, tx, nodeID); err != nil {
		return err
	}
	var current int64
	var status string
	if err = tx.QueryRow(ctx, `SELECT revision,status FROM nodes WHERE id=$1 FOR UPDATE`, nodeID).Scan(&current, &status); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if current != revision || status != "maintenance" {
		return ErrConflict
	}
	if _, err = tx.Exec(ctx, `UPDATE nodes SET status='draining',revision=revision+1 WHERE id=$1`, nodeID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE node_bootstrap SET revoked=true WHERE node_id=$1`, nodeID); err != nil {
		return err
	}
	if err = platformLog(ctx, tx, &who, "node.draining", nodeID, map[string]any{"revision": current + 1}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) ReportNodeDrainAttestation(ctx context.Context, nodeID, fingerprint string, value DrainAttestation) error {
	if !nodeIDPattern.MatchString(nodeID) || !fingerprintPattern.MatchString(fingerprint) {
		return ErrInvalid
	}
	value.AgentStartedAt = value.AgentStartedAt.UTC().Truncate(time.Microsecond)
	now := time.Now().UTC()
	if !validDrainAttestation(value, now) || value.NodeID != nodeID || value.CertificateFingerprint != fingerprint {
		return ErrInvalid
	}
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > 8*1024 {
		return ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = lockNode(ctx, tx, nodeID); err != nil {
		return err
	}
	if err = checkNodeCredential(ctx, tx, nodeID, []string{fingerprint}); err != nil {
		return err
	}
	var status, storedFingerprint, bootID, version string
	var generation, nodeRevision int64
	var storedSequence int64
	var started *time.Time
	if err = tx.QueryRow(ctx, `SELECT status,desired_generation,COALESCE(certificate_fingerprint,''),COALESCE(agent_kernel_boot_id::text,''),agent_started_at,agent_version,agent_journal_sequence,revision FROM nodes WHERE id=$1 FOR UPDATE`, nodeID).Scan(&status, &generation, &storedFingerprint, &bootID, &started, &version, &storedSequence, &nodeRevision); err != nil {
		return err
	}
	if status != "draining" || generation != value.Generation || storedFingerprint != fingerprint || bootID != value.KernelBootID || started == nil || !started.Equal(value.AgentStartedAt) || version != value.AgentVersion || value.JournalSequence < storedSequence {
		return ErrConflict
	}
	expected, err := expectedDrainQuarantines(ctx, tx, nodeID)
	if err != nil {
		return err
	}
	expectedCount, expectedDigest, err := DrainQuarantineDigest(expected)
	if err != nil {
		return err
	}
	if value.QuarantineCount != expectedCount || value.QuarantineDigest != expectedDigest {
		return ErrConflict
	}
	reasons, err := s.nodeRetirementReasons(ctx, tx, nodeID)
	if err != nil {
		return err
	}
	if len(reasons) > 1 || len(reasons) == 1 && reasons[0] != "attestation" {
		return ErrConflict
	}
	if _, err = tx.Exec(ctx, `UPDATE node_drain_attestations SET consumed_at=COALESCE(consumed_at,clock_timestamp()) WHERE node_id=$1 AND consumed_at IS NULL`, nodeID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO node_drain_attestations(id,node_id,generation,certificate_fingerprint,kernel_boot_id,agent_started_at,agent_version,journal_sequence,good_digest,quarantine_digest,observed_at,evidence,node_revision) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, newID(), nodeID, value.Generation, fingerprint, value.KernelBootID, value.AgentStartedAt, value.AgentVersion, value.JournalSequence, value.GoodDigest, value.QuarantineDigest, value.ObservedAt, raw, nodeRevision); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE nodes SET agent_journal_sequence=$2 WHERE id=$1`, nodeID, value.JournalSequence); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) NodeDrainRequested(ctx context.Context, nodeID string) (bool, error) {
	if !nodeIDPattern.MatchString(nodeID) {
		return false, ErrInvalid
	}
	var status string
	if err := s.pool.QueryRow(ctx, "SELECT status FROM nodes WHERE id=$1", nodeID).Scan(&status); errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	} else if err != nil {
		return false, err
	}
	return status == "draining", nil
}

func (s *Store) nodeRetirementReasons(ctx context.Context, tx pgx.Tx, nodeID string) ([]string, error) {
	reasons := []string{}
	var status, delivery, rotation string
	var desired, applied int64
	var rules, allocations, retirements, busyConfig, upgrades, rotations int
	var attestation bool
	err := tx.QueryRow(ctx, `SELECT n.status,n.desired_generation,n.applied_generation,COALESCE(o.state,''),n.agent_rotation_state,
 (SELECT count(*) FROM relay_rules r WHERE r.node_id=n.id AND r.status<>'deleted'),
 (SELECT count(*) FROM endpoint_allocations e WHERE e.node_id=n.id),
 (SELECT count(*) FROM endpoint_retirements e WHERE e.node_id=n.id AND e.state<>'released'),
 (SELECT count(*) FROM config_outbox c WHERE c.node_id=n.id AND c.state IN ('leased','applying')),
 (SELECT count(*) FROM node_agent_upgrades u WHERE u.node_id=n.id AND u.state=ANY($2)),
 (SELECT count(*) FROM node_certificate_rotations cr WHERE cr.node_id=n.id),
 EXISTS(SELECT 1 FROM node_drain_attestations a WHERE a.node_id=n.id AND a.consumed_at IS NULL AND a.node_revision=n.revision AND a.generation=n.desired_generation AND a.certificate_fingerprint=n.certificate_fingerprint AND a.kernel_boot_id=n.agent_kernel_boot_id AND a.agent_started_at=n.agent_started_at AND a.agent_version=n.agent_version AND a.journal_sequence=n.agent_journal_sequence AND a.observed_at>=clock_timestamp()-$3*interval '1 second')
 FROM nodes n LEFT JOIN config_outbox o ON o.node_id=n.id AND o.generation=n.desired_generation WHERE n.id=$1`, nodeID, activeUpgradeStates, DrainAttestationFreshness.Seconds()).Scan(&status, &desired, &applied, &delivery, &rotation, &rules, &allocations, &retirements, &busyConfig, &upgrades, &rotations, &attestation)
	if err != nil {
		return nil, err
	}
	if status != "draining" {
		reasons = append(reasons, "status")
	}
	if rules != 0 {
		reasons = append(reasons, "rules")
	}
	if allocations != 0 {
		reasons = append(reasons, "allocations")
	}
	if retirements != 0 {
		reasons = append(reasons, "retirements")
	}
	if desired != applied || delivery != "applied" {
		reasons = append(reasons, "generation")
	}
	if busyConfig != 0 {
		reasons = append(reasons, "configuration-active")
	}
	if upgrades != 0 {
		reasons = append(reasons, "upgrade-active")
	}
	if rotation == "pending" || rotations != 0 {
		reasons = append(reasons, "rotation-pending")
	}
	if !attestation {
		reasons = append(reasons, "attestation")
	}
	return reasons, nil
}

func (s *Store) FinalizeNodeRetirement(ctx context.Context, who Identity, nodeID string, revision int64) error {
	if !nodeIDPattern.MatchString(nodeID) || revision < 1 {
		return ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = platformLock(ctx, tx); err != nil {
		return err
	}
	if err = authorizePlatform(ctx, tx, who); err != nil {
		return err
	}
	if err = lockNode(ctx, tx, nodeID); err != nil {
		return err
	}
	var current int64
	if err = tx.QueryRow(ctx, `SELECT revision FROM nodes WHERE id=$1 FOR UPDATE`, nodeID).Scan(&current); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if current != revision {
		return ErrConflict
	}
	reasons, err := s.nodeRetirementReasons(ctx, tx, nodeID)
	if err != nil {
		return err
	}
	if len(reasons) != 0 {
		return ErrConflict
	}
	var evidenceRaw []byte
	if err = tx.QueryRow(ctx, `SELECT evidence FROM node_drain_attestations WHERE node_id=$1 AND consumed_at IS NULL ORDER BY received_at DESC,id DESC LIMIT 1 FOR UPDATE`, nodeID).Scan(&evidenceRaw); err != nil {
		return ErrConflict
	}
	var evidence DrainAttestation
	if json.Unmarshal(evidenceRaw, &evidence) != nil {
		return ErrConflict
	}
	expected, expectedErr := expectedDrainQuarantines(ctx, tx, nodeID)
	if expectedErr != nil {
		return expectedErr
	}
	expectedCount, expectedDigest, digestErr := DrainQuarantineDigest(expected)
	if digestErr != nil {
		return digestErr
	}
	if evidence.QuarantineCount != expectedCount || evidence.QuarantineDigest != expectedDigest {
		return ErrConflict
	}
	if _, err = tx.Exec(ctx, `UPDATE node_drain_attestations SET consumed_at=clock_timestamp() WHERE node_id=$1 AND consumed_at IS NULL`, nodeID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE node_bootstrap SET revoked=true WHERE node_id=$1`, nodeID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM node_certificate_rotations WHERE node_id=$1`, nodeID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE nodes SET status='retired',certificate_fingerprint=NULL,agent_heartbeat_at=NULL,agent_certificate_not_after=NULL,agent_rotation_state='idle',agent_rotation_error_code='',agent_rotation_error_since=NULL,revision=revision+1 WHERE id=$1`, nodeID); err != nil {
		return err
	}
	if err = platformLog(ctx, tx, &who, "node.retired", nodeID, map[string]any{"revision": current + 1}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
