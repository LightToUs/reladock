package platform

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"reladock/internal/nodeauth"
)

func rotationSigner(t *testing.T) *nodeauth.BootstrapCA {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "rotation-ca")
	if err := nodeauth.CreateLocalBundle(dir, "hk-01"); err != nil {
		t.Fatal(err)
	}
	signer, err := nodeauth.LoadBootstrapSigner(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key"), filepath.Join(dir, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func ed25519CSR(t *testing.T) []byte {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
}

func rsaCSR(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
}

func TestCertificateRotationIssueIsAuthorizedIdempotentAndNodeScoped(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	current := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := s.pool.Exec(ctx, "UPDATE nodes SET certificate_fingerprint=$1 WHERE id='hk-01'", current); err != nil {
		t.Fatal(err)
	}
	signer := rotationSigner(t)
	csr := ed25519CSR(t)
	var signs atomic.Int32
	sign := func(id string, raw []byte) ([]byte, error) {
		signs.Add(1)
		return signer.Sign(id, raw)
	}
	if _, err := s.RenewNodeCertificate(ctx, "hk-01", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", csr, sign); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong current fingerprint: %v", err)
	}
	first, err := s.RenewNodeCertificate(ctx, "hk-01", current, csr, sign)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.RenewNodeCertificate(ctx, "hk-01", current, csr, sign)
	if err != nil || !bytes.Equal(first.Certificate, second.Certificate) || !first.ExpiresAt.Equal(second.ExpiresAt) {
		t.Fatalf("same CSR replay changed result: %+v %+v %v", first, second, err)
	}
	if signs.Load() != 1 {
		t.Fatalf("same CSR signed %d times", signs.Load())
	}
	block, _ := pem.Decode(first.Certificate)
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	id, err := nodeauth.NodeIdentity(leaf)
	if err != nil || id != "hk-01" || !leaf.NotAfter.Equal(first.ExpiresAt) {
		t.Fatalf("renewed identity: id=%q expiry=%v result=%v err=%v", id, leaf.NotAfter, first.ExpiresAt, err)
	}
	var stored string
	if err = s.pool.QueryRow(ctx, "SELECT certificate_fingerprint FROM nodes WHERE id='hk-01'").Scan(&stored); err != nil || stored != current {
		t.Fatalf("renewal changed current identity: %q %v", stored, err)
	}
	if _, err = s.RenewNodeCertificate(ctx, "hk-01", current, ed25519CSR(t), sign); !errors.Is(err, ErrConflict) {
		t.Fatalf("different pending CSR: %v", err)
	}
}

func TestCertificateRotationConcurrentReplayCreatesOnePendingIdentity(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	current := "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	if _, err := s.pool.Exec(ctx, "UPDATE nodes SET certificate_fingerprint=$1 WHERE id='hk-01'", current); err != nil {
		t.Fatal(err)
	}
	signer := rotationSigner(t)
	csr := ed25519CSR(t)
	var signs atomic.Int32
	sign := func(id string, raw []byte) ([]byte, error) {
		signs.Add(1)
		return signer.Sign(id, raw)
	}
	const workers = 8
	results := make([]CertificateRotation, workers)
	errs := make([]error, workers)
	var wait sync.WaitGroup
	wait.Add(workers)
	for i := range results {
		go func(i int) {
			defer wait.Done()
			results[i], errs[i] = s.RenewNodeCertificate(ctx, "hk-01", current, csr, sign)
		}(i)
	}
	wait.Wait()
	for i := range results {
		if errs[i] != nil || !bytes.Equal(results[0].Certificate, results[i].Certificate) {
			t.Fatalf("concurrent result %d: %v", i, errs[i])
		}
	}
	var rows int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM node_certificate_rotations WHERE node_id='hk-01'").Scan(&rows); err != nil || rows != 1 || signs.Load() != 1 {
		t.Fatalf("pending rows=%d signs=%d err=%v", rows, signs.Load(), err)
	}
}

func TestCertificateRotationRejectsNonEd25519CSRBeforeSigning(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	current := "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	if _, err := s.pool.Exec(ctx, "UPDATE nodes SET certificate_fingerprint=$1 WHERE id='hk-01'", current); err != nil {
		t.Fatal(err)
	}
	called := false
	_, err := s.RenewNodeCertificate(ctx, "hk-01", current, rsaCSR(t), func(string, []byte) ([]byte, error) {
		called = true
		return nil, nil
	})
	if !errors.Is(err, ErrInvalid) || called {
		t.Fatalf("non-Ed25519 CSR reached signer: called=%v err=%v", called, err)
	}
}

