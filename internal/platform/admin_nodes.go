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
	ID            string `json:"id"`
	Name          string `json:"name"`
	Region        string `json:"region"`
	ListenAddress string `json:"listenAddress"`
	PublicAddress string `json:"publicAddress"`
	CapacityMbps  int    `json:"capacityMbps"`
}
type ManagedNode struct {
	Node
	ListenAddress          string `json:"listenAddress"`
	PublicAddress          string `json:"publicAddress"`
	CapacityMbps           int    `json:"capacityMbps"`
	Revision               int64  `json:"revision"`
	CertificateFingerprint string `json:"certificateFingerprint"`
	ContactStatus          string `json:"contactStatus"`
}

func normalizeNode(input NodeInput) (NodeInput, error) {
	input.Name = strings.TrimSpace(input.Name)
	input.Region = strings.TrimSpace(input.Region)
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
	rows, err := tx.Query(ctx, `SELECT n.id,n.name,n.region,n.status,n.desired_generation,n.validated_generation,n.last_seen_at,n.agent_version,COALESCE(o.state,'queued'),COALESCE(o.attempts,0),host(n.listen_address),COALESCE(host(n.public_address),''),n.capacity_mbps,n.revision,COALESCE(n.certificate_fingerprint,''),n.applied_generation,CASE WHEN n.last_seen_at IS NULL THEN 'never-seen' WHEN n.last_seen_at<clock_timestamp()-$1*interval '1 second' THEN 'offline' ELSE 'online' END FROM nodes n LEFT JOIN config_outbox o ON o.node_id=n.id AND o.generation=n.desired_generation ORDER BY n.id`, DefaultContactStaleAfter.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []ManagedNode{}
	for rows.Next() {
		var n ManagedNode
		if err = rows.Scan(&n.ID, &n.Name, &n.Region, &n.Status, &n.DesiredGeneration, &n.ValidatedGeneration, &n.LastSeenAt, &n.AgentVersion, &n.DeliveryState, &n.DeliveryAttempts, &n.ListenAddress, &n.PublicAddress, &n.CapacityMbps, &n.Revision, &n.CertificateFingerprint, &n.AppliedGeneration, &n.ContactStatus); err != nil {
			return nil, err
		}
		items = append(items, n)
	}
	return items, rows.Err()
}
func nodeError(err error) error {
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) && pgerr.Code == "23505" {
		return ErrConflict
	}
	return err
}

// Actor=nil is reserved for trusted local operator commands, not HTTP input.
func (s *Store) mutateNode(ctx context.Context, actor *Identity, id string, revision *int64, action string, input *NodeInput, fingerprint string) error {
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
	if action != "node.add" {
		err = tx.QueryRow(ctx, "SELECT revision,host(listen_address) FROM nodes WHERE id=$1 FOR UPDATE", id).Scan(&current, &bind)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if revision != nil && current != *revision {
			return ErrConflict
		}
	}
	details := map[string]any{}
	switch action {
	case "node.add":
		if input == nil {
			return ErrInvalid
		}
		if _, err = tx.Exec(ctx, "INSERT INTO nodes(id,name,region,status,listen_address,public_address,capacity_mbps) VALUES($1,$2,$3,'unprovisioned',$4,NULLIF($5,'')::inet,$6)", id, input.Name, input.Region, input.ListenAddress, input.PublicAddress, input.CapacityMbps); err != nil {
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
		if maxBandwidth > input.CapacityMbps || input.ListenAddress != bind && count > 0 {
			return ErrConflict
		}
		if _, err = tx.Exec(ctx, "UPDATE nodes SET name=$2,region=$3,listen_address=$4,public_address=NULLIF($5,'')::inet,capacity_mbps=$6 WHERE id=$1", id, input.Name, input.Region, input.ListenAddress, input.PublicAddress, input.CapacityMbps); err != nil {
			return err
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
		if _, err = tx.Exec(ctx, "UPDATE nodes SET certificate_fingerprint=$2 WHERE id=$1", id, fingerprint); err != nil {
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
		if _, err = tx.Exec(ctx, "UPDATE nodes SET certificate_fingerprint=NULL WHERE id=$1", id); err != nil {
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
		details["revision"] = 1
	}
	if err = platformLog(ctx, tx, actor, action, id, details); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) AddNode(ctx context.Context, id, name, region, address string) error {
	return s.mutateNode(ctx, nil, id, nil, "node.add", &NodeInput{ID: id, Name: name, Region: region, ListenAddress: address, CapacityMbps: 1000}, "")
}
func (s *Store) MaintainNode(ctx context.Context, id string, enabled bool) error {
	action := "node.enable"
	if enabled {
		action = "node.maintenance"
	}
	return s.mutateNode(ctx, nil, id, nil, action, nil, "")
}
func (s *Store) EnrollNode(ctx context.Context, id, fingerprint string) error {
	return s.mutateNode(ctx, nil, id, nil, "node.enroll", nil, fingerprint)
}
func (s *Store) RetryNodeConfig(ctx context.Context, id string) error {
	return s.mutateNode(ctx, nil, id, nil, "config.retry", nil, "")
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
	return s.mutateNode(ctx, &who, input.ID, nil, "node.add", &input, "")
}
func (s *Store) AdminUpdateNode(ctx context.Context, who Identity, input NodeInput, revision int64) error {
	return s.mutateNode(ctx, &who, input.ID, &revision, "node.edit", &input, "")
}
func (s *Store) AdminNodeAction(ctx context.Context, who Identity, id string, revision int64, action, certificate string) error {
	mapped := map[string]string{"maintenance": "node.maintenance", "enable": "node.enable", "retry": "config.retry", "enroll": "node.enroll", "revoke-certificate": "node.revoke-certificate"}[action]
	if mapped == "" {
		return ErrInvalid
	}
	fingerprint := ""
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
	}
	return s.mutateNode(ctx, &who, id, &revision, mapped, nil, fingerprint)
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
