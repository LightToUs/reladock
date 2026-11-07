package platform

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"reladock/internal/nodeauth"
)

type CertificateRotation struct {
	Certificate []byte    `json:"-"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

// RequestNodeCertificateRenewal records intent only. The current identity stays
// authorized until the Agent proves possession of the new key during activation.
func (s *Store) RequestNodeCertificateRenewal(ctx context.Context, who Identity, nodeID string, revision int64) error {
	if !nodeIDPattern.MatchString(nodeID) || revision < 1 {
		return ErrInvalid
	}
	tx, err := s.adminTx(ctx, who)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = lockNode(ctx, tx, nodeID); err != nil {
		return err
	}
	var supported bool
	var currentRevision int64
	var status, current, desired string
	err = tx.QueryRow(ctx, `SELECT revision,status,COALESCE(certificate_fingerprint,''),COALESCE(certificate_renewal_fingerprint,''),agent_certificate_rotation_supported FROM nodes WHERE id=$1 FOR UPDATE`, nodeID).Scan(&currentRevision, &status, &current, &desired, &supported)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if !supported || currentRevision != revision || status == "draining" || status == "retired" || current == "" {
		return ErrConflict
	}
	if desired == current {
		return tx.Commit(ctx)
	}
	if _, err = tx.Exec(ctx, "UPDATE nodes SET certificate_renewal_fingerprint=certificate_fingerprint,revision=revision+1 WHERE id=$1", nodeID); err != nil {
		return err
	}
	if err = platformLog(ctx, tx, &who, "node.certificate-renew-requested", nodeID, map[string]any{"revision": revision + 1}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// NodeCertificateRenewalRequest rechecks current authorization after heartbeat
// commit. A concurrent activation/revocation cannot send intent to an old peer.
func (s *Store) NodeCertificateRenewalRequest(ctx context.Context, nodeID, fingerprint string) (string, error) {
	if !nodeIDPattern.MatchString(nodeID) || !fingerprintPattern.MatchString(fingerprint) {
		return "", ErrUnauthorized
	}
	var desired string
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(certificate_renewal_fingerprint,'') FROM nodes WHERE id=$1 AND certificate_fingerprint=$2`, nodeID, fingerprint).Scan(&desired)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrUnauthorized
	}
	return desired, err
}

func validateRenewalCSR(raw []byte) error {
	if len(raw) == 0 || len(raw) > 8192 {
		return ErrInvalid
	}
	block, rest := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE REQUEST" || len(rest) != 0 {
		return ErrInvalid
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil || csr.CheckSignature() != nil {
		return ErrInvalid
	}
	if _, ok := csr.PublicKey.(ed25519.PublicKey); !ok || csr.Subject.String() != "" || len(csr.Extensions) != 0 {
		return ErrInvalid
	}
	return nil
}

