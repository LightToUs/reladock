package platform

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"reladock/internal/buildinfo"
)

const AgentUpgradeLease = 5 * time.Minute

var activeUpgradeStates = []string{"queued", "claimed", "installing", "verifying"}
var upgradeIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type ReleaseTarget struct {
	Version     string `json:"version"`
	AMD64SHA256 string `json:"amd64Sha256"`
	ARM64SHA256 string `json:"arm64Sha256"`
}

type AgentUpgrade struct {
	ID              string     `json:"id"`
	NodeID          string     `json:"nodeId"`
	TargetVersion   string     `json:"targetVersion"`
	PreviousVersion string     `json:"previousVersion"`
	State           string     `json:"state"`
	ErrorCode       string     `json:"errorCode"`
	RequestedAt     time.Time  `json:"requestedAt"`
	ClaimedAt       *time.Time `json:"claimedAt"`
	UpdatedAt       time.Time  `json:"updatedAt"`
	CompletedAt     *time.Time `json:"completedAt"`
}

type AgentUpgradeSummary struct {
	ID              string     `json:"id"`
	TargetVersion   string     `json:"targetVersion"`
	PreviousVersion string     `json:"previousVersion"`
	State           string     `json:"state"`
	ErrorCode       string     `json:"errorCode"`
	RequestedAt     time.Time  `json:"requestedAt"`
	UpdatedAt       time.Time  `json:"updatedAt"`
	CompletedAt     *time.Time `json:"completedAt"`
}

type UpgradeClaim struct {
	ID              string `json:"id"`
	TargetVersion   string `json:"targetVersion"`
	PreviousVersion string `json:"previousVersion"`
	SHA256          string `json:"sha256"`
	LeaseToken      string `json:"leaseToken"`
}

type UpgradeReport struct {
	ID         string `json:"id"`
	LeaseToken string `json:"leaseToken"`
	State      string `json:"state"`
	ErrorCode  string `json:"errorCode"`
}

type upgradeScanner interface{ Scan(...any) error }

func scanAgentUpgrade(row upgradeScanner) (AgentUpgrade, error) {
	var value AgentUpgrade
	err := row.Scan(&value.ID, &value.NodeID, &value.TargetVersion, &value.PreviousVersion, &value.State, &value.ErrorCode, &value.RequestedAt, &value.ClaimedAt, &value.UpdatedAt, &value.CompletedAt)
	return value, err
}

const agentUpgradeColumns = `id::text,node_id,target_version,previous_version,state,error_code,requested_at,claimed_at,updated_at,completed_at`

func validReleaseTarget(target ReleaseTarget) bool {
	_, _, _, ok := buildinfo.ParseProduction(target.Version)
	return ok && fingerprintPattern.MatchString(target.AMD64SHA256) && fingerprintPattern.MatchString(target.ARM64SHA256)
}

func validUpgradeError(code string) bool {
	switch code {
	case "download-unavailable", "archive-invalid", "checksum-mismatch", "install-failed", "service-failed", "health-timeout", "rollback-failed", "state-invalid":
		return true
	default:
		return false
	}
}

