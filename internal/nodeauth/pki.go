package nodeauth

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var idPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,63}$`)

func NodeIdentity(cert *x509.Certificate) (string, error) {
	if cert == nil || len(cert.URIs) != 1 {
		return "", errors.New("missing node identity")
	}
	uri := cert.URIs[0]
	if !strings.HasPrefix(uri.Path, "/nodes/") {
		return "", errors.New("invalid node identity")
	}
	id := strings.TrimPrefix(uri.Path, "/nodes/")
	if uri.Scheme != "spiffe" || uri.Host != "reladock" || uri.RawQuery != "" || uri.Fragment != "" || uri.User != nil || uri.Path != "/nodes/"+id || !idPattern.MatchString(id) {
		return "", errors.New("invalid node identity")
	}
	return id, nil
}
func Fingerprint(cert *x509.Certificate) string {
	hash := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(hash[:])
}
func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		panic(err)
	}
	return n
}

// Local development credentials only. The directory must not already exist.
// Production enrollment must use an operator-managed CA and certificate lifecycle.
func CreateLocalBundle(dir, nodeID string) error {
	if !idPattern.MatchString(nodeID) {
		return errors.New("invalid node id")
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		return err
	}
	now := time.Now()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	ca := &x509.Certificate{SerialNumber: serial(), Subject: pkix.Name{CommonName: "RelaDock local development CA"}, NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(1, 0, 0), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, pub, key)
	if err != nil {
		return err
	}
	write := func(name, kind string, data []byte) error {
		file, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, err = file.Write(pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: data}))
		closeErr := file.Close()
		if err != nil {
			return err
		}
		return closeErr
	}
	caKey, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err = write("ca.key", "PRIVATE KEY", caKey); err != nil {
		return err
	}
	if err = write("ca.crt", "CERTIFICATE", caDER); err != nil {
		return err
	}
	for _, kind := range []string{"server", "agent"} {
		public, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		cert := &x509.Certificate{SerialNumber: serial(), Subject: pkix.Name{CommonName: "RelaDock " + kind}, NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(0, 3, 0), KeyUsage: x509.KeyUsageDigitalSignature}
		if kind == "server" {
			cert.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
			cert.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		} else {
			uri, _ := url.Parse("spiffe://reladock/nodes/" + nodeID)
			cert.URIs = []*url.URL{uri}
			cert.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		}
		der, err := x509.CreateCertificate(rand.Reader, cert, ca, public, key)
		if err != nil {
			return err
		}
		privateDER, err := x509.MarshalPKCS8PrivateKey(private)
		if err != nil {
			return err
		}
		if err = write(kind+".key", "PRIVATE KEY", privateDER); err != nil {
			return err
		}
		if err = write(kind+".crt", "CERTIFICATE", der); err != nil {
			return err
		}
	}
	return nil
}
func ReadCertificate(path string) (*x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("invalid certificate PEM")
	}
	return x509.ParseCertificate(block.Bytes)
}
func ServerTLS(caFile, certFile, keyFile string) (*tls.Config, error) {
	data, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(data) {
		return nil, errors.New("invalid CA")
	}
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots, Certificates: []tls.Certificate{pair}}, nil
}
func ClientTLS(caFile, certFile, keyFile string) (*tls.Config, error) {
	data, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(data) {
		return nil, errors.New("invalid CA")
	}
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{pair}}, nil
}