func parseIssuedNodeCertificate(nodeID string, raw []byte) (*x509.Certificate, string, error) {
	if len(raw) == 0 || len(raw) > 32768 {
		return nil, "", ErrInvalid
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, "", ErrInvalid
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil || cert.IsCA || !cert.NotAfter.After(time.Now()) {
		return nil, "", ErrInvalid
	}
	id, err := nodeauth.NodeIdentity(cert)
	if err != nil || id != nodeID {
		return nil, "", ErrInvalid
	}
	clientAuth := false
	for _, usage := range cert.ExtKeyUsage {
		if usage == x509.ExtKeyUsageClientAuth {
			clientAuth = true
		}
	}
	if !clientAuth {
		return nil, "", ErrInvalid
	}
	return cert, nodeauth.Fingerprint(cert), nil
}

func (s *Store) RenewNodeCertificate(ctx context.Context, nodeID, currentFingerprint string, csr []byte, sign BootstrapSigner) (CertificateRotation, error) {
	result := CertificateRotation{}
	if !nodeIDPattern.MatchString(nodeID) || !fingerprintPattern.MatchString(currentFingerprint) || sign == nil {
		return result, ErrInvalid
	}
	if err := validateRenewalCSR(csr); err != nil {
		return result, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	if err = lockNode(ctx, tx, nodeID); err != nil {
		return result, err
	}
	if err = checkNodeCredential(ctx, tx, nodeID, []string{currentFingerprint}); err != nil {
		return result, err
	}
	hash := bootstrapHash(csr)
	var storedHash string
	var storedCertificate []byte
	var storedExpiry time.Time
	err = tx.QueryRow(ctx, "SELECT csr_hash,certificate,not_after FROM node_certificate_rotations WHERE node_id=$1 FOR UPDATE", nodeID).Scan(&storedHash, &storedCertificate, &storedExpiry)
	if err == nil {
		if storedHash != hash {
			return result, ErrConflict
		}
		return CertificateRotation{Certificate: storedCertificate, ExpiresAt: storedExpiry}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return result, err
	}
	certificate, err := sign(nodeID, csr)
	if err != nil {
		return result, ErrInvalid
	}
	leaf, fingerprint, err := parseIssuedNodeCertificate(nodeID, certificate)
	if err != nil {
		return result, err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO node_certificate_rotations(node_id,csr_hash,certificate,fingerprint,not_after) VALUES($1,$2,$3,$4,$5)", nodeID, hash, certificate, fingerprint, leaf.NotAfter); err != nil {
		return result, nodeError(err)
	}
	if err = platformLog(ctx, tx, nil, "node.certificate-renew-issued", nodeID, map[string]any{"expiresAt": leaf.NotAfter}); err != nil {
		return result, err
	}
	if err = tx.Commit(ctx); err != nil {
		return result, err
	}
	return CertificateRotation{Certificate: certificate, ExpiresAt: leaf.NotAfter}, nil
}

func (s *Store) AuthenticateNodeForActivation(ctx context.Context, nodeID, fingerprint string) error {
	if !nodeIDPattern.MatchString(nodeID) || !fingerprintPattern.MatchString(fingerprint) {
		return ErrUnauthorized
	}
	var allowed bool
	err := s.pool.QueryRow(ctx, `SELECT CASE WHEN r.node_id IS NULL THEN COALESCE(n.certificate_fingerprint=$2,false) ELSE r.fingerprint=$2 END FROM nodes n LEFT JOIN node_certificate_rotations r ON r.node_id=n.id WHERE n.id=$1`, nodeID, fingerprint).Scan(&allowed)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !allowed {
		return ErrUnauthorized
	}
	return err
}

func (s *Store) ActivateNodeCertificate(ctx context.Context, nodeID, presentedFingerprint string) error {
	if !nodeIDPattern.MatchString(nodeID) || !fingerprintPattern.MatchString(presentedFingerprint) {
		return ErrUnauthorized
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = lockNode(ctx, tx, nodeID); err != nil {
		return err
	}
	var current string
	if err = tx.QueryRow(ctx, "SELECT COALESCE(certificate_fingerprint,'') FROM nodes WHERE id=$1 FOR UPDATE", nodeID).Scan(&current); errors.Is(err, pgx.ErrNoRows) {
		return ErrUnauthorized
	}
	if err != nil {
		return err
	}
	var pending string
	var notAfter time.Time
	err = tx.QueryRow(ctx, "SELECT fingerprint,not_after FROM node_certificate_rotations WHERE node_id=$1 FOR UPDATE", nodeID).Scan(&pending, &notAfter)
	if errors.Is(err, pgx.ErrNoRows) {
		if current != presentedFingerprint {
			return ErrUnauthorized
		}
		return nil
	}
	if err != nil {
		return err
	}
	if pending != presentedFingerprint {
		return ErrUnauthorized
	}
	if _, err = tx.Exec(ctx, "UPDATE nodes SET certificate_fingerprint=$2,certificate_renewal_fingerprint=NULL,agent_certificate_not_after=$3,revision=revision+1 WHERE id=$1", nodeID, pending, notAfter); err != nil {
		return nodeError(err)
	}
	if _, err = tx.Exec(ctx, "DELETE FROM node_certificate_rotations WHERE node_id=$1", nodeID); err != nil {
		return err
	}
	if err = platformLog(ctx, tx, nil, "node.certificate-renew-activated", nodeID, map[string]any{"expiresAt": notAfter}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
