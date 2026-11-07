package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"reladock/internal/platform"
)

const certificateRenewalWindow = 7 * 24 * time.Hour

var renewalFingerprintPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func (m *CredentialManager) RotationRequests() <-chan struct{} { return m.rotationRequested }

// Server intent is durable until activation; once work begins rotation.json
// takes over recovery. Delayed responses cannot renew a replacement identity.
func (m *CredentialManager) RequestRotation(fingerprint string) error {
	if fingerprint == "" {
		return nil
	}
	if !renewalFingerprintPattern.MatchString(fingerprint) {
		return errors.New("invalid certificate renewal request")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if fingerprint != m.info.Fingerprint || fingerprint == m.desiredFingerprint {
		return nil
	}
	m.desiredFingerprint = fingerprint
	select {
	case m.rotationRequested <- struct{}{}:
	default:
	}
	return nil
}

type RotationAPI interface {
	Renew(context.Context, []byte) (platform.CertificateRotation, error)
	Activate(context.Context, *tls.Certificate) error
}

type RotationError struct {
	Code string
	Err  error
}

func (e *RotationError) Error() string { return "certificate rotation failed: " + e.Code }
func (e *RotationError) Unwrap() error { return e.Err }

type rotationRecord struct {
	Phase       string `json:"phase"`
	CSR         string `json:"csr"`
	PrivateKey  string `json:"privateKey"`
	Certificate string `json:"certificate,omitempty"`
}

func (m *CredentialManager) setRotationStatus(state, code string) {
	m.mu.Lock()
	m.rotationState = state
	m.rotationErrorCode = code
	m.mu.Unlock()
}

func (m *CredentialManager) RotationStatus() (string, string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	state := m.rotationState
	if state == "" {
		state = "idle"
	}
	return state, m.rotationErrorCode
}

func rotationFailure(m *CredentialManager, code string, err error) error {
	m.setRotationStatus("error", code)
	return &RotationError{Code: code, Err: err}
}

func decodeRotationRecord(raw []byte) (rotationRecord, error) {
	var record rotationRecord
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return record, err
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return record, errors.New("trailing rotation state")
	}
	if (record.Phase != "requesting" && record.Phase != "issued") || record.CSR == "" || record.PrivateKey == "" || record.Phase == "requesting" && record.Certificate != "" || record.Phase == "issued" && record.Certificate == "" {
		return record, errors.New("invalid rotation state")
	}
	keyBlock, keyRest := pem.Decode([]byte(record.PrivateKey))
	csrBlock, csrRest := pem.Decode([]byte(record.CSR))
	if keyBlock == nil || keyBlock.Type != "PRIVATE KEY" || len(keyRest) != 0 || csrBlock == nil || csrBlock.Type != "CERTIFICATE REQUEST" || len(csrRest) != 0 {
		return record, errors.New("invalid rotation key or CSR")
	}
	parsedKey, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return record, err
	}
	key, ok := parsedKey.(ed25519.PrivateKey)
	if !ok {
		return record, errors.New("rotation key is not Ed25519")
	}
	csr, err := x509.ParseCertificateRequest(csrBlock.Bytes)
	if err != nil || csr.CheckSignature() != nil {
		return record, errors.New("invalid rotation CSR")
	}
	want, _ := x509.MarshalPKIXPublicKey(key.Public())
	if !bytes.Equal(want, csr.RawSubjectPublicKeyInfo) {
		return record, errors.New("rotation key and CSR mismatch")
	}
	return record, nil
}

func (m *CredentialManager) loadRotation() (*rotationRecord, error) {
	path := filepath.Join(m.pkiDir, "rotation.json")
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if err := safePrivateFile(path); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) == 0 || len(raw) > 65536 {
		return nil, errors.New("invalid rotation state size")
	}
	record, err := decodeRotationRecord(raw)
	if err != nil {
		return nil, err
	}
	return &record, nil
}

