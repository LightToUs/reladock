package nodeauth

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func managedTestDir(t *testing.T) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(p, "managed")
}

func TestValidateManagedBundleIsReadOnlyAndRejectsMissingRootKey(t *testing.T) {
	dir := managedTestDir(t)
	if err := ValidateManagedBundle(dir, "console.reladock.com"); err == nil {
		t.Fatal("missing identity passed")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("validation created identity")
	}
	if err := EnsureManagedBundle(dir, "console.reladock.com"); err != nil {
		t.Fatal(err)
	}
	before := managedSnapshot(t, dir)
	if err := ValidateManagedBundle(dir, "console.reladock.com"); err != nil {
		t.Fatal(err)
	}
	managedAssertUnchanged(t, dir, before)
	if err := os.Remove(filepath.Join(dir, "root", "ca.key")); err != nil {
		t.Fatal(err)
	}
	if err := ValidateManagedBundle(dir, "console.reladock.com"); err == nil {
		t.Fatal("incomplete root passed check-only validation")
	}
}

func TestValidateManagedBundleNormalizesTrailingSlashBeforeCheckingState(t *testing.T) {
	for _, state := range []string{"lock", "staging"} {
		t.Run(state, func(t *testing.T) {
			dir := managedTestDir(t)
			if err := EnsureManagedBundle(dir, "console.reladock.com"); err != nil {
				t.Fatal(err)
			}
			path := dir + ".lock"
			if state == "staging" {
				path = filepath.Join(filepath.Dir(dir), "."+filepath.Base(dir)+".staging-interrupted")
			}
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
			if err := ValidateManagedBundle(dir+string(filepath.Separator), "console.reladock.com"); err == nil {
				t.Fatal("stale initialization state passed validation")
			}
		})
	}
}

func managedSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		name, _ := filepath.Rel(dir, path)
		result[name] = info.Mode().String()
		if info.Mode().IsRegular() {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			result[name] += string(b)
		} else if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			result[name] += target
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func managedAssertUnchanged(t *testing.T, dir string, before map[string]string) {
	t.Helper()
	after := managedSnapshot(t, dir)
	if len(before) != len(after) {
		t.Fatal("bundle entry count changed")
	}
	for name, b := range before {
		if after[name] != b {
			t.Fatalf("bundle entry changed: %s", name)
		}
	}
}

func TestManagedBundleCreatesAndPreservesTrust(t *testing.T) {
	for _, host := range []string{"controller.example.com", "192.0.2.10", "2001:db8::1"} {
		t.Run(host, func(t *testing.T) {
			dir := managedTestDir(t)
			if err := EnsureManagedBundle(dir, host); err != nil {
				t.Fatal(err)
			}
			before := managedSnapshot(t, dir)
			if len(before) != 12 {
				t.Fatalf("unexpected bundle entries: %d", len(before))
			}
			for name := range before {
				info, err := os.Lstat(filepath.Join(dir, name))
				if err != nil {
					t.Fatal(err)
				}
				want := os.FileMode(0600)
				if info.IsDir() {
					want = 0700
				}
				if info.Mode().Perm() != want {
					t.Fatalf("unsafe permissions: %s", name)
				}
			}
			root, err := ReadCertificate(filepath.Join(dir, "root/ca.crt"))
			if err != nil {
				t.Fatal(err)
			}
			issuer, err := ReadCertificate(filepath.Join(dir, "issuer/ca.crt"))
			if err != nil {
				t.Fatal(err)
			}
			server, err := ReadCertificate(filepath.Join(dir, "server/server.crt"))
			if err != nil {
				t.Fatal(err)
			}
			if root.CheckSignatureFrom(root) != nil || issuer.CheckSignatureFrom(root) != nil || bytes.Equal(root.RawSubjectPublicKeyInfo, issuer.RawSubjectPublicKeyInfo) {
				t.Fatal("root and dedicated issuer invalid")
			}
			for _, c := range []struct {
				cert  *x509.Certificate
				years int
			}{{root, 10}, {issuer, 5}, {server, 1}} {
				if d := c.cert.NotAfter.Sub(time.Now().AddDate(c.years, 0, 0)); d < -time.Minute || d > time.Minute {
					t.Fatal("wrong certificate lifetime")
				}
			}
			roots := x509.NewCertPool()
			roots.AddCert(root)
			if _, err := server.Verify(x509.VerifyOptions{Roots: roots, DNSName: host}); err != nil {
				t.Fatal(err)
			}
			signer, err := LoadBootstrapSigner(filepath.Join(dir, "issuer/ca.crt"), filepath.Join(dir, "issuer/ca.key"), filepath.Join(dir, "issuer/chain.crt"))
			if err != nil {
				t.Fatal(err)
			}
			pair, err := tls.LoadX509KeyPair(filepath.Join(dir, "server/server.crt"), filepath.Join(dir, "server/server.key"))
			if err != nil {
				t.Fatal(err)
			}
			csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, pair.PrivateKey)
			if err != nil {
				t.Fatal(err)
			}
			issued, err := signer.Sign("node-01", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}))
			if err != nil {
				t.Fatal(err)
			}
			block, _ := pem.Decode(issued)
			leaf, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				t.Fatal(err)
			}
			intermediates := x509.NewCertPool()
			intermediates.AddCert(issuer)
			if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
				t.Fatal(err)
			}
			if err := EnsureManagedBundle(dir, host); err != nil {
				t.Fatal(err)
			}
			managedAssertUnchanged(t, dir, before)
			entries, err := os.ReadDir(filepath.Dir(dir))
			if err != nil || len(entries) != 1 {
				t.Fatal("staging or lock leaked after success")
			}
		})
	}
}

