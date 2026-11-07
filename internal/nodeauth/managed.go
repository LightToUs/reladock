package nodeauth

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// ValidateManagedBundle checks the complete identity without creating files or locks.
func ValidateManagedBundle(dir, hostname string) error {
	if !managedHostname(hostname) {
		return errors.New("invalid managed PKI hostname")
	}
	if dir == "" || strings.TrimSpace(dir) != dir {
		return errors.New("invalid managed PKI directory")
	}
	var err error
	dir, err = filepath.Abs(dir)
	if err != nil {
		return err
	}
	if dir == filepath.Dir(dir) {
		return errors.New("managed PKI directory cannot be a filesystem root")
	}
	for _, path := range []string{dir + ".lock"} {
		if _, err := os.Lstat(path); err == nil {
			return errors.New("managed PKI initialization lock exists")
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	parent := filepath.Dir(dir)
	for p := parent; ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || p == parent && info.Mode().Perm()&0022 != 0 {
			return errors.New("unsafe managed PKI parent")
		}
		if p == filepath.Dir(p) {
			break
		}
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "."+filepath.Base(dir)+".staging-") {
			return errors.New("managed PKI staging state exists")
		}
	}
	return managedValidate(dir, hostname)
}

// EnsureManagedBundle provisions once and reuses only a complete, private bundle.
// Existing state is never repaired, renewed, or overwritten implicitly.
func EnsureManagedBundle(dir, hostname string) (err error) {
	if !managedHostname(hostname) {
		return errors.New("invalid managed PKI hostname: expected a DNS name or unbracketed IP address")
	}
	if dir == "" || strings.TrimSpace(dir) != dir {
		return errors.New("invalid managed PKI directory")
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return err
	}
	parent := filepath.Dir(dir)
	if dir == parent {
		return errors.New("managed PKI directory cannot be a filesystem root")
	}
	// Reject symlink components rather than resolving an operator's unknown state.
	for p := parent; ; p = filepath.Dir(p) {
		info, e := os.Lstat(p)
		if e != nil {
			return e
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("managed PKI parent path must contain only real directories")
		}
		if p == parent && info.Mode().Perm()&0022 != 0 {
			return errors.New("managed PKI parent is writable by other users")
		}
		if p == filepath.Dir(p) {
			break
		}
	}
	lock := dir + ".lock"
	if err = os.Mkdir(lock, 0700); err != nil {
		return fmt.Errorf("managed PKI lock unavailable; investigate existing locks without deleting unknown state: %w", err)
	}
	// Only this invocation's lock can be removed. A process crash leaves it intact.
	defer func() { err = errors.Join(err, os.Remove(lock)) }()
	if err = managedSyncDir(parent); err != nil {
		return err
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		return err
	}
	prefix := "." + filepath.Base(dir) + ".staging-"
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) {
			return errors.New("managed PKI staging state exists; operator investigation required")
		}
	}
	if _, err = os.Lstat(dir); err == nil {
		return managedValidate(dir, hostname)
	} else if !os.IsNotExist(err) {
		return err
	}
	stage, err := os.MkdirTemp(parent, prefix)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(stage)) }()
	if err = managedGenerate(stage, hostname, time.Now()); err != nil {
		return err
	}
	if err = managedValidate(stage, hostname); err != nil {
		return err
	}
	// RENAME_NOREPLACE/RENAME_EXCL is essential: ordinary Rename would silently
	// replace an empty destination created after the initial existence check.
	if err = managedPublish(stage, dir); err != nil {
		return err
	}
	// Never remove the published bundle, even if the durability barrier fails.
	return managedSyncDir(parent)
}

