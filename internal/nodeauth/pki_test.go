package nodeauth

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalBundleHasScopedClientIdentityAndRefusesOverwrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pki")
	if err := CreateLocalBundle(dir, "hk-01"); err != nil {
		t.Fatal(err)
	}
	pair, err := tls.LoadX509KeyPair(filepath.Join(dir, "agent.crt"), filepath.Join(dir, "agent.key"))
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	id, err := NodeIdentity(cert)
	if err != nil || id != "hk-01" {
		t.Fatalf("identity %q: %v", id, err)
	}
	caPEM, _ := os.ReadFile(filepath.Join(dir, "ca.crt"))
	block, _ := pem.Decode(caPEM)
	ca, _ := x509.ParseCertificate(block.Bytes)
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	if _, err = cert.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatal(err)
	}
	original, _ := os.ReadFile(filepath.Join(dir, "agent.key"))
	if err = CreateLocalBundle(dir, "hk-01"); err == nil {
		t.Fatal("silently overwrote credentials")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "agent.key"))
	if string(original) != string(after) {
		t.Fatal("key changed")
	}
	info, _ := os.Stat(filepath.Join(dir, "agent.key"))
	if info.Mode().Perm() != 0600 {
		t.Fatal("private key is not private")
	}
}
