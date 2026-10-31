package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"reladock/internal/nodeauth"
	"reladock/internal/platform"
)

type rotationFake struct {
	sign          platform.BootstrapSigner
	renewCalls    int
	activateCalls int
	csrs          [][]byte
	renewErr      error
	activateErrs  []error
	certificate   []byte
}

func (f *rotationFake) Renew(_ context.Context, csr []byte) (platform.CertificateRotation, error) {
	f.renewCalls++
	f.csrs = append(f.csrs, append([]byte(nil), csr...))
	if f.renewErr != nil {
		err := f.renewErr
		f.renewErr = nil
		return platform.CertificateRotation{}, err
	}
	certificate := f.certificate
	if certificate == nil {
		var err error
		certificate, err = f.sign("hk-01", csr)
		if err != nil {
			return platform.CertificateRotation{}, err
		}
	}
	leaf, err := parseIssuedNodeCertificateForTest(certificate)
	if err != nil {
		return platform.CertificateRotation{}, err
	}
	return platform.CertificateRotation{Certificate: certificate, ExpiresAt: leaf.NotAfter}, nil
}

func (f *rotationFake) Activate(_ context.Context, _ *tls.Certificate) error {
	f.activateCalls++
	if len(f.activateErrs) == 0 {
		return nil
	}
	err := f.activateErrs[0]
	f.activateErrs = f.activateErrs[1:]
	return err
}

func rotationFixture(t *testing.T) (string, *CredentialManager, platform.BootstrapSigner) {
	t.Helper()
	dir := credentialFixture(t)
	manager, err := OpenCredentialManager(dir, "hk-01")
	if err != nil {
		t.Fatal(err)
	}
	signer, err := nodeauth.LoadBootstrapSigner(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key"), filepath.Join(dir, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	return dir, manager, signer.Sign
}

func parseIssuedNodeCertificateForTest(raw []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("missing certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}

func rotationCode(t *testing.T, err error) string {
	t.Helper()
	var rotationErr *RotationError
	if !errors.As(err, &rotationErr) {
		t.Fatalf("not a bounded rotation error: %v", err)
	}
	return rotationErr.Code
}

func TestRotationPersistsCSRAndResumesAfterRenewFailure(t *testing.T) {
	dir, manager, sign := rotationFixture(t)
	now := manager.Current().NotAfter.Add(-6 * 24 * time.Hour)
	fake := &rotationFake{sign: sign, renewErr: errors.New("response lost")}
	if code := rotationCode(t, manager.ReconcileRotation(context.Background(), fake, now)); code != "renew-unavailable" {
		t.Fatalf("renew failure code=%q", code)
	}
	if fake.renewCalls != 1 || len(fake.csrs) != 1 {
		t.Fatalf("renew attempts=%d", fake.renewCalls)
	}
	reopened, err := OpenCredentialManager(dir, "hk-01")
	if err != nil {
		t.Fatal(err)
	}
	if err = reopened.ReconcileRotation(context.Background(), fake, now); err != nil {
		t.Fatal(err)
	}
	if fake.renewCalls != 2 || string(fake.csrs[0]) != string(fake.csrs[1]) {
		t.Fatal("restart generated a different CSR")
	}
	if reopened.Current().Fingerprint == manager.Current().Fingerprint {
		t.Fatal("successful rotation did not publish new identity")
	}
	if _, err = os.Lstat(filepath.Join(dir, "rotation.json")); !os.IsNotExist(err) {
		t.Fatalf("completed rotation state retained: %v", err)
	}
}

func TestRotationResumesAfterActivationResponseLoss(t *testing.T) {
	dir, manager, sign := rotationFixture(t)
	old := manager.Current().Fingerprint
	now := manager.Current().NotAfter.Add(-6 * 24 * time.Hour)
	fake := &rotationFake{sign: sign, activateErrs: []error{errors.New("response lost")}}
	if code := rotationCode(t, manager.ReconcileRotation(context.Background(), fake, now)); code != "activate-unavailable" {
		t.Fatalf("activate failure code=%q", code)
	}
	reopened, err := OpenCredentialManager(dir, "hk-01")
	if err != nil {
		t.Fatal(err)
	}
	if err = reopened.ReconcileRotation(context.Background(), fake, now); err != nil {
		t.Fatal(err)
	}
	if fake.renewCalls != 1 || fake.activateCalls != 2 || reopened.Current().Fingerprint == old {
		t.Fatalf("activation recovery renew=%d activate=%d old=%v", fake.renewCalls, fake.activateCalls, reopened.Current().Fingerprint == old)
	}
}

func TestRotationPublicationFailureKeepsPendingStateForRecovery(t *testing.T) {
	dir, manager, sign := rotationFixture(t)
	old := manager.Current().Fingerprint
	now := manager.Current().NotAfter.Add(-6 * 24 * time.Hour)
	if err := os.Chmod(filepath.Join(dir, "identity.pem"), 0644); err != nil {
		t.Fatal(err)
	}
	fake := &rotationFake{sign: sign}
	if code := rotationCode(t, manager.ReconcileRotation(context.Background(), fake, now)); code != "publish-failed" {
		t.Fatalf("publish failure code=%q", code)
	}
	if manager.Current().Fingerprint != old {
		t.Fatal("failed publication changed in-memory identity")
	}
	if err := os.Chmod(filepath.Join(dir, "identity.pem"), 0600); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenCredentialManager(dir, "hk-01")
	if err != nil {
		t.Fatal(err)
	}
	if err = reopened.ReconcileRotation(context.Background(), fake, now); err != nil {
		t.Fatal(err)
	}
	if fake.renewCalls != 1 || fake.activateCalls != 2 || reopened.Current().Fingerprint == old {
		t.Fatalf("publish recovery renew=%d activate=%d", fake.renewCalls, fake.activateCalls)
	}
}

func TestRotationCorruptStateFailsClosed(t *testing.T) {
	dir, manager, sign := rotationFixture(t)
	if err := os.WriteFile(filepath.Join(dir, "rotation.json"), []byte(`{"phase":"issued","privateKey":"secret"}`), 0600); err != nil {
		t.Fatal(err)
	}
	fake := &rotationFake{sign: sign}
	err := manager.ReconcileRotation(context.Background(), fake, manager.Current().NotAfter.Add(-6*24*time.Hour))
	if code := rotationCode(t, err); code != "state-invalid" {
		t.Fatalf("corrupt state code=%q", code)
	}
	if fake.renewCalls != 0 || fake.activateCalls != 0 {
		t.Fatal("corrupt state reached control API")
	}
}

func TestRotationDoesNothingBeforeRenewalWindow(t *testing.T) {
	_, manager, sign := rotationFixture(t)
	fake := &rotationFake{sign: sign}
	if err := manager.ReconcileRotation(context.Background(), fake, time.Now()); err != nil {
		t.Fatal(err)
	}
	if fake.renewCalls != 0 || fake.activateCalls != 0 {
		t.Fatal("early rotation contacted control API")
	}
}