func (s *Store) RequestAgentUpgrade(ctx context.Context, who Identity, nodeID string, revision int64, target ReleaseTarget) (AgentUpgrade, error) {
	if !nodeIDPattern.MatchString(nodeID) || revision < 1 || !validReleaseTarget(target) {
		return AgentUpgrade{}, ErrInvalid
	}
	tx, err := s.adminTx(ctx, who)
	if err != nil {
		return AgentUpgrade{}, err
	}
	defer tx.Rollback(ctx)
	if err = lockNode(ctx, tx, nodeID); err != nil {
		return AgentUpgrade{}, err
	}

	existing, err := scanAgentUpgrade(tx.QueryRow(ctx, `SELECT `+agentUpgradeColumns+` FROM node_agent_upgrades WHERE node_id=$1 AND target_version=$2 AND state=ANY($3) FOR UPDATE`, nodeID, target.Version, activeUpgradeStates))
	if err == nil {
		return existing, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return AgentUpgrade{}, err
	}

	var currentRevision int64
	var status, currentVersion string
	var heartbeatAt *time.Time
	err = tx.QueryRow(ctx, `SELECT revision,status,agent_version,agent_heartbeat_at FROM nodes WHERE id=$1 FOR UPDATE`, nodeID).Scan(&currentRevision, &status, &currentVersion, &heartbeatAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentUpgrade{}, ErrNotFound
	}
	if err != nil {
		return AgentUpgrade{}, err
	}
	if currentRevision != revision || status == "maintenance" || heartbeatAt == nil || heartbeatAt.Before(time.Now().UTC().Add(-AgentHeartbeatStaleAfter)) || !buildinfo.CanUpgrade(currentVersion, target.Version) {
		return AgentUpgrade{}, ErrConflict
	}
	var active bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM node_agent_upgrades WHERE state=ANY($1))`, activeUpgradeStates).Scan(&active); err != nil {
		return AgentUpgrade{}, err
	}
	if active {
		return AgentUpgrade{}, ErrConflict
	}

	id := newID()
	value, err := scanAgentUpgrade(tx.QueryRow(ctx, `INSERT INTO node_agent_upgrades(id,node_id,target_version,amd64_sha256,arm64_sha256,previous_version,state) VALUES($1,$2,$3,$4,$5,$6,'queued') RETURNING `+agentUpgradeColumns, id, nodeID, target.Version, target.AMD64SHA256, target.ARM64SHA256, currentVersion))
	if err != nil {
		return AgentUpgrade{}, nodeError(err)
	}
	if _, err = tx.Exec(ctx, "UPDATE nodes SET revision=revision+1 WHERE id=$1", nodeID); err != nil {
		return AgentUpgrade{}, err
	}
	if err = platformLog(ctx, tx, &who, "node.agent-upgrade-request", nodeID, map[string]any{"taskId": id, "targetVersion": target.Version, "state": "queued"}); err != nil {
		return AgentUpgrade{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return AgentUpgrade{}, err
	}
	return value, nil
}

func (s *Store) CancelAgentUpgrade(ctx context.Context, who Identity, nodeID string, revision int64) (AgentUpgrade, error) {
	if !nodeIDPattern.MatchString(nodeID) || revision < 1 {
		return AgentUpgrade{}, ErrInvalid
	}
	tx, err := s.adminTx(ctx, who)
	if err != nil {
		return AgentUpgrade{}, err
	}
	defer tx.Rollback(ctx)
	if err = lockNode(ctx, tx, nodeID); err != nil {
		return AgentUpgrade{}, err
	}
	var currentRevision int64
	if err = tx.QueryRow(ctx, "SELECT revision FROM nodes WHERE id=$1 FOR UPDATE", nodeID).Scan(&currentRevision); errors.Is(err, pgx.ErrNoRows) {
		return AgentUpgrade{}, ErrNotFound
	}
	if err != nil {
		return AgentUpgrade{}, err
	}
	if currentRevision != revision {
		return AgentUpgrade{}, ErrConflict
	}
	value, err := scanAgentUpgrade(tx.QueryRow(ctx, `UPDATE node_agent_upgrades SET state='cancelled',updated_at=now(),completed_at=now() WHERE node_id=$1 AND state IN ('queued','claimed') RETURNING `+agentUpgradeColumns, nodeID))
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentUpgrade{}, ErrConflict
	}
	if err != nil {
		return AgentUpgrade{}, err
	}
	if _, err = tx.Exec(ctx, "UPDATE nodes SET revision=revision+1 WHERE id=$1", nodeID); err != nil {
		return AgentUpgrade{}, err
	}
	if err = platformLog(ctx, tx, &who, "node.agent-upgrade-cancel", nodeID, map[string]any{"taskId": value.ID, "targetVersion": value.TargetVersion, "state": "cancelled"}); err != nil {
		return AgentUpgrade{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return AgentUpgrade{}, err
	}
	return value, nil
}

func newUpgradeLease() (token, digest string, err error) {
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		return "", "", err
	}
	token = hex.EncodeToString(raw)
	return token, bootstrapHash([]byte(token)), nil
}

func (s *Store) ClaimAgentUpgrade(ctx context.Context, nodeID, fingerprint, arch, currentVersion string) (*UpgradeClaim, error) {
	if !nodeIDPattern.MatchString(nodeID) || !fingerprintPattern.MatchString(fingerprint) || (arch != "amd64" && arch != "arm64") {
		return nil, ErrInvalid
	}
	if _, _, _, ok := buildinfo.ParseProduction(currentVersion); !ok {
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
	var id, targetVersion, previousVersion, state, amd64, arm64 string
	var leaseExpires *time.Time
	err = tx.QueryRow(ctx, `SELECT id::text,target_version,previous_version,state,amd64_sha256,arm64_sha256,lease_expires_at FROM node_agent_upgrades WHERE node_id=$1 AND state=ANY($2) FOR UPDATE`, nodeID, activeUpgradeStates).Scan(&id, &targetVersion, &previousVersion, &state, &amd64, &arm64, &leaseExpires)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, tx.Commit(ctx)
	}
	if err != nil {
		return nil, err
	}
	if state == "installing" || state == "verifying" {
		return nil, tx.Commit(ctx)
	}
	if state == "claimed" && leaseExpires != nil && leaseExpires.After(time.Now().UTC()) {
		return nil, tx.Commit(ctx)
	}
	if currentVersion != previousVersion || !buildinfo.CanUpgrade(currentVersion, targetVersion) {
		return nil, ErrConflict
	}
	token, digest, err := newUpgradeLease()
	if err != nil {
		return nil, err
	}
	expires := time.Now().UTC().Add(AgentUpgradeLease)
	if _, err = tx.Exec(ctx, `UPDATE node_agent_upgrades SET state='claimed',claimed_arch=$2,lease_token_hash=$3,lease_expires_at=$4,claimed_at=COALESCE(claimed_at,now()),updated_at=now() WHERE id=$1`, id, arch, digest, expires); err != nil {
		return nil, err
	}
	if err = platformLog(ctx, tx, nil, "node.agent-upgrade-claimed", nodeID, map[string]any{"taskId": id, "targetVersion": targetVersion, "state": "claimed"}); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	checksum := amd64
	if arch == "arm64" {
		checksum = arm64
	}
	return &UpgradeClaim{ID: id, TargetVersion: targetVersion, PreviousVersion: previousVersion, SHA256: checksum, LeaseToken: token}, nil
}

func validUpgradeTransition(from, to, errorCode string) bool {
	if from == to {
		return true
	}
	if to == "failed" {
		return (from == "claimed" || from == "installing" || from == "verifying") && validUpgradeError(errorCode)
	}
	if errorCode != "" {
		if to != "rolled-back" || !validUpgradeError(errorCode) {
			return false
		}
	}
	switch from + "->" + to {
	case "claimed->installing", "installing->verifying", "verifying->succeeded", "verifying->rolled-back":
		return to != "rolled-back" || errorCode != ""
	default:
		return false
	}
}

func (s *Store) ReportAgentUpgrade(ctx context.Context, nodeID, fingerprint string, report UpgradeReport) (AgentUpgrade, error) {
	if !nodeIDPattern.MatchString(nodeID) || !fingerprintPattern.MatchString(fingerprint) || !upgradeIDPattern.MatchString(report.ID) || len(report.LeaseToken) != 64 {
		return AgentUpgrade{}, ErrInvalid
	}
	if report.ErrorCode != "" && !validUpgradeError(report.ErrorCode) {
		return AgentUpgrade{}, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AgentUpgrade{}, err
	}
	defer tx.Rollback(ctx)
	if err = lockNode(ctx, tx, nodeID); err != nil {
		return AgentUpgrade{}, err
	}
	if err = checkNodeCredential(ctx, tx, nodeID, []string{fingerprint}); err != nil {
		return AgentUpgrade{}, err
	}
	var current, targetVersion, storedError, leaseDigest string
	var leaseExpires *time.Time
	err = tx.QueryRow(ctx, `SELECT state,target_version,error_code,COALESCE(lease_token_hash,''),lease_expires_at FROM node_agent_upgrades WHERE id=$1 AND node_id=$2 FOR UPDATE`, report.ID, nodeID).Scan(&current, &targetVersion, &storedError, &leaseDigest, &leaseExpires)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentUpgrade{}, ErrNotFound
	}
	if err != nil {
		return AgentUpgrade{}, err
	}
	if leaseDigest == "" || leaseDigest != bootstrapHash([]byte(report.LeaseToken)) {
		return AgentUpgrade{}, ErrUnauthorized
	}
	if current == "claimed" && report.State != current && (leaseExpires == nil || !leaseExpires.After(time.Now().UTC())) {
		return AgentUpgrade{}, ErrUnauthorized
	}
	if current == report.State {
		if storedError != report.ErrorCode {
			return AgentUpgrade{}, ErrConflict
		}
		value, scanErr := scanAgentUpgrade(tx.QueryRow(ctx, `SELECT `+agentUpgradeColumns+` FROM node_agent_upgrades WHERE id=$1`, report.ID))
		if scanErr != nil {
			return AgentUpgrade{}, scanErr
		}
		return value, tx.Commit(ctx)
	}
	if !validUpgradeTransition(current, report.State, report.ErrorCode) {
		return AgentUpgrade{}, ErrConflict
	}
	if report.State == "succeeded" {
		var version string
		var heartbeat *time.Time
		if err = tx.QueryRow(ctx, "SELECT agent_version,agent_heartbeat_at FROM nodes WHERE id=$1", nodeID).Scan(&version, &heartbeat); err != nil {
			return AgentUpgrade{}, err
		}
		if version != targetVersion || heartbeat == nil || heartbeat.Before(time.Now().UTC().Add(-AgentHeartbeatStaleAfter)) {
			return AgentUpgrade{}, ErrConflict
		}
	}
	terminal := report.State == "succeeded" || report.State == "failed" || report.State == "rolled-back"
	value, err := scanAgentUpgrade(tx.QueryRow(ctx, `UPDATE node_agent_upgrades SET state=$2,error_code=$3,reported_at=now(),updated_at=now(),lease_expires_at=CASE WHEN $4 THEN lease_expires_at ELSE now()+$5*interval '1 second' END,completed_at=CASE WHEN $4 THEN now() ELSE NULL END WHERE id=$1 RETURNING `+agentUpgradeColumns, report.ID, report.State, report.ErrorCode, terminal, AgentUpgradeLease.Seconds()))
	if err != nil {
		return AgentUpgrade{}, err
	}
	if err = platformLog(ctx, tx, nil, "node.agent-upgrade-"+report.State, nodeID, map[string]any{"taskId": report.ID, "targetVersion": targetVersion, "state": report.State}); err != nil {
		return AgentUpgrade{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return AgentUpgrade{}, err
	}
	return value, nil
}
