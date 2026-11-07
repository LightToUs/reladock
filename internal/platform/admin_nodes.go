package platform

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"reladock/internal/nodeauth"
)

type NodeInput struct {
	ID                string       `json:"id"`
	Name              string       `json:"name"`
	Region            string       `json:"region"`
	ListenAddress     string       `json:"listenAddress"`
	PublicAddress     string       `json:"publicAddress"`
	CapacityMbps      int          `json:"capacityMbps"`
	ResourceMode      ResourceMode `json:"resourceMode"`
	DedicatedTenantID string       `json:"dedicatedTenantId"`
}
type ManagedNode struct {
	CertificateRotationSupported bool `json:"certificateRotationSupported"`
	Node
	DedicatedTenantID            string               `json:"dedicatedTenantId"`
	ListenAddress                string               `json:"listenAddress"`
	PublicAddress                string               `json:"publicAddress"`
	CapacityMbps                 int                  `json:"capacityMbps"`
	Revision                     int64                `json:"revision"`
	Hidden                       bool                 `json:"hidden"`
	VisibilityRevision           int64                `json:"visibilityRevision"`
	CertificateFingerprint       string               `json:"certificateFingerprint"`
	CertificateRotationRequested bool                 `json:"certificateRotationRequested"`
	ContactStatus                string               `json:"contactStatus"`
	AgentMode                    string               `json:"agentMode"`
	AgentStartedAt               *time.Time           `json:"agentStartedAt"`
	HeartbeatAt                  *time.Time           `json:"heartbeatAt"`
	CertificateNotAfter          *time.Time           `json:"certificateNotAfter"`
	RotationState                string               `json:"rotationState"`
	RotationErrorCode            string               `json:"rotationErrorCode"`
	RotationErrorSince           *time.Time           `json:"rotationErrorSince"`
	ProbeStatus                  string               `json:"probeStatus"`
	ProbeObservedAt              *time.Time           `json:"probeObservedAt"`
	Upgrade                      *AgentUpgradeSummary `json:"upgrade"`
	UpgradeTargetVersion         string               `json:"upgradeTargetVersion"`
	UpgradeEligible              bool                 `json:"upgradeEligible"`
	UpgradeBlockedReason         string               `json:"upgradeBlockedReason"`
	Withdrawable                 bool                 `json:"withdrawable"`
	RetirementEligible           bool                 `json:"retirementEligible"`
	RetirementReasons            []string             `json:"retirementReasons"`
	ResourceStatus               string               `json:"resourceStatus"`
	ResourceObservedAt           *time.Time           `json:"resourceObservedAt"`
	CPUPercent                   *int                 `json:"cpuPercent"`
	MemoryPercent                *int                 `json:"memoryPercent"`
	DiskPercent                  *int                 `json:"diskPercent"`
	ConntrackPercent             *int                 `json:"conntrackPercent"`
	ClockSkewSeconds             *int                 `json:"clockSkewSeconds"`
	DataplaneState               string               `json:"dataplaneState"`
}

