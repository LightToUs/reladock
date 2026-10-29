package nodeauth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"net/url"
	"os"
	"time"
)

type BootstrapCA struct {
	certificate *x509.Certificate
	key         crypto.Signer
	Chain       []byte
}

func LoadBootstrapSigner(certFile, keyFile, chainFile string) (*BootstrapCA, error) {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, errors.New("invalid bootstrap CA key pair")
	}
	ca, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil || !ca.IsCA || !ca.BasicConstraintsValid || ca.KeyUsage&x509.KeyUsageCertSign == 0 || time.Now().Before(ca.NotBefore) || time.Until(ca.NotAfter) < 31*24*time.Hour {
		return nil, errors.New("bootstrap signer must be a valid CA with at least 31 days remaining")
	}
	key, ok := pair.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, errors.New("unsupported CA key")
	}
	chain, err := os.ReadFile(chainFile)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(chain) {
		return nil, errors.New("invalid bootstrap CA chain")
	}
	if _, err = ca.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		return nil, errors.New("bootstrap CA not trusted by configured chain")
	}
	return &BootstrapCA{certificate: ca, key: key, Chain: chain}, nil
}

func (s *BootstrapCA) Sign(id string, raw []byte) ([]byte, error) {
	if !idPattern.MatchString(id) || len(raw) > 8192 {
		return nil, errors.New("invalid CSR")
	}
	block, rest := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE REQUEST" || len(rest) != 0 {
		return nil, errors.New("invalid CSR")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil || csr.CheckSignature() != nil {
		return nil, errors.New("invalid CSR signature")
	}
	switch k := csr.PublicKey.(type) {
	case ed25519.PublicKey:
	case *ecdsa.PublicKey:
		if k.Curve.Params().BitSize < 256 {
			return nil, errors.New("weak public key")
		}
	case *rsa.PublicKey:
		if k.N.BitLen() < 2048 {
			return nil, errors.New("weak public key")
		}
	default:
		return nil, errors.New("unsupported public key")
	}
	// Never copy CSR names, SANs, key usages, extensions or CA requests.
	uri, _ := url.Parse("spiffe://reladock/nodes/" + id)
	now := time.Now()
	cert := &x509.Certificate{SerialNumber: serial(), Subject: pkix.Name{CommonName: "RelaDock node " + id}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(30 * 24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, URIs: []*url.URL{uri}, BasicConstraintsValid: true}
	if cert.NotAfter.After(s.certificate.NotAfter) {
		return nil, errors.New("bootstrap CA expires too soon")
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, s.certificate, csr.PublicKey, s.key)
	if err != nil {
		return nil, err
	}
	result := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	// Include the issuing intermediate so mTLS can build a chain to the root.
	return append(result, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.certificate.Raw})...), nil
}