func TestManagedBundleRejectsUnsafeHostWithoutWriting(t *testing.T) {
	for _, host := range []string{"", " ", "https://example.com", "example.com:443", "*.example.com", "../host", "host\n", "a..b", "-host", "host_foo", "[::1]", "a\x00b"} {
		dir := managedTestDir(t)
		if err := EnsureManagedBundle(dir, host); err == nil {
			t.Fatalf("accepted unsafe hostname %q", host)
		}
		entries, err := os.ReadDir(filepath.Dir(dir))
		if err != nil || len(entries) != 0 {
			t.Fatal("invalid input wrote state")
		}
	}
}

func TestManagedBundleRejectsExistingUnsafeState(t *testing.T) {
	for _, kind := range []string{"hostname", "partial", "extra", "directory-mode", "file-mode", "file-symlink", "dir-symlink", "wrong-key", "wrong-root", "issuer-chain", "expired-server", "expired-root", "short-issuer"} {
		t.Run(kind, func(t *testing.T) {
			dir := managedTestDir(t)
			if err := EnsureManagedBundle(dir, "controller.example.com"); err != nil {
				t.Fatal(err)
			}
			host := "controller.example.com"
			var err error
			switch kind {
			case "hostname":
				host = "other.example.com"
			case "partial":
				err = os.Remove(filepath.Join(dir, "issuer/ca.key"))
			case "extra":
				err = os.WriteFile(filepath.Join(dir, "unknown"), []byte("untouched"), 0600)
			case "directory-mode":
				err = os.Chmod(filepath.Join(dir, "root"), 0755)
			case "file-mode":
				err = os.Chmod(filepath.Join(dir, "server/server.crt"), 0644)
			case "file-symlink":
				p := filepath.Join(dir, "server/server.key")
				if err = os.Remove(p); err == nil {
					err = os.Symlink("../root/ca.key", p)
				}
			case "dir-symlink":
				p := filepath.Join(dir, "server")
				if err = os.RemoveAll(p); err == nil {
					err = os.Symlink("root", p)
				}
			case "wrong-key":
				b, e := os.ReadFile(filepath.Join(dir, "root/ca.key"))
				if e != nil {
					t.Fatal(e)
				}
				err = os.WriteFile(filepath.Join(dir, "issuer/ca.key"), b, 0600)
			case "wrong-root":
				other := managedTestDir(t)
				if e := EnsureManagedBundle(other, host); e != nil {
					t.Fatal(e)
				}
				for _, name := range []string{"ca.crt", "ca.key"} {
					b, e := os.ReadFile(filepath.Join(other, "root", name))
					if e != nil {
						t.Fatal(e)
					}
					if e = os.WriteFile(filepath.Join(dir, "root", name), b, 0600); e != nil {
						t.Fatal(e)
					}
				}
			case "issuer-chain":
				b, e := os.ReadFile(filepath.Join(dir, "issuer/ca.crt"))
				if e != nil {
					t.Fatal(e)
				}
				err = os.WriteFile(filepath.Join(dir, "issuer/chain.crt"), b, 0600)
			case "expired-server":
				managedResign(t, dir, "server/server.crt", time.Now().Add(-time.Hour))
			case "expired-root":
				managedResign(t, dir, "root/ca.crt", time.Now().Add(-time.Hour))
			case "short-issuer":
				managedResign(t, dir, "issuer/ca.crt", time.Now().Add(31*24*time.Hour))
			}
			if err != nil {
				t.Fatal(err)
			}
			before := managedSnapshot(t, dir)
			err = EnsureManagedBundle(dir, host)
			if err == nil {
				t.Fatal("unsafe bundle accepted")
			}
			if strings.Contains(err.Error(), "PRIVATE KEY") {
				t.Fatal("error exposed private key")
			}
			managedAssertUnchanged(t, dir, before)
		})
	}
}

