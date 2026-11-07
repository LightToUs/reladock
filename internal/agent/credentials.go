package agent

import (
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"reladock/internal/nodeauth"
)

type CertificateInfo struct {
	NodeID      string
	Fingerprint string
	NotAfter    time.Time
}

type CredentialManager struct {
	rotationMu         sync.Mutex
	rotationRequested  chan struct{}
	desiredFingerprint string
	mu                 sync.RWMutex
	pkiDir             string
	nodeID             string
	roots              *x509.CertPool
	current            tls.Certificate
	info               CertificateInfo
	transports         []*http.Transport
	rotationState      string
	rotationErrorCode  string
}

func safePrivateFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return errors.New("unsafe private credential file")
	}
	return nil
}

func validateClientPair(nodeID string, pair tls.Certificate) (CertificateInfo, error) {
	if len(pair.Certificate) == 0 {
		return CertificateInfo{}, errors.New("missing client certificate")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil || leaf.IsCA || time.Now().Before(leaf.NotBefore) || !time.Now().Before(leaf.NotAfter) {
		return CertificateInfo{}, errors.New("invalid client certificate")
	}
	id, err := nodeauth.NodeIdentity(leaf)
	if err != nil || id != nodeID {
		return CertificateInfo{}, errors.New("client certificate identity mismatch")
	}
	clientAuth := false
	for _, usage := range leaf.ExtKeyUsage {
		if usage == x509.ExtKeyUsageClientAuth {
			clientAuth = true
		}
	}
	if !clientAuth {
		return CertificateInfo{}, errors.New("client certificate lacks client authentication usage")
	}
	pair.Leaf = leaf
	return CertificateInfo{NodeID: id, Fingerprint: nodeauth.Fingerprint(leaf), NotAfter: leaf.NotAfter}, nil
}

func parseIdentityBundle(nodeID string, bundle []byte) (tls.Certificate, CertificateInfo, error) {
	pair, err := tls.X509KeyPair(bundle, bundle)
	if err != nil {
		return tls.Certificate{}, CertificateInfo{}, errors.New("invalid identity bundle")
	}
	info, err := validateClientPair(nodeID, pair)
	if err != nil {
		return tls.Certificate{}, CertificateInfo{}, err
	}
	pair.Leaf, _ = x509.ParseCertificate(pair.Certificate[0])
	return pair, info, nil
}

func OpenCredentialManager(pkiDir, nodeID string) (*CredentialManager, error) {
	dirInfo, err := os.Lstat(pkiDir)
	if err != nil || !dirInfo.IsDir() || dirInfo.Mode().Perm() != 0700 {
		return nil, errors.New("unsafe private PKI directory")
	}
	caPath := filepath.Join(pkiDir, "ca.crt")
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("invalid CA bundle")
	}
	identityPath := filepath.Join(pkiDir, "identity.pem")
	var bundle []byte
	if _, err = os.Lstat(identityPath); err == nil {
		if err = safePrivateFile(identityPath); err != nil {
			return nil, err
		}
		bundle, err = os.ReadFile(identityPath)
	} else if os.IsNotExist(err) {
		certPath := filepath.Join(pkiDir, "agent.crt")
		keyPath := filepath.Join(pkiDir, "agent.key")
		if err = safePrivateFile(certPath); err != nil {
			return nil, err
		}
		if err = safePrivateFile(keyPath); err != nil {
			return nil, err
		}
		certificate, certErr := os.ReadFile(certPath)
		key, keyErr := os.ReadFile(keyPath)
		if certErr != nil || keyErr != nil {
			return nil, errors.New("legacy identity unavailable")
		}
		bundle = append(append([]byte{}, certificate...), key...)
	} else {
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	pair, info, err := parseIdentityBundle(nodeID, bundle)
	if err != nil {
		return nil, err
	}
	return &CredentialManager{pkiDir: pkiDir, nodeID: nodeID, roots: roots, current: pair, info: info, rotationRequested: make(chan struct{}, 1)}, nil
}

func (m *CredentialManager) TLSConfig() (*tls.Config, error) {
	if m == nil || m.roots == nil {
		return nil, errors.New("credential manager unavailable")
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: m.roots, GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
		m.mu.RLock()
		defer m.mu.RUnlock()
		pair := m.current
		return &pair, nil
	}}, nil
}

func (m *CredentialManager) Current() CertificateInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.info
}

func (m *CredentialManager) RegisterTransport(transport *http.Transport) {
	if transport == nil {
		return
	}
	m.mu.Lock()
	m.transports = append(m.transports, transport)
	m.mu.Unlock()
}

func (m *CredentialManager) CloseIdleConnections() {
	m.mu.RLock()
	transports := append([]*http.Transport(nil), m.transports...)
	m.mu.RUnlock()
	for _, transport := range transports {
		transport.CloseIdleConnections()
	}
}

func (m *CredentialManager) publishIdentity(bundle []byte) error {
	pair, info, err := parseIdentityBundle(m.nodeID, bundle)
	if err != nil {
		return err
	}
	target := filepath.Join(m.pkiDir, "identity.pem")
	if _, err = os.Lstat(target); err == nil {
		if err = safePrivateFile(target); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	temp, err := os.CreateTemp(m.pkiDir, ".identity-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err = temp.Chmod(0600); err == nil {
		_, err = temp.Write(bundle)
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
	if err = os.Rename(tempPath, target); err != nil {
		return err
	}
	dir, err := os.Open(m.pkiDir)
	if err != nil {
		return err
	}
	err = dir.Sync()
	closeErr = dir.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	m.mu.Lock()
	m.current = pair
	m.info = info
	m.mu.Unlock()
	m.CloseIdleConnections()
	return nil
}

func identityBundle(certificate []byte, privateKey ed25519.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, err
	}
	return append(append([]byte{}, certificate...), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})...), nil
}
