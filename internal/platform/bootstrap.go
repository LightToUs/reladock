package platform

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"github.com/jackc/pgx/v5"
	"time"
)

type BootstrapGrant struct {
	Token     string    `json:"token"`
	NodeID    string    `json:"nodeId"`
	ExpiresAt time.Time `json:"expiresAt"`
	Revision  int64     `json:"revision"`
}
type BootstrapCertificate struct {
	NodeID      string `json:"nodeId"`
	Certificate []byte `json:"-"`
}
type BootstrapSigner func(string, []byte) ([]byte, error)

func bootstrapHash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func (s *Store) CreateNodeBootstrap(ctx context.Context, who Identity, id string, revision int64) (BootstrapGrant, error) {
	g := BootstrapGrant{}
	if !nodeIDPattern.MatchString(id) || revision < 1 {
		return g, ErrInvalid
	}
	tx, err := s.adminTx(ctx, who)
	if err != nil {
		return g, err
	}
	defer tx.Rollback(ctx)
	if err = lockNode(ctx, tx, id); err != nil {
		return g, err
	}
	var rev int64
	err = tx.QueryRow(ctx, "SELECT revision FROM nodes WHERE id=$1 FOR UPDATE", id).Scan(&rev)
	if errors.Is(err, pgx.ErrNoRows) {
		return g, ErrNotFound
	}
	if err != nil {
		return g, err
	}
	if rev != revision {
		return g, ErrConflict
	}
	random := make([]byte, 32)
	if _, err = rand.Read(random); err != nil {
		return g, err
	}
	g = BootstrapGrant{Token: hex.EncodeToString(random), NodeID: id, ExpiresAt: time.Now().UTC().Add(15 * time.Minute), Revision: rev + 1}
	if _, err = tx.Exec(ctx, "UPDATE node_bootstrap SET revoked=true WHERE node_id=$1", id); err != nil {
		return g, err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO node_bootstrap(token_hash,node_id,expires_at) VALUES($1,$2,$3)", bootstrapHash([]byte(g.Token)), id, g.ExpiresAt); err != nil {
		return g, err
	}
	if _, err = tx.Exec(ctx, "UPDATE nodes SET revision=revision+1 WHERE id=$1", id); err != nil {
		return g, err
	}
	if err = platformLog(ctx, tx, &who, "node.bootstrap-create", id, map[string]any{"expiresAt": g.ExpiresAt}); err != nil {
		return BootstrapGrant{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return BootstrapGrant{}, err
	}
	return g, nil
}

func (s *Store) RevokeNodeBootstrap(ctx context.Context, who Identity, id string, revision int64) error {
	tx, err := s.adminTx(ctx, who)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = lockNode(ctx, tx, id); err != nil {
		return err
	}
	var rev int64
	if err = tx.QueryRow(ctx, "SELECT revision FROM nodes WHERE id=$1 FOR UPDATE", id).Scan(&rev); err != nil {
		return err
	}
	if rev != revision {
		return ErrConflict
	}
	if _, err = tx.Exec(ctx, "UPDATE node_bootstrap SET revoked=true WHERE node_id=$1", id); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "UPDATE nodes SET revision=revision+1 WHERE id=$1", id); err != nil {
		return err
	}
	if err = platformLog(ctx, tx, &who, "node.bootstrap-revoke", id, nil); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) RedeemNodeBootstrap(ctx context.Context, token string, csr []byte, sign BootstrapSigner) (BootstrapCertificate, error) {
	out := BootstrapCertificate{}
	if len(token) != 64 || len(csr) == 0 || len(csr) > 8192 || sign == nil {
		return out, ErrUnauthorized
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	digest := bootstrapHash([]byte(token))
	var id string
	if err = tx.QueryRow(ctx, "SELECT node_id FROM node_bootstrap WHERE token_hash=$1", digest).Scan(&id); errors.Is(err, pgx.ErrNoRows) {
		return out, ErrUnauthorized
	}
	if err != nil {
		return out, err
	}
	if err = lockNode(ctx, tx, id); err != nil {
		return out, err
	}
	var expires time.Time
	var revoked bool
	var csrHash, fp *string
	var cert []byte
	if err = tx.QueryRow(ctx, "SELECT expires_at,revoked,csr_hash,certificate,fingerprint FROM node_bootstrap WHERE token_hash=$1 FOR UPDATE", digest).Scan(&expires, &revoked, &csrHash, &cert, &fp); err != nil {
		return out, err
	}
	if revoked || !time.Now().Before(expires) {
		return out, ErrUnauthorized
	}
	ch := bootstrapHash(csr)
	if csrHash != nil {
		if *csrHash != ch {
			return out, ErrUnauthorized
		}
		var current string
		if err = tx.QueryRow(ctx, "SELECT COALESCE(certificate_fingerprint,'') FROM nodes WHERE id=$1", id).Scan(&current); err != nil {
			return out, err
		}
		if fp == nil || current != *fp {
			return out, ErrUnauthorized
		}
		return BootstrapCertificate{NodeID: id, Certificate: cert}, nil
	}
	cert, err = sign(id, csr)
	if err != nil {
		return out, ErrInvalid
	}
	if _, err = tx.Exec(ctx, "DELETE FROM node_preflight WHERE node_id=$1", id); err != nil {
		return out, err
	}
	if len(cert) == 0 || len(cert) > 32768 {
		return out, ErrInvalid
	}
	fingerprint := bootstrapHash(cert)
	if p, _ := pem.Decode(cert); p != nil && p.Type == "CERTIFICATE" {
		fingerprint = bootstrapHash(p.Bytes)
	}
	// Never reset an applying barrier or any metering state during enrollment.
	if _, err = tx.Exec(ctx, "UPDATE nodes SET certificate_fingerprint=$2,revision=revision+1 WHERE id=$1", id, fingerprint); err != nil {
		return out, nodeError(err)
	}
	if _, err = tx.Exec(ctx, "UPDATE config_outbox SET state='superseded' WHERE node_id=$1 AND state='leased'", id); err != nil {
		return out, err
	}
	if err = enqueueConfig(ctx, tx, id); err != nil {
		return out, err
	}
	if _, err = tx.Exec(ctx, "UPDATE node_bootstrap SET csr_hash=$2,certificate=$3,fingerprint=$4 WHERE token_hash=$1", digest, ch, cert, fingerprint); err != nil {
		return out, err
	}
	if err = platformLog(ctx, tx, nil, "node.bootstrap-redeem", id, map[string]any{"fingerprint": fingerprint}); err != nil {
		return out, err
	}
	if err = tx.Commit(ctx); err != nil {
		return out, err
	}
	return BootstrapCertificate{NodeID: id, Certificate: cert}, nil
}