func normalizeNode(input NodeInput) (NodeInput, error) {
	input.Name = strings.TrimSpace(input.Name)
	input.Region = strings.TrimSpace(input.Region)
	mode, err := normalizeResourceMode(string(input.ResourceMode))
	if err != nil {
		return input, err
	}
	input.ResourceMode = mode
	if mode == ResourceModeShared {
		if input.DedicatedTenantID != "" {
			return input, ErrInvalid
		}
	} else if !billingUUID.MatchString(input.DedicatedTenantID) {
		return input, ErrInvalid
	}
	if !nodeIDPattern.MatchString(input.ID) || !utf8.ValidString(input.Name) || utf8.RuneCountInString(input.Name) < 1 || utf8.RuneCountInString(input.Name) > 120 || !utf8.ValidString(input.Region) || utf8.RuneCountInString(input.Region) < 1 || utf8.RuneCountInString(input.Region) > 80 || net.ParseIP(input.ListenAddress) == nil || input.CapacityMbps < 1 || input.CapacityMbps > 1000000 {
		return input, ErrInvalid
	}
	input.ListenAddress = net.ParseIP(input.ListenAddress).String()
	if input.PublicAddress != "" {
		ip := net.ParseIP(input.PublicAddress)
		if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() {
			return input, ErrInvalid
		}
		input.PublicAddress = ip.String()
	}
	return input, nil
}
func (s *Store) AdminNodes(ctx context.Context, who Identity) ([]ManagedNode, error) {
	tx, err := s.adminReadTx(ctx, who)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT n.id,n.name,n.region,n.status,n.desired_generation,n.validated_generation,n.last_seen_at,n.agent_version,COALESCE(o.state,'queued'),COALESCE(o.attempts,0),n.resource_mode,COALESCE(n.dedicated_tenant_id::text,''),host(n.listen_address),COALESCE(host(n.public_address),''),n.capacity_mbps,n.revision,COALESCE(v.hidden,false),COALESCE(v.revision,1),COALESCE(n.certificate_fingerprint,''),n.certificate_renewal_fingerprint IS NOT NULL,n.agent_certificate_rotation_supported,n.applied_generation,CASE WHEN n.agent_heartbeat_at IS NULL THEN 'never-seen' WHEN n.agent_heartbeat_at<clock_timestamp()-$1*interval '1 second' THEN 'offline' ELSE 'online' END,n.agent_mode,n.agent_started_at,n.agent_heartbeat_at,n.agent_certificate_not_after,n.agent_rotation_state,n.agent_rotation_error_code,n.agent_rotation_error_since,
	CASE
	 WHEN EXISTS(SELECT 1 FROM probe_latest p WHERE p.node_id=n.id AND p.generation=n.desired_generation AND p.generation=n.applied_generation AND p.status='targetUnreachable') THEN 'targetUnreachable'
	 WHEN EXISTS(SELECT 1 FROM probe_latest p WHERE p.node_id=n.id AND p.generation=n.desired_generation AND p.generation=n.applied_generation AND p.status='targetReachable') THEN 'targetReachable'
	 WHEN EXISTS(SELECT 1 FROM probe_latest p WHERE p.node_id=n.id AND p.generation=n.desired_generation AND p.generation=n.applied_generation AND p.status='unknown') THEN 'udpUnknown'
	 ELSE 'unknown' END,
	(SELECT max(p.observed_at) FROM probe_latest p WHERE p.node_id=n.id AND p.generation=n.desired_generation AND p.generation=n.applied_generation),
	u.id::text,u.target_version,u.previous_version,u.state,u.error_code,u.requested_at,u.updated_at,u.completed_at,
	CASE WHEN rs.node_id IS NULL THEN 'unknown' WHEN rs.received_at<clock_timestamp()-interval '90 seconds' THEN 'stale' WHEN rs.dataplane_state='drifted' THEN 'drifted' WHEN COALESCE(rs.cpu_percent>=90,false) OR COALESCE(rs.memory_percent>=90,false) OR COALESCE(rs.disk_percent>=85,false) OR COALESCE(rs.conntrack_percent>=80,false) OR rs.clock_skew_seconds>120 THEN 'pressure' WHEN rs.cpu_percent IS NULL AND rs.memory_percent IS NULL AND rs.disk_percent IS NULL AND rs.conntrack_percent IS NULL THEN 'unknown' ELSE 'normal' END,
	rs.received_at,rs.cpu_percent,rs.memory_percent,rs.disk_percent,rs.conntrack_percent,rs.clock_skew_seconds,COALESCE(rs.dataplane_state,'')
	FROM nodes n LEFT JOIN config_outbox o ON o.node_id=n.id AND o.generation=n.desired_generation
	LEFT JOIN platform_node_visibility v ON v.node_id=n.id
	LEFT JOIN node_resource_latest rs ON rs.node_id=n.id
	LEFT JOIN LATERAL (SELECT id,target_version,previous_version,state,error_code,requested_at,updated_at,completed_at FROM node_agent_upgrades WHERE node_id=n.id ORDER BY requested_at DESC,id DESC LIMIT 1) u ON true
	ORDER BY n.id`, AgentHeartbeatStaleAfter.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []ManagedNode{}
	for rows.Next() {
		var n ManagedNode
		var upgradeID, targetVersion, previousVersion, state, errorCode *string
		var requestedAt, updatedAt, completedAt *time.Time
		if err = rows.Scan(&n.ID, &n.Name, &n.Region, &n.Status, &n.DesiredGeneration, &n.ValidatedGeneration, &n.LastSeenAt, &n.AgentVersion, &n.DeliveryState, &n.DeliveryAttempts, &n.ResourceMode, &n.DedicatedTenantID, &n.ListenAddress, &n.PublicAddress, &n.CapacityMbps, &n.Revision, &n.Hidden, &n.VisibilityRevision, &n.CertificateFingerprint, &n.CertificateRotationRequested, &n.CertificateRotationSupported, &n.AppliedGeneration, &n.ContactStatus, &n.AgentMode, &n.AgentStartedAt, &n.HeartbeatAt, &n.CertificateNotAfter, &n.RotationState, &n.RotationErrorCode, &n.RotationErrorSince, &n.ProbeStatus, &n.ProbeObservedAt, &upgradeID, &targetVersion, &previousVersion, &state, &errorCode, &requestedAt, &updatedAt, &completedAt, &n.ResourceStatus, &n.ResourceObservedAt, &n.CPUPercent, &n.MemoryPercent, &n.DiskPercent, &n.ConntrackPercent, &n.ClockSkewSeconds, &n.DataplaneState); err != nil {
			return nil, err
		}
		if upgradeID != nil {
			n.Upgrade = &AgentUpgradeSummary{ID: *upgradeID, TargetVersion: *targetVersion, PreviousVersion: *previousVersion, State: *state, ErrorCode: *errorCode, RequestedAt: *requestedAt, UpdatedAt: *updatedAt, CompletedAt: completedAt}
		}
		items = append(items, n)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i := range items {
		items[i].Withdrawable, err = s.nodeRegistrationWithdrawable(ctx, tx, items[i].ID)
		if err != nil {
			return nil, err
		}
		items[i].RetirementReasons, err = s.nodeRetirementReasons(ctx, tx, items[i].ID)
		if err != nil {
			return nil, err
		}
		items[i].RetirementEligible = len(items[i].RetirementReasons) == 0
	}
	return items, nil
}

func (s *Store) AdminSetNodeVisibility(ctx context.Context, who Identity, id string, visibilityRevision int64, hidden bool) error {
	if !nodeIDPattern.MatchString(id) || visibilityRevision < 1 {
		return ErrInvalid
	}
	tx, err := s.adminTx(ctx, who)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = lockNode(ctx, tx, id); err != nil {
		return err
	}
	var current int64
	var currentHidden bool
	err = tx.QueryRow(ctx, `SELECT COALESCE(v.revision,1),COALESCE(v.hidden,false) FROM nodes n LEFT JOIN platform_node_visibility v ON v.node_id=n.id WHERE n.id=$1 FOR UPDATE OF n`, id).Scan(&current, &currentHidden)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if current != visibilityRevision {
		return ErrConflict
	}
	if currentHidden == hidden {
		return tx.Commit(ctx)
	}
	next := current + 1
	if _, err = tx.Exec(ctx, `INSERT INTO platform_node_visibility(node_id,hidden,revision) VALUES($1,$2,$3) ON CONFLICT(node_id) DO UPDATE SET hidden=EXCLUDED.hidden,revision=EXCLUDED.revision,updated_at=now()`, id, hidden, next); err != nil {
		return err
	}
	action := "node.unhide"
	if hidden {
		action = "node.hide"
	}
	if err = platformLog(ctx, tx, &who, action, id, map[string]any{"hidden": hidden, "visibilityRevision": next}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) nodeRegistrationWithdrawable(ctx context.Context, tx pgx.Tx, id string) (bool, error) {
	var withdrawable bool
	err := tx.QueryRow(ctx, `SELECT
		n.status='unprovisioned'
		AND n.desired_generation=1
		AND n.validated_generation=0
		AND n.applied_generation=0
		AND n.last_seen_at IS NULL
		AND n.agent_version=''
		AND n.certificate_fingerprint IS NULL
		AND n.agent_heartbeat_at IS NULL
		AND n.agent_mode=''
		AND n.agent_started_at IS NULL
		AND n.agent_certificate_not_after IS NULL
		AND n.agent_rotation_state='idle'
		AND n.agent_rotation_error_code=''
		AND n.agent_rotation_error_since IS NULL
		AND n.agent_kernel_boot_id IS NULL
		AND n.agent_journal_sequence=0
		AND (SELECT count(*)=1 FROM config_outbox c WHERE c.node_id=n.id)
		AND EXISTS(SELECT 1 FROM config_outbox c WHERE c.node_id=n.id AND c.generation=1 AND c.state='queued' AND c.attempts=0 AND c.lease_token IS NULL AND c.lease_until IS NULL AND c.error_code='' AND c.reported_at IS NULL)
		AND NOT EXISTS(SELECT 1 FROM node_bootstrap b WHERE b.node_id=n.id AND (b.csr_hash IS NOT NULL OR b.certificate IS NOT NULL OR b.fingerprint IS NOT NULL))
		AND NOT EXISTS(SELECT 1 FROM relay_rules r WHERE r.node_id=n.id)
		AND NOT EXISTS(SELECT 1 FROM endpoint_allocations a WHERE a.node_id=n.id)
		AND NOT EXISTS(SELECT 1 FROM endpoint_retirements e WHERE e.node_id=n.id)
		AND NOT EXISTS(SELECT 1 FROM node_operator_events e WHERE e.node_id=n.id)
		AND NOT EXISTS(SELECT 1 FROM node_preflight p WHERE p.node_id=n.id)
		AND NOT EXISTS(SELECT 1 FROM node_certificate_rotations r WHERE r.node_id=n.id)
		AND NOT EXISTS(SELECT 1 FROM node_agent_upgrades u WHERE u.node_id=n.id)
		AND NOT EXISTS(SELECT 1 FROM node_drain_attestations a WHERE a.node_id=n.id)
		AND NOT EXISTS(SELECT 1 FROM node_resource_latest r WHERE r.node_id=n.id)
		AND NOT EXISTS(SELECT 1 FROM operations_incidents i WHERE i.node_id=n.id)
		AND NOT EXISTS(SELECT 1 FROM node_cost_entries c WHERE c.node_id=n.id)
		AND NOT EXISTS(SELECT 1 FROM probe_latest p WHERE p.node_id=n.id)
		AND NOT EXISTS(SELECT 1 FROM billing_usage_totals u WHERE u.node_id=n.id)
		AND NOT EXISTS(SELECT 1 FROM billing_usage_epochs e WHERE e.node_id=n.id)
		AND NOT EXISTS(SELECT 1 FROM billing_usage_reports r WHERE r.node_id=n.id)
		AND NOT EXISTS(SELECT 1 FROM subscription_usage u WHERE u.node_id=n.id)
		FROM nodes n WHERE n.id=$1`, id).Scan(&withdrawable)
	return withdrawable, err
}

func (s *Store) AdminWithdrawNodeRegistration(ctx context.Context, who Identity, id string, revision int64) error {
	if !nodeIDPattern.MatchString(id) || revision < 1 {
		return ErrInvalid
	}
	tx, err := s.adminTx(ctx, who)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = lockNode(ctx, tx, id); err != nil {
		return err
	}
	var current int64
	if err = tx.QueryRow(ctx, "SELECT revision FROM nodes WHERE id=$1 FOR UPDATE", id).Scan(&current); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if current != revision {
		return ErrConflict
	}
	withdrawable, err := s.nodeRegistrationWithdrawable(ctx, tx, id)
	if err != nil {
		return err
	}
	if !withdrawable {
		return ErrConflict
	}
	if _, err = tx.Exec(ctx, "DELETE FROM node_bootstrap WHERE node_id=$1", id); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "DELETE FROM config_outbox WHERE node_id=$1", id); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "DELETE FROM nodes WHERE id=$1", id); err != nil {
		return err
	}
	if err = platformLog(ctx, tx, &who, "node.withdraw-registration", id, map[string]any{"revision": current}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func nodeError(err error) error {
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) && pgerr.Code == "23505" {
		return ErrConflict
	}
	if errors.As(err, &pgerr) && pgerr.Code == "23503" {
		return ErrInvalid
	}
	return err
}

// Actor=nil is reserved for trusted local operator commands, not HTTP input.
func (s *Store) mutateNode(ctx context.Context, actor *Identity, id string, revision *int64, action string, input *NodeInput, fingerprint string, certificateNotAfter *time.Time) error {
	if !nodeIDPattern.MatchString(id) {
		return ErrInvalid
	}
	if input != nil {
		normalized, err := normalizeNode(*input)
		if err != nil {
			return err
		}
		input = &normalized
	}
	if revision != nil && *revision < 1 {
		return ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if actor != nil {
		if err = platformLock(ctx, tx); err != nil {
			return err
		}
		if err = authorizePlatform(ctx, tx, *actor); err != nil {
			return err
		}
	}
	if err = lockNode(ctx, tx, id); err != nil {
		return err
	}
	var current int64
	var bind string
	var currentStatus string
	var currentMode ResourceMode
	var currentOwner string
	if action != "node.add" {
		err = tx.QueryRow(ctx, "SELECT revision,host(listen_address),status,resource_mode,COALESCE(dedicated_tenant_id::text,'') FROM nodes WHERE id=$1 FOR UPDATE", id).Scan(&current, &bind, &currentStatus, &currentMode, &currentOwner)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if revision != nil && current != *revision {
			return ErrConflict
		}
		if currentStatus == "retired" || currentStatus == "draining" && action != "config.retry" {
			return ErrConflict
		}
	}
	details := map[string]any{}
	createdRevision := int64(1)
	switch action {
	case "node.add":
		if input == nil {
			return ErrInvalid
		}
		if err = tx.QueryRow(ctx, `SELECT COALESCE(MAX((details->>'revision')::bigint),0)+1 FROM platform_audit_logs WHERE resource_id=$1 AND jsonb_typeof(details->'revision')='number'`, id).Scan(&createdRevision); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "INSERT INTO nodes(id,name,region,status,listen_address,public_address,capacity_mbps,revision,resource_mode,dedicated_tenant_id) VALUES($1,$2,$3,'unprovisioned',$4,NULLIF($5,'')::inet,$6,$7,$8,NULLIF($9,'')::uuid)", id, input.Name, input.Region, input.ListenAddress, input.PublicAddress, input.CapacityMbps, createdRevision, input.ResourceMode, input.DedicatedTenantID); err != nil {
			return nodeError(err)
		}
		if err = s.enqueueConfig(ctx, tx, id); err != nil {
			return err
		}
		details["name"] = input.Name
	case "node.edit":
		if input == nil {
			return ErrInvalid
		}
		var count, maxBandwidth int
		if err = tx.QueryRow(ctx, "SELECT count(*),COALESCE(max(bandwidth_mbps),0) FROM relay_rules WHERE node_id=$1 AND status<>'deleted'", id).Scan(&count, &maxBandwidth); err != nil {
			return err
		}
		modeChanged := input.ResourceMode != currentMode || input.DedicatedTenantID != currentOwner
		if maxBandwidth > input.CapacityMbps || input.ListenAddress != bind && count > 0 || modeChanged && count > 0 {
			return ErrConflict
		}
		if _, err = tx.Exec(ctx, "UPDATE nodes SET name=$2,region=$3,listen_address=$4,public_address=NULLIF($5,'')::inet,capacity_mbps=$6,resource_mode=$7,dedicated_tenant_id=NULLIF($8,'')::uuid WHERE id=$1", id, input.Name, input.Region, input.ListenAddress, input.PublicAddress, input.CapacityMbps, input.ResourceMode, input.DedicatedTenantID); err != nil {
			return nodeError(err)
		}
		if input.ListenAddress != bind {
			if err = s.enqueueConfig(ctx, tx, id); err != nil {
				return err
			}
		}
		details["name"] = input.Name
	case "node.maintenance", "node.enable":
		status := "maintenance"
		if action == "node.enable" {
			status = "unprovisioned"
		}
		if _, err = tx.Exec(ctx, "UPDATE nodes SET status=$2 WHERE id=$1", id, status); err != nil {
			return err
		}
	case "node.enroll":
		if !fingerprintPattern.MatchString(fingerprint) {
			return ErrInvalid
		}
		if _, err = tx.Exec(ctx, `UPDATE nodes SET certificate_fingerprint=$2,last_seen_at=NULL,agent_heartbeat_at=NULL,agent_version='',agent_mode='',agent_started_at=NULL,agent_certificate_not_after=$3,agent_rotation_state='idle',agent_rotation_error_code='',agent_rotation_error_since=NULL,agent_kernel_boot_id=NULL,agent_journal_sequence=0 WHERE id=$1`, id, fingerprint, certificateNotAfter); err != nil {
			return nodeError(err)
		}
		if _, err = tx.Exec(ctx, "UPDATE config_outbox SET state='superseded' WHERE node_id=$1 AND state='leased'", id); err != nil {
			return err
		}
		if err = s.enqueueConfig(ctx, tx, id); err != nil {
			return err
		}
		details["fingerprint"] = fingerprint
	case "node.revoke-certificate":
		if _, err = tx.Exec(ctx, `UPDATE nodes SET certificate_fingerprint=NULL,last_seen_at=NULL,agent_heartbeat_at=NULL,agent_version='',agent_mode='',agent_started_at=NULL,agent_certificate_not_after=NULL,agent_rotation_state='idle',agent_rotation_error_code='',agent_rotation_error_since=NULL,agent_kernel_boot_id=NULL,agent_journal_sequence=0 WHERE id=$1`, id); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "UPDATE config_outbox SET state='superseded' WHERE node_id=$1 AND state='leased'", id); err != nil {
			return err
		}
	case "config.retry":
		if err = s.enqueueConfig(ctx, tx, id); err != nil {
			return err
		}
	default:
		return ErrInvalid
	}
	if action != "node.add" {
		if action == "node.enroll" || action == "node.revoke-certificate" {
			if _, err = tx.Exec(ctx, "DELETE FROM node_resource_latest WHERE node_id=$1", id); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, "DELETE FROM node_certificate_rotations WHERE node_id=$1", id); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, "DELETE FROM node_preflight WHERE node_id=$1", id); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, "UPDATE node_bootstrap SET revoked=true WHERE node_id=$1", id); err != nil {
				return err
			}
		}
		if _, err = tx.Exec(ctx, "UPDATE nodes SET revision=revision+1 WHERE id=$1", id); err != nil {
			return err
		}
		details["revision"] = current + 1
	} else {
		details["revision"] = createdRevision
	}
	if err = platformLog(ctx, tx, actor, action, id, details); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) AddNode(ctx context.Context, id, name, region, address string) error {
	return s.mutateNode(ctx, nil, id, nil, "node.add", &NodeInput{ID: id, Name: name, Region: region, ListenAddress: address, CapacityMbps: 1000, ResourceMode: ResourceModeShared}, "", nil)
}
func (s *Store) MaintainNode(ctx context.Context, id string, enabled bool) error {
	action := "node.enable"
	if enabled {
		action = "node.maintenance"
	}
	return s.mutateNode(ctx, nil, id, nil, action, nil, "", nil)
}
func (s *Store) EnrollNode(ctx context.Context, id, fingerprint string) error {
	return s.mutateNode(ctx, nil, id, nil, "node.enroll", nil, fingerprint, nil)
}
func (s *Store) RetryNodeConfig(ctx context.Context, id string) error {
	return s.mutateNode(ctx, nil, id, nil, "config.retry", nil, "", nil)
}
func (s *Store) AuthenticateNode(ctx context.Context, id, fingerprint string) error {
	var found string
	err := s.pool.QueryRow(ctx, "SELECT id FROM nodes WHERE id=$1 AND certificate_fingerprint=$2", id, fingerprint).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrUnauthorized
	}
	return err
}
func (s *Store) AdminAddNode(ctx context.Context, who Identity, input NodeInput) error {
	return s.mutateNode(ctx, &who, input.ID, nil, "node.add", &input, "", nil)
}
func (s *Store) AdminUpdateNode(ctx context.Context, who Identity, input NodeInput, revision int64) error {
	return s.mutateNode(ctx, &who, input.ID, &revision, "node.edit", &input, "", nil)
}
func (s *Store) AdminNodeAction(ctx context.Context, who Identity, id string, revision int64, action, certificate string) error {
	if action == "rotate-certificate" {
		if certificate != "" {
			return ErrInvalid
		}
		return s.RequestNodeCertificateRenewal(ctx, who, id, revision)
	}
	mapped := map[string]string{"maintenance": "node.maintenance", "enable": "node.enable", "retry": "config.retry", "enroll": "node.enroll", "revoke-certificate": "node.revoke-certificate"}[action]
	if mapped == "" {
		return ErrInvalid
	}
	fingerprint := ""
	var certificateNotAfter *time.Time
	if action == "enroll" {
		block, rest := pem.Decode([]byte(certificate))
		if block == nil || block.Type != "CERTIFICATE" || len(strings.TrimSpace(string(rest))) != 0 {
			return ErrInvalid
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return ErrInvalid
		}
		identity, err := nodeauth.NodeIdentity(cert)
		if err != nil || identity != id || cert.IsCA || time.Now().Before(cert.NotBefore) || time.Now().After(cert.NotAfter) {
			return ErrInvalid
		}
		clientUsage := false
		for _, use := range cert.ExtKeyUsage {
			if use == x509.ExtKeyUsageClientAuth {
				clientUsage = true
			}
		}
		if !clientUsage {
			return ErrInvalid
		}
		fingerprint = nodeauth.Fingerprint(cert)
		notAfter := cert.NotAfter.UTC()
		certificateNotAfter = &notAfter
	}
	return s.mutateNode(ctx, &who, id, &revision, mapped, nil, fingerprint, certificateNotAfter)
}

type DeliveryRecord struct {
	ID         string     `json:"id"`
	Generation int64      `json:"generation"`
	State      string     `json:"state"`
	Attempts   int        `json:"attempts"`
	ErrorCode  string     `json:"errorCode"`
	CreatedAt  time.Time  `json:"createdAt"`
	ReportedAt *time.Time `json:"reportedAt"`
}

func (s *Store) AdminDeliveries(ctx context.Context, who Identity, nodeID string) ([]DeliveryRecord, error) {
	tx, err := s.adminReadTx(ctx, who)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, "SELECT id,generation,state,attempts,error_code,created_at,reported_at FROM config_outbox WHERE node_id=$1 ORDER BY generation DESC LIMIT 30", nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []DeliveryRecord{}
	for rows.Next() {
		var r DeliveryRecord
		if err = rows.Scan(&r.ID, &r.Generation, &r.State, &r.Attempts, &r.ErrorCode, &r.CreatedAt, &r.ReportedAt); err != nil {
			return nil, err
		}
		items = append(items, r)
	}
	return items, rows.Err()
}
