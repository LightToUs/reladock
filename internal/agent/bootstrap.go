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
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reladock/internal/nodeauth"
	"strings"
	"time"
)

type BootstrapOptions struct {
	Origin, PKIDir, EnvFile string
	Token                   io.Reader
	HTTP                    *http.Client
}
type enrollmentPending struct {
	Origin string `json:"origin"`
	CSR    string `json:"csr"`
}

func privateWrite(path string, b []byte) error {
	// Publish only a complete synced file. Link is atomic and refuses an existing
	// destination, unlike Rename, so retries cannot replace credentials.
	f, e := os.CreateTemp(filepath.Dir(path), ".enroll-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	_, e = f.Write(b)
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	if e = os.Link(f.Name(), path); e != nil {
		return e
	}
	dir, e := os.Open(filepath.Dir(path))
	if e != nil {
		return e
	}
	defer dir.Close()
	return dir.Sync()
}
func Bootstrap(ctx context.Context, o BootstrapOptions) error {
	u, e := url.Parse(o.Origin)
	if e != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return errors.New("bootstrap requires exact HTTPS origin")
	}
	if !filepath.IsAbs(o.PKIDir) || o.Token == nil {
		return errors.New("absolute private PKI directory and token input required")
	}
	pending := enrollmentPending{Origin: o.Origin}
	keyPath := filepath.Join(o.PKIDir, "agent.key")
	pendingPath := filepath.Join(o.PKIDir, "enrollment.json")
	if _, e = os.Lstat(filepath.Join(o.PKIDir, "agent.crt")); e == nil {
		return errors.New("already enrolled; refusing credential overwrite")
	}
	if e = os.Mkdir(o.PKIDir, 0700); e != nil {
		if !os.IsExist(e) {
			return e
		}
		info, err := os.Lstat(o.PKIDir)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
			return errors.New("unsafe PKI directory")
		}
		b, err := os.ReadFile(pendingPath)
		if err != nil || json.Unmarshal(b, &pending) != nil || pending.Origin != o.Origin || pending.CSR == "" {
			return errors.New("existing PKI is not a resumable enrollment")
		}
	} else {
		_, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		der, _ := x509.MarshalPKCS8PrivateKey(key)
		if err = privateWrite(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})); err != nil {
			return err
		}
		csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
		if err != nil {
			return err
		}
		pending.CSR = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}))
		b, _ := json.Marshal(pending)
		if err = privateWrite(pendingPath, b); err != nil {
			return err
		}
	}
	keyInfo, e := os.Lstat(keyPath)
	if e != nil || !keyInfo.Mode().IsRegular() || keyInfo.Mode().Perm() != 0600 {
		return errors.New("unsafe private key")
	}
	rawToken, e := io.ReadAll(io.LimitReader(o.Token, 66))
	token := strings.TrimSpace(string(rawToken))
	if e != nil || len(token) != 64 || strings.Trim(token, "0123456789abcdef") != "" {
		return errors.New("invalid bootstrap token")
	}
	body, _ := json.Marshal(map[string]string{"csr": pending.CSR})
	req, e := http.NewRequestWithContext(ctx, "POST", o.Origin+"/api/node-bootstrap/redeem", bytes.NewReader(body))
	if e != nil {
		return e
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	client := o.HTTP
	if client == nil {
		client = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13}}, Timeout: 20 * time.Second}
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, e := copyClient.Do(req)
	if e != nil {
		return errors.New("bootstrap HTTPS connection failed; retained local enrollment for retry")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return errors.New("bootstrap rejected; check token expiry/revocation and retry with retained enrollment")
	}
	var out struct {
		NodeID      string    `json:"nodeId"`
		Certificate string    `json:"certificate"`
		CA          string    `json:"ca"`
		Control     string    `json:"control"`
		ExpiresAt   time.Time `json:"expiresAt"`
	}
	d := json.NewDecoder(io.LimitReader(response.Body, 65537))
	d.DisallowUnknownFields()
	if e = d.Decode(&out); e != nil {
		return errors.New("invalid bootstrap response")
	}
	if e = ValidateEndpoint(out.Control, out.NodeID); e != nil {
		return e
	}
	keyPEM, e := os.ReadFile(keyPath)
	if e != nil {
		return e
	}
	pair, e := tls.X509KeyPair([]byte(out.Certificate), keyPEM)
	if e != nil {
		return errors.New("issued certificate does not match local key")
	}
	cert, e := x509.ParseCertificate(pair.Certificate[0])
	if e != nil {
		return e
	}
	id, e := nodeauth.NodeIdentity(cert)
	if e != nil || id != out.NodeID || cert.IsCA {
		return errors.New("invalid issued node identity")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(out.CA)) {
		return errors.New("invalid trust chain")
	}
	inter := x509.NewCertPool()
	for _, der := range pair.Certificate[1:] {
		c, err := x509.ParseCertificate(der)
		if err != nil {
			return err
		}
		inter.AddCert(c)
	}
	if _, e = cert.Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); e != nil {
		return errors.New("issued certificate chain invalid")
	}
	save := func(name, value string) error {
		path := filepath.Join(o.PKIDir, name)
		if b, err := os.ReadFile(path); err == nil {
			if string(b) == value {
				return nil
			}
			return errors.New("conflicting partial enrollment")
		}
		return privateWrite(path, []byte(value))
	}
	if e = save("ca.crt", out.CA); e != nil {
		return e
	}
	if e = save("control", out.Control); e != nil {
		return e
	}
	if e = save("node-id", out.NodeID); e != nil {
		return e
	}
	if o.EnvFile != "" {
		if !filepath.IsAbs(o.EnvFile) {
			return errors.New("absolute env file required")
		}
		env := "RELADOCK_NODE=" + out.NodeID + "\nRELADOCK_CONTROL=" + out.Control + "\nRELADOCK_PKI=" + o.PKIDir + "\n"
		if e = privateWrite(o.EnvFile, []byte(env)); e != nil {
			info, statErr := os.Lstat(o.EnvFile)
			prior, readErr := os.ReadFile(o.EnvFile)
			if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || readErr != nil || string(prior) != env {
				return errors.New("conflicting or unsafe partial environment file; preserved")
			}
		}
	}
	return save("agent.crt", out.Certificate)
}