func (m *CredentialManager) saveRotation(record rotationRecord) error {
	raw, err := json.Marshal(record)
	if err != nil || len(raw) > 65536 {
		return errors.New("rotation state exceeds size bound")
	}
	if _, err = decodeRotationRecord(raw); err != nil {
		return err
	}
	temp, err := os.CreateTemp(m.pkiDir, ".rotation-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if err = temp.Chmod(0600); err == nil {
		_, err = temp.Write(raw)
	}
	if err == nil {
		err = temp.Sync()
	}
	closeErr := temp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	path := filepath.Join(m.pkiDir, "rotation.json")
	if _, statErr := os.Lstat(path); statErr == nil {
		if statErr = safePrivateFile(path); statErr != nil {
			return statErr
		}
	} else if !os.IsNotExist(statErr) {
		return statErr
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	return syncDirectory(m.pkiDir)
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	err = dir.Sync()
	closeErr := dir.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func newRotationRecord() (rotationRecord, error) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return rotationRecord{}, err
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		return rotationRecord{}, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return rotationRecord{}, err
	}
	return rotationRecord{Phase: "requesting", CSR: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})), PrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))}, nil
}

func (m *CredentialManager) pendingPair(record rotationRecord) (tls.Certificate, CertificateInfo, []byte, error) {
	bundle := append([]byte(record.Certificate), []byte(record.PrivateKey)...)
	pair, info, err := parseIdentityBundle(m.nodeID, bundle)
	if err != nil {
		return tls.Certificate{}, CertificateInfo{}, nil, err
	}
	intermediates := x509.NewCertPool()
	for _, raw := range pair.Certificate[1:] {
		certificate, parseErr := x509.ParseCertificate(raw)
		if parseErr != nil {
			return tls.Certificate{}, CertificateInfo{}, nil, parseErr
		}
		intermediates.AddCert(certificate)
	}
	leaf, _ := x509.ParseCertificate(pair.Certificate[0])
	if _, err = leaf.Verify(x509.VerifyOptions{Roots: m.roots, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return tls.Certificate{}, CertificateInfo{}, nil, errors.New("renewed certificate chain invalid")
	}
	return pair, info, bundle, nil
}

func (m *CredentialManager) ReconcileRotation(ctx context.Context, api RotationAPI, now time.Time) error {
	m.rotationMu.Lock()
	defer m.rotationMu.Unlock()
	if api == nil {
		return rotationFailure(m, "renew-unavailable", errors.New("rotation API unavailable"))
	}
	record, err := m.loadRotation()
	if err != nil {
		return rotationFailure(m, "state-invalid", err)
	}
	if record == nil {
		m.mu.RLock()
		forced := m.desiredFingerprint != "" && m.desiredFingerprint == m.info.Fingerprint
		m.mu.RUnlock()
		if !forced && m.Current().NotAfter.After(now.Add(certificateRenewalWindow)) {
			m.setRotationStatus("idle", "")
			return nil
		}
		created, createErr := newRotationRecord()
		if createErr != nil {
			return rotationFailure(m, "state-invalid", createErr)
		}
		if err = m.saveRotation(created); err != nil {
			return rotationFailure(m, "state-invalid", err)
		}
		record = &created
	}
	m.setRotationStatus("pending", "")
	if record.Phase == "requesting" {
		issued, renewErr := api.Renew(ctx, []byte(record.CSR))
		if renewErr != nil {
			return rotationFailure(m, "renew-unavailable", renewErr)
		}
		record.Certificate = string(issued.Certificate)
		record.Phase = "issued"
		_, info, _, certErr := m.pendingPair(*record)
		if certErr != nil || !info.NotAfter.Equal(issued.ExpiresAt) {
			return rotationFailure(m, "certificate-invalid", errors.New("renewed certificate response invalid"))
		}
		if err = m.saveRotation(*record); err != nil {
			return rotationFailure(m, "state-invalid", err)
		}
	}
	pair, _, bundle, err := m.pendingPair(*record)
	if err != nil {
		return rotationFailure(m, "certificate-invalid", err)
	}
	if err = api.Activate(ctx, &pair); err != nil {
		return rotationFailure(m, "activate-unavailable", err)
	}
	if err = m.publishIdentity(bundle); err != nil {
		return rotationFailure(m, "publish-failed", err)
	}
	if err = os.Remove(filepath.Join(m.pkiDir, "rotation.json")); err != nil && !os.IsNotExist(err) {
		return rotationFailure(m, "state-invalid", err)
	}
	if err = syncDirectory(m.pkiDir); err != nil {
		return rotationFailure(m, "state-invalid", err)
	}
	m.setRotationStatus("idle", "")
	return nil
}
