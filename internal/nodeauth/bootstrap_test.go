package nodeauth

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

func TestBootstrapSignerScopesIdentityAndRejectsBadCSR(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pki")
	if err := CreateLocalBundle(dir, "hk-01"); err != nil {
		t.Fatal(err)
	}
	signer, err := LoadBootstrapSigner(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key"), filepath.Join(dir, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	csr, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	issued, err := signer.Sign("hk-02", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := pem.Decode(issued)
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	id, err := NodeIdentity(c)
	if err != nil || id != "hk-02" || c.IsCA || len(c.ExtKeyUsage) != 1 || c.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Fatal("unscoped certificate")
	}
	if _, err = signer.Sign("hk-02", []byte("bad")); err == nil {
		t.Fatal("bad CSR accepted")
	}
	if _, err = LoadBootstrapSigner(filepath.Join(dir, "agent.crt"), filepath.Join(dir, "agent.key"), filepath.Join(dir, "ca.crt")); err == nil {
		t.Fatal("leaf signer accepted")
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if string(signer.Chain) != string(raw) {
		t.Fatal("wrong trust chain")
	}
}
