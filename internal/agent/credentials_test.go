package agent

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"reladock/internal/nodeauth"
)

func credentialFixture(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "pki")
	if err := nodeauth.CreateLocalBundle(dir, "hk-01"); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCredentialManagerLoadsCanonicalAndLegacyIdentity(t *testing.T) {
	dir := credentialFixture(t)
	manager, err := OpenCredentialManager(dir, "hk-01")
	if err != nil {
		t.Fatal(err)
	}
	current := manager.Current()
	if current.NodeID != "hk-01" || current.Fingerprint == "" || current.NotAfter.IsZero() {
		t.Fatalf("current identity: %+v", current)
	}
	config, err := manager.TLSConfig()
	if err != nil || config.GetClientCertificate == nil || config.RootCAs == nil {
		t.Fatalf("dynamic TLS config: %+v %v", config, err)
	}
	if _, err = OpenCredentialManager(dir, "sg-02"); err == nil {
		t.Fatal("identity opened under wrong node")
	}
	if err = os.Remove(filepath.Join(dir, "identity.pem")); err != nil {
		t.Fatal(err)
	}
	legacy, err := OpenCredentialManager(dir, "hk-01")
	if err != nil || legacy.Current().Fingerprint != current.Fingerprint {
		t.Fatalf("legacy identity unavailable: %+v %v", legacy.Current(), err)
	}
}

func TestCredentialManagerRejectsUnsafeOrMalformedCanonicalIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{"world readable", func(t *testing.T, dir string) {
			if err := os.Chmod(filepath.Join(dir, "identity.pem"), 0644); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink", func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, "identity.pem")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(dir, "agent.crt"), filepath.Join(dir, "identity.pem")); err != nil {
				t.Fatal(err)
			}
		}},
		{"truncated", func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "identity.pem"), []byte("-----BEGIN CERTIFICATE-----\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"wrong key", func(t *testing.T, dir string) {
			certificate, err := os.ReadFile(filepath.Join(dir, "agent.crt"))
			if err != nil {
				t.Fatal(err)
			}
			_, key, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			der, _ := x509.MarshalPKCS8PrivateKey(key)
			bundle := append(certificate, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})...)
			if err = os.WriteFile(filepath.Join(dir, "identity.pem"), bundle, 0600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := credentialFixture(t)
			tc.mutate(t, dir)
			if _, err := OpenCredentialManager(dir, "hk-01"); err == nil {
				t.Fatal("unsafe canonical identity fell back to legacy files")
			}
		})
	}
}

func TestCredentialManagerFailedPublicationPreservesCurrentIdentity(t *testing.T) {
	dir := credentialFixture(t)
	manager, err := OpenCredentialManager(dir, "hk-01")
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "identity.pem"))
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := manager.Current().Fingerprint
	if err = manager.publishIdentity([]byte("invalid identity bundle")); err == nil {
		t.Fatal("invalid identity published")
	}
	after, err := os.ReadFile(filepath.Join(dir, "identity.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) || manager.Current().Fingerprint != fingerprint {
		t.Fatal("failed publication replaced usable identity")
	}
}
