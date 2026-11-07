package platform

import (
	"context"
	"errors"
	"os"

	"reladock/internal/nodeauth"
)

// Missing identity files on an established control plane are a recovery event,
// never an instruction to create a new CA and strand its enrolled nodes.
func (s *Store) EnsureManagedNodePKI(ctx context.Context, dir, hostname string) error {
	if _, err := os.Lstat(dir); os.IsNotExist(err) {
		var used bool
		err = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM nodes WHERE certificate_fingerprint IS NOT NULL OR last_seen_at IS NOT NULL OR agent_heartbeat_at IS NOT NULL)
   OR EXISTS(SELECT 1 FROM node_bootstrap WHERE csr_hash IS NOT NULL)
   OR EXISTS(SELECT 1 FROM node_certificate_rotations)
   OR EXISTS(SELECT 1 FROM platform_audit_logs WHERE action IN ('node.enroll','node.bootstrap-redeem','node.certificate-renew-activated','node.revoke-certificate'))
   OR EXISTS(SELECT 1 FROM node_operator_events)`).Scan(&used)
		if err != nil {
			return err
		}
		if used {
			return errors.New("node identity history exists: restore original managed PKI backup instead of creating a new CA")
		}
	} else if err != nil {
		return err
	}
	return nodeauth.EnsureManagedBundle(dir, hostname)
}
