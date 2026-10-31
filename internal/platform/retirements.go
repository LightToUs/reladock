package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"time"

	"github.com/jackc/pgx/v5"
)

const RetirementLease = 2 * time.Minute

type RetirementClaim struct {
	ID                   string    `json:"id"`
	AllocationID         string    `json:"allocationId"`
	NodeID               string    `json:"nodeId"`
	RuleID               string    `json:"ruleId"`
	Address              string    `json:"address"`
	Protocol             string    `json:"protocol"`
	Port                 int       `json:"port"`
	QuarantineGeneration int64     `json:"quarantineGeneration"`
	LeaseToken           string    `json:"leaseToken"`
	LeaseUntil           time.Time `json:"leaseUntil"`
}

type RetirementProof struct {
	SchemaVersion        int       `json:"schemaVersion"`
	TaskID               string    `json:"taskId"`
	AllocationID         string    `json:"allocationId"`
	NodeID               string    `json:"nodeId"`
	Address              string    `json:"address"`
	Protocol             string    `json:"protocol"`
	Port                 int       `json:"port"`
	QuarantineGeneration int64     `json:"quarantineGeneration"`
	GuardBeforeDigest    string    `json:"guardBeforeDigest"`
	GuardAfterDigest     string    `json:"guardAfterDigest"`
	ConntrackVersion     string    `json:"conntrackVersion"`
	FilterVersion        string    `json:"filterVersion"`
	DeletedEntries       int       `json:"deletedEntries"`
	ZeroMatches          bool      `json:"zeroMatches"`
	ForwardGuardRemoved  bool      `json:"forwardGuardRemoved"`
	KernelBootID         string    `json:"kernelBootId"`
	JournalSequence      int64     `json:"journalSequence"`
	GoodDigest           string    `json:"goodDigest"`
	ObservedAt           time.Time `json:"observedAt"`
}

type RetirementReport struct {
	LeaseToken string          `json:"leaseToken"`
	Proof      RetirementProof `json:"proof"`
}

func validRetirementProofValue(proof RetirementProof) bool {
	ip := net.ParseIP(proof.Address)
	return proof.SchemaVersion == 1 && upgradeIDPattern.MatchString(proof.TaskID) && upgradeIDPattern.MatchString(proof.AllocationID) && nodeIDPattern.MatchString(proof.NodeID) && ip != nil && ip.To4() != nil && ip.IsGlobalUnicast() && proof.Address == ip.String() && (proof.Protocol == "tcp" || proof.Protocol == "udp") && proof.Port >= 20000 && proof.Port <= 60000 && proof.QuarantineGeneration >= 1 && fingerprintPattern.MatchString(proof.GuardBeforeDigest) && fingerprintPattern.MatchString(proof.GuardAfterDigest) && len(proof.ConntrackVersion) >= 1 && len(proof.ConntrackVersion) <= 80 && proof.FilterVersion == "reladock-conntrack-v1" && proof.DeletedEntries >= 0 && proof.ZeroMatches && proof.ForwardGuardRemoved && upgradeIDPattern.MatchString(proof.KernelBootID) && proof.JournalSequence >= 1 && fingerprintPattern.MatchString(proof.GoodDigest) && !proof.ObservedAt.IsZero()
}

func retirementProofBytes(proof RetirementProof) ([]byte, string, error) {
	if !validRetirementProofValue(proof) {
		return nil, "", ErrInvalid
	}
	proof.Address = net.ParseIP(proof.Address).String()
	proof.ObservedAt = proof.ObservedAt.UTC()
	raw, err := json.Marshal(proof)
	if err != nil || len(raw) > 64*1024 {
		return nil, "", ErrInvalid
	}
	digest := sha256.Sum256(raw)
	return raw, hex.EncodeToString(digest[:]), nil
}