func TestCertificateActivationPromotesPendingAndIsIdempotent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	current := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	if _, err := s.pool.Exec(ctx, "UPDATE nodes SET certificate_fingerprint=$1 WHERE id='hk-01'", current); err != nil {
		t.Fatal(err)
	}
	signer := rotationSigner(t)
	issued, err := s.RenewNodeCertificate(ctx, "hk-01", current, ed25519CSR(t), signer.Sign)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(issued.Certificate)
	leaf, _ := x509.ParseCertificate(block.Bytes)
	pending := nodeauth.Fingerprint(leaf)
	if err = s.AuthenticateNode(ctx, "hk-01", pending); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("pending identity reached ordinary API: %v", err)
	}
	if err = s.AuthenticateNodeForActivation(ctx, "hk-01", pending); err != nil {
		t.Fatalf("pending identity cannot activate: %v", err)
	}
	if err = s.ActivateNodeCertificate(ctx, "hk-01", pending); err != nil {
		t.Fatal(err)
	}
	if err = s.ActivateNodeCertificate(ctx, "hk-01", pending); err != nil {
		t.Fatalf("lost activation response cannot replay: %v", err)
	}
	if err = s.AuthenticateNode(ctx, "hk-01", pending); err != nil {
		t.Fatalf("promoted identity rejected: %v", err)
	}
	if err = s.AuthenticateNode(ctx, "hk-01", current); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("old identity survived activation: %v", err)
	}
	if err = s.AuthenticateNodeForActivation(ctx, "hk-01", current); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("old identity can activate: %v", err)
	}
	var pendingRows, auditRows int
	if err = s.pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM node_certificate_rotations WHERE node_id='hk-01'),(SELECT count(*) FROM platform_audit_logs WHERE action='node.certificate-renew-activated' AND resource_id='hk-01')").Scan(&pendingRows, &auditRows); err != nil || pendingRows != 0 || auditRows != 1 {
		t.Fatalf("activation rows pending=%d audit=%d err=%v", pendingRows, auditRows, err)
	}
}

func TestCertificateRotationManualEnrollmentCancelsPending(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	current := "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	if _, err := s.pool.Exec(ctx, "UPDATE nodes SET certificate_fingerprint=$1 WHERE id='hk-01'", current); err != nil {
		t.Fatal(err)
	}
	signer := rotationSigner(t)
	issued, err := s.RenewNodeCertificate(ctx, "hk-01", current, ed25519CSR(t), signer.Sign)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(issued.Certificate)
	leaf, _ := x509.ParseCertificate(block.Bytes)
	pending := nodeauth.Fingerprint(leaf)
	manual := "abababababababababababababababababababababababababababababababab"
	if err = s.EnrollNode(ctx, "hk-01", manual); err != nil {
		t.Fatal(err)
	}
	if err = s.AuthenticateNodeForActivation(ctx, "hk-01", pending); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("cancelled pending identity can activate: %v", err)
	}
	var rows int
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM node_certificate_rotations WHERE node_id='hk-01'").Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("manual enrollment retained rotation rows=%d err=%v", rows, err)
	}
}

func TestCertificateRotationManualRevocationCancelsPending(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	current := "acacacacacacacacacacacacacacacacacacacacacacacacacacacacacacacac"
	if _, err := s.pool.Exec(ctx, "UPDATE nodes SET certificate_fingerprint=$1 WHERE id='hk-01'", current); err != nil {
		t.Fatal(err)
	}
	signer := rotationSigner(t)
	issued, err := s.RenewNodeCertificate(ctx, "hk-01", current, ed25519CSR(t), signer.Sign)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(issued.Certificate)
	leaf, _ := x509.ParseCertificate(block.Bytes)
	pending := nodeauth.Fingerprint(leaf)
	if err = s.AdminNodeAction(ctx, admin, "hk-01", 1, "revoke-certificate", ""); err != nil {
		t.Fatal(err)
	}
	if err = s.AuthenticateNodeForActivation(ctx, "hk-01", pending); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked pending identity can activate: %v", err)
	}
}

func TestCertificateRotationManualBootstrapCancelsPending(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	current := "adadadadadadadadadadadadadadadadadadadadadadadadadadadadadadadad"
	if _, err := s.pool.Exec(ctx, "UPDATE nodes SET certificate_fingerprint=$1 WHERE id='hk-01'", current); err != nil {
		t.Fatal(err)
	}
	signer := rotationSigner(t)
	issued, err := s.RenewNodeCertificate(ctx, "hk-01", current, ed25519CSR(t), signer.Sign)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(issued.Certificate)
	leaf, _ := x509.ParseCertificate(block.Bytes)
	pending := nodeauth.Fingerprint(leaf)
	grant, err := s.CreateNodeBootstrap(ctx, admin, "hk-01", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.RedeemNodeBootstrap(ctx, grant.Token, ed25519CSR(t), signer.Sign); err != nil {
		t.Fatal(err)
	}
	if err = s.AuthenticateNodeForActivation(ctx, "hk-01", pending); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("bootstrap-replaced pending identity can activate: %v", err)
	}
}