func managedHostname(host string) bool {
	if net.ParseIP(host) != nil {
		return true
	}
	if len(host) == 0 || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

func managedGenerate(dir, host string, now time.Time) error {
	for _, name := range []string{"root", "issuer", "server"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0700); err != nil {
			return err
		}
	}
	rootPublic, rootKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	issuerPublic, issuerKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	serverPublic, serverKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	root := &x509.Certificate{SerialNumber: serial(), Subject: pkix.Name{CommonName: "RelaDock managed root CA"}, NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(10, 0, 0), IsCA: true, BasicConstraintsValid: true, MaxPathLen: 1, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	issuer := &x509.Certificate{SerialNumber: serial(), Subject: pkix.Name{CommonName: "RelaDock managed node issuer"}, NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(5, 0, 0), IsCA: true, BasicConstraintsValid: true, MaxPathLen: 0, MaxPathLenZero: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	server := &x509.Certificate{SerialNumber: serial(), Subject: pkix.Name{CommonName: "RelaDock managed controller"}, NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(1, 0, 0), BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	if ip := net.ParseIP(host); ip != nil {
		server.IPAddresses = []net.IP{ip}
	} else {
		server.DNSNames = []string{host}
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, root, root, rootPublic, rootKey)
	if err != nil {
		return err
	}
	// Parse the root so the children's AuthorityKeyId comes from its generated SKI.
	root, err = x509.ParseCertificate(rootDER)
	if err != nil {
		return err
	}
	issuerDER, err := x509.CreateCertificate(rand.Reader, issuer, root, issuerPublic, rootKey)
	if err != nil {
		return err
	}
	// The online intermediate is dedicated to node enrollment. Server TLS is
	// signed directly by the durable root, independently of the node signer.
	serverDER, err := x509.CreateCertificate(rand.Reader, server, root, serverPublic, rootKey)
	if err != nil {
		return err
	}
	certPEM := func(der []byte) []byte { return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}) }
	files := map[string][]byte{"root/ca.crt": certPEM(rootDER), "issuer/ca.crt": certPEM(issuerDER), "issuer/chain.crt": certPEM(rootDER), "server/ca.crt": certPEM(rootDER), "server/server.crt": certPEM(serverDER)}
	for name, key := range map[string]ed25519.PrivateKey{"root/ca.key": rootKey, "issuer/ca.key": issuerKey, "server/server.key": serverKey} {
		der, e := x509.MarshalPKCS8PrivateKey(key)
		if e != nil {
			return e
		}
		files[name] = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	}
	for name, data := range files {
		if err := managedWrite(filepath.Join(dir, name), data); err != nil {
			return err
		}
	}
	for _, name := range []string{"root", "issuer", "server", "."} {
		if err := managedSyncDir(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}

func managedWrite(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	return errors.Join(writeErr, syncErr, f.Close())
}

func managedSyncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

func managedValidate(dir, host string) error {
	expected := map[string]bool{".": true, "root": true, "issuer": true, "server": true, "root/ca.crt": false, "root/ca.key": false, "issuer/ca.crt": false, "issuer/ca.key": false, "issuer/chain.crt": false, "server/ca.crt": false, "server/server.crt": false, "server/server.key": false}
	seen := 0
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		directory, known := expected[relative]
		if !known {
			return errors.New("unknown entry in managed PKI bundle")
		}
		if directory {
			if info.Mode() != os.ModeDir|0700 {
				return errors.New("managed PKI directories must be real directories with mode 0700")
			}
		} else if info.Mode() != 0600 || info.Size() > 64*1024 {
			return errors.New("managed PKI files must be regular files with mode 0600 and bounded size")
		}
		seen++
		return nil
	})
	if err != nil {
		return err
	}
	if seen != len(expected) {
		return errors.New("managed PKI bundle is incomplete")
	}
	read := func(name string) ([]byte, error) { return os.ReadFile(filepath.Join(dir, name)) }
	pair := func(certName, keyName string) (*x509.Certificate, error) {
		certPEM, err := read(certName)
		if err != nil {
			return nil, err
		}
		keyPEM, err := read(keyName)
		if err != nil {
			return nil, err
		}
		cb, rest := pem.Decode(certPEM)
		if cb == nil || cb.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
			return nil, errors.New("invalid managed certificate encoding")
		}
		kb, rest := pem.Decode(keyPEM)
		if kb == nil || kb.Type != "PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
			return nil, errors.New("invalid managed private key encoding")
		}
		p, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return nil, errors.New("managed certificate and private key do not match")
		}
		return x509.ParseCertificate(p.Certificate[0])
	}
	root, err := pair("root/ca.crt", "root/ca.key")
	if err != nil {
		return err
	}
	issuer, err := pair("issuer/ca.crt", "issuer/ca.key")
	if err != nil {
		return err
	}
	server, err := pair("server/server.crt", "server/server.key")
	if err != nil {
		return err
	}
	now := time.Now()
	validCA := func(c *x509.Certificate) bool {
		return c.IsCA && c.BasicConstraintsValid && c.KeyUsage&x509.KeyUsageCertSign != 0 && !now.Before(c.NotBefore) && now.Before(c.NotAfter)
	}
	if !validCA(root) || root.CheckSignatureFrom(root) != nil || !bytes.Equal(root.RawSubject, root.RawIssuer) {
		return errors.New("invalid managed root CA")
	}
	if !validCA(issuer) || !issuer.NotAfter.After(now.Add(31*24*time.Hour)) || issuer.CheckSignatureFrom(root) != nil || issuer.NotAfter.After(root.NotAfter) {
		return errors.New("invalid managed issuer or fewer than 31 days remaining")
	}
	if bytes.Equal(root.RawSubjectPublicKeyInfo, issuer.RawSubjectPublicKeyInfo) || bytes.Equal(root.RawSubjectPublicKeyInfo, server.RawSubjectPublicKeyInfo) || bytes.Equal(issuer.RawSubjectPublicKeyInfo, server.RawSubjectPublicKeyInfo) {
		return errors.New("managed PKI roles must use distinct keys")
	}
	rootPEM, err := read("root/ca.crt")
	if err != nil {
		return err
	}
	for _, name := range []string{"issuer/chain.crt", "server/ca.crt"} {
		data, err := read(name)
		if err != nil {
			return err
		}
		if !bytes.Equal(data, rootPEM) {
			return errors.New("managed trust chain differs from root CA")
		}
	}
	roots := x509.NewCertPool()
	roots.AddCert(root)
	if _, err = issuer.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		return errors.New("managed issuer chain verification failed")
	}
	if server.IsCA || server.NotAfter.After(root.NotAfter) || server.CheckSignatureFrom(root) != nil {
		return errors.New("invalid managed server certificate")
	}
	// Require exactly the requested SAN, not a wildcard or a broader identity.
	if ip := net.ParseIP(host); ip != nil {
		if len(server.IPAddresses) != 1 || !ip.Equal(server.IPAddresses[0]) || len(server.DNSNames) != 0 {
			return errors.New("managed server IP SAN mismatch")
		}
	} else if len(server.DNSNames) != 1 || !strings.EqualFold(server.DNSNames[0], host) || len(server.IPAddresses) != 0 {
		return errors.New("managed server DNS SAN mismatch")
	}
	if len(server.URIs) != 0 || len(server.EmailAddresses) != 0 {
		return errors.New("unexpected managed server identity")
	}
	if _, err = server.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: now, DNSName: host, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		return errors.New("managed server chain or validity verification failed")
	}
	return nil
}