func (s *Store) ClaimRetirement(ctx context.Context, nodeID, fingerprint string) (*RetirementClaim, error) {
	if !nodeIDPattern.MatchString(nodeID) || !fingerprintPattern.MatchString(fingerprint) {
		return nil, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if err = lockNode(ctx, tx, nodeID); err != nil {
		return nil, err
	}
	if err = checkNodeCredential(ctx, tx, nodeID, []string{fingerprint}); err != nil {
		return nil, err
	}
	var busy bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM config_outbox WHERE node_id=$1 AND state='applying') OR EXISTS(SELECT 1 FROM node_agent_upgrades WHERE node_id=$1 AND state=ANY($2)) OR EXISTS(SELECT 1 FROM endpoint_retirements WHERE node_id=$1 AND state='purging')`, nodeID, activeUpgradeStates).Scan(&busy); err != nil {
		return nil, err
	}
	if busy {
		return nil, tx.Commit(ctx)
	}
	if _, err = tx.Exec(ctx, `UPDATE endpoint_retirements SET state='failed',error_code='internal-error',reported_at=now() WHERE node_id=$1 AND state='claimed' AND lease_expires_at<=now() AND attempts>=5`, nodeID); err != nil {
		return nil, err
	}
	var claim RetirementClaim
	var attempts int
	err = tx.QueryRow(ctx, `SELECT id::text,allocation_id::text,node_id,rule_id::text,host(listen_address),protocol,listen_port,quarantine_generation,attempts FROM endpoint_retirements WHERE node_id=$1 AND attempts<5 AND ((state IN ('queued','failed') AND next_attempt_at<=now()) OR (state='claimed' AND lease_expires_at<=now())) ORDER BY created_at,id LIMIT 1 FOR UPDATE`, nodeID).Scan(&claim.ID, &claim.AllocationID, &claim.NodeID, &claim.RuleID, &claim.Address, &claim.Protocol, &claim.Port, &claim.QuarantineGeneration, &attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, tx.Commit(ctx)
	}
	if err != nil {
		return nil, err
	}
	token, digest, err := newUpgradeLease()
	if err != nil {
		return nil, err
	}
	claim.LeaseToken = token
	claim.LeaseUntil = time.Now().UTC().Add(RetirementLease)
	if _, err = tx.Exec(ctx, `UPDATE endpoint_retirements SET state='claimed',attempts=attempts+1,lease_token_hash=$2,lease_expires_at=$3,claimed_at=COALESCE(claimed_at,now()),error_code='' WHERE id=$1`, claim.ID, digest, claim.LeaseUntil); err != nil {
		return nil, err
	}
	if err = platformLog(ctx, tx, nil, "node.retirement-claimed", nodeID, map[string]any{"taskId": claim.ID, "allocationId": claim.AllocationID}); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &claim, nil
}

func validRetirementError(code string) bool {
	switch code {
	case "tool-missing", "tool-version", "permission-denied", "guard-missing", "guard-mismatch", "state-mismatch", "output-invalid", "query-nonzero", "io-failed", "internal-error":
		return true
	default:
		return false
	}
}

func (s *Store) ReconcileRetirements(ctx context.Context, nodeID string) error {
	if !nodeIDPattern.MatchString(nodeID) {
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
	_, err = tx.Exec(ctx, `UPDATE endpoint_retirements r SET state='queued',error_code='',next_attempt_at=now() WHERE r.node_id=$1 AND r.state='waiting-removal' AND EXISTS(SELECT 1 FROM config_outbox o WHERE o.node_id=r.node_id AND o.generation>=r.quarantine_generation AND o.state='applied') AND EXISTS(SELECT 1 FROM nodes n WHERE n.id=r.node_id AND n.applied_generation>=r.quarantine_generation)`, nodeID)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) ReportRetirementFailure(ctx context.Context, nodeID, fingerprint, taskID, leaseToken, errorCode string) error {
	if !nodeIDPattern.MatchString(nodeID) || !fingerprintPattern.MatchString(fingerprint) || !upgradeIDPattern.MatchString(taskID) || len(leaseToken) != 64 || !validRetirementError(errorCode) {
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
	var state, digest, storedError string
	var attempts int
	err = tx.QueryRow(ctx, `SELECT state,COALESCE(lease_token_hash,''),error_code,attempts FROM endpoint_retirements WHERE id=$1 AND node_id=$2 FOR UPDATE`, taskID, nodeID).Scan(&state, &digest, &storedError, &attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if digest != bootstrapHash([]byte(leaseToken)) {
		return ErrUnauthorized
	}
	if (state == "failed" || state == "purging") && storedError == errorCode {
		return tx.Commit(ctx)
	}
	if state != "claimed" && state != "purging" {
		return ErrConflict
	}
	if state == "claimed" {
		delay := time.Duration(1<<attempts) * time.Second
		_, err = tx.Exec(ctx, `UPDATE endpoint_retirements SET state='failed',error_code=$2,reported_at=now(),next_attempt_at=$3,lease_token_hash=NULL,lease_expires_at=NULL WHERE id=$1`, taskID, errorCode, time.Now().UTC().Add(delay))
	} else {
		_, err = tx.Exec(ctx, `UPDATE endpoint_retirements SET error_code=$2,reported_at=now() WHERE id=$1`, taskID, errorCode)
	}
	if err != nil {
		return err
	}
	if err = platformLog(ctx, tx, nil, "node.retirement-failed", nodeID, map[string]any{"taskId": taskID, "state": state, "errorCode": errorCode}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) BeginRetirementPurge(ctx context.Context, nodeID, fingerprint, taskID, leaseToken string) error {
	if !nodeIDPattern.MatchString(nodeID) || !fingerprintPattern.MatchString(fingerprint) || !upgradeIDPattern.MatchString(taskID) || len(leaseToken) != 64 {
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
	var state, digest string
	var expires *time.Time
	err = tx.QueryRow(ctx, `SELECT state,COALESCE(lease_token_hash,''),lease_expires_at FROM endpoint_retirements WHERE id=$1 AND node_id=$2 FOR UPDATE`, taskID, nodeID).Scan(&state, &digest, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if digest != bootstrapHash([]byte(leaseToken)) {
		return ErrUnauthorized
	}
	if state == "purging" {
		return tx.Commit(ctx)
	}
	if state != "claimed" || expires == nil || !expires.After(time.Now().UTC()) {
		return ErrConflict
	}
	var busy bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM config_outbox WHERE node_id=$1 AND state='applying') OR EXISTS(SELECT 1 FROM node_agent_upgrades WHERE node_id=$1 AND state=ANY($2))`, nodeID, activeUpgradeStates).Scan(&busy); err != nil {
		return err
	}
	if busy {
		return ErrConflict
	}
	if _, err = tx.Exec(ctx, "UPDATE endpoint_retirements SET state='purging',purging_at=now() WHERE id=$1", taskID); err != nil {
		return err
	}
	if err = platformLog(ctx, tx, nil, "node.retirement-purging", nodeID, map[string]any{"taskId": taskID}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) ReportRetirement(ctx context.Context, nodeID, fingerprint, taskID string, report RetirementReport) error {
	if !nodeIDPattern.MatchString(nodeID) || !fingerprintPattern.MatchString(fingerprint) || !upgradeIDPattern.MatchString(taskID) || len(report.LeaseToken) != 64 {
		return ErrInvalid
	}
	raw, proofDigest, err := retirementProofBytes(report.Proof)
	if err != nil {
		return err
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
	var state, leaseDigest, allocationID, ruleID, address, protocol, storedProofDigest string
	var port int
	var generation, applied int64
	err = tx.QueryRow(ctx, `SELECT r.state,COALESCE(r.lease_token_hash,''),r.allocation_id::text,r.rule_id::text,host(r.listen_address),r.protocol,r.listen_port,r.quarantine_generation,COALESCE(r.proof_digest,''),n.applied_generation FROM endpoint_retirements r JOIN nodes n ON n.id=r.node_id WHERE r.id=$1 AND r.node_id=$2 FOR UPDATE OF r`, taskID, nodeID).Scan(&state, &leaseDigest, &allocationID, &ruleID, &address, &protocol, &port, &generation, &storedProofDigest, &applied)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if leaseDigest != bootstrapHash([]byte(report.LeaseToken)) {
		return ErrUnauthorized
	}
	if state == "released" {
		if storedProofDigest != proofDigest {
			return ErrConflict
		}
		return tx.Commit(ctx)
	}
	if state != "purging" || report.Proof.TaskID != taskID || report.Proof.AllocationID != allocationID || report.Proof.NodeID != nodeID || report.Proof.Address != address || report.Proof.Protocol != protocol || report.Proof.Port != port || report.Proof.QuarantineGeneration != generation || applied < generation {
		return ErrConflict
	}
	var deleted bool
	var appliedSnapshot bool
	if err = tx.QueryRow(ctx, "SELECT status='deleted' FROM relay_rules WHERE id=$1 AND node_id=$2", ruleID, nodeID).Scan(&deleted); err != nil {
		return err
	}
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM config_outbox WHERE node_id=$1 AND generation>=$2 AND state='applied')", nodeID, generation).Scan(&appliedSnapshot); err != nil {
		return err
	}
	if !deleted || !appliedSnapshot {
		return ErrConflict
	}
	result, err := tx.Exec(ctx, "DELETE FROM endpoint_allocations WHERE allocation_id=$1 AND rule_id=$2", allocationID, ruleID)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrConflict
	}
	if _, err = tx.Exec(ctx, `UPDATE endpoint_retirements SET state='released',proof=$2::jsonb,proof_digest=$3,reported_at=now(),released_at=now(),error_code='' WHERE id=$1`, taskID, raw, proofDigest); err != nil {
		return err
	}
	if err = platformLog(ctx, tx, nil, "node.retirement-released", nodeID, map[string]any{"taskId": taskID, "allocationId": allocationID, "proofDigest": proofDigest}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