func managedResign(t *testing.T, dir, certName string, expiry time.Time) {
	t.Helper()
	cert, err := ReadCertificate(filepath.Join(dir, certName))
	if err != nil {
		t.Fatal(err)
	}
	root, err := ReadCertificate(filepath.Join(dir, "root/ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.LoadX509KeyPair(filepath.Join(dir, "root/ca.crt"), filepath.Join(dir, "root/ca.key"))
	if err != nil {
		t.Fatal(err)
	}
	cert.NotBefore = time.Now().Add(-48 * time.Hour)
	cert.NotAfter = expiry
	der, err := x509.CreateCertificate(rand.Reader, cert, root, cert.PublicKey, pair.PrivateKey.(crypto.Signer))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, certName), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestManagedBundleRefusesEmptyDirectoryAndStaleLock(t *testing.T) {
	for _, kind := range []string{"empty", "lock", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := managedTestDir(t)
			target := dir
			if kind == "lock" {
				target = dir + ".lock"
			}
			if kind == "symlink" {
				if err := os.Symlink("missing", target); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Mkdir(target, 0700); err != nil {
				t.Fatal(err)
			}
			before := managedSnapshot(t, filepath.Dir(dir))
			if err := EnsureManagedBundle(dir, "controller.example.com"); err == nil {
				t.Fatal("unknown state accepted")
			}
			managedAssertUnchanged(t, filepath.Dir(dir), before)
		})
	}
}

// Catch regression to ordinary os.Rename, which replaces existing empty dirs.
func TestManagedPublicationNeverReplacesExistingDestination(t *testing.T) {
	for _, kind := range []string{"empty-directory", "file", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := managedTestDir(t)
			stage := filepath.Join(filepath.Dir(dir), "stage")
			if err := os.Mkdir(stage, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(stage, "sentinel"), []byte("staged"), 0600); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "empty-directory":
				err = os.Mkdir(dir, 0700)
			case "file":
				err = os.WriteFile(dir, []byte("existing"), 0600)
			case "symlink":
				err = os.Symlink("missing", dir)
			}
			if err != nil {
				t.Fatal(err)
			}
			before := managedSnapshot(t, filepath.Dir(dir))
			if err := managedPublish(stage, dir); err == nil {
				t.Fatal("publication overwrote existing state")
			}
			managedAssertUnchanged(t, filepath.Dir(dir), before)
		})
	}
}

func TestManagedBundlePreservesUnknownStagingAndParent(t *testing.T) {
	for _, kind := range []string{"staging", "parent-symlink", "parent-writable", "bundle-permissions"} {
		t.Run(kind, func(t *testing.T) {
			dir := managedTestDir(t)
			parent := filepath.Dir(dir)
			switch kind {
			case "staging":
				stale := filepath.Join(parent, ".managed.staging-unknown")
				if err := os.Mkdir(stale, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(stale, "unknown.key"), []byte("operator-owned"), 0600); err != nil {
					t.Fatal(err)
				}
			case "parent-symlink":
				alias := filepath.Join(parent, "alias")
				if err := os.Symlink(parent, alias); err != nil {
					t.Fatal(err)
				}
				dir = filepath.Join(alias, "managed")
			case "parent-writable":
				if err := os.Chmod(parent, 0777); err != nil {
					t.Fatal(err)
				}
			case "bundle-permissions":
				if err := EnsureManagedBundle(dir, "example.com"); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(dir, 0755); err != nil {
					t.Fatal(err)
				}
			}
			before := managedSnapshot(t, parent)
			if err := EnsureManagedBundle(dir, "example.com"); err == nil {
				t.Fatal("unknown state accepted")
			}
			managedAssertUnchanged(t, parent, before)
		})
	}
}

func TestManagedBundleConcurrentInitializationPreservesWinner(t *testing.T) {
	dir := managedTestDir(t)
	start := make(chan struct{})
	results := make(chan error, 12)
	for i := 0; i < cap(results); i++ {
		go func() { <-start; results <- EnsureManagedBundle(dir, "example.com") }()
	}
	close(start)
	successes := 0
	for i := 0; i < cap(results); i++ {
		if <-results == nil {
			successes++
		}
	}
	if successes == 0 {
		t.Fatal("no initializer succeeded")
	}
	before := managedSnapshot(t, dir)
	if err := EnsureManagedBundle(dir, "example.com"); err != nil {
		t.Fatal(err)
	}
	managedAssertUnchanged(t, dir, before)
	entries, err := os.ReadDir(filepath.Dir(dir))
	if err != nil || len(entries) != 1 {
		t.Fatal("initialization left extra state")
	}
}