// managedPublish uses the native atomic no-replace rename. Keep these ABI values
// local because syscall is frozen and lacks renameat2/renameatx_np wrappers.
// Supported deployment targets are Linux amd64/arm64 and macOS amd64/arm64;
// unknown platforms fail closed rather than falling back to unsafe Rename.
func managedPublish(from, to string) error {
	var trap, flags uintptr
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "linux/amd64":
		trap, flags = 316, 1 // renameat2, RENAME_NOREPLACE
	case "linux/arm64":
		trap, flags = 276, 1
	case "darwin/amd64", "darwin/arm64":
		trap, flags = 488, 4 // renameatx_np, RENAME_EXCL
	default:
		return errors.New("atomic managed PKI publication is unsupported on this platform")
	}
	// Directory descriptors keep both names anchored in the same parent.
	if filepath.Dir(from) != filepath.Dir(to) {
		return errors.New("managed PKI publication must stay in one directory")
	}
	parent, err := os.Open(filepath.Dir(to))
	if err != nil {
		return err
	}
	defer parent.Close()
	old, err := syscall.BytePtrFromString(filepath.Base(from))
	if err != nil {
		return err
	}
	next, err := syscall.BytePtrFromString(filepath.Base(to))
	if err != nil {
		return err
	}
	_, _, errno := syscall.Syscall6(trap, parent.Fd(), uintptr(unsafe.Pointer(old)), parent.Fd(), uintptr(unsafe.Pointer(next)), flags, 0)
	runtime.KeepAlive(old)
	runtime.KeepAlive(next)
	runtime.KeepAlive(parent)
	if errno != 0 {
		return fmt.Errorf("publish managed PKI without replacing existing state: %w", errno)
	}
	return nil
}
