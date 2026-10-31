package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"reladock/internal/nodeauth"
)

func pendingCertificate(t *testing.T, dir string) (tls.Certificate, []byte, time.Time) {
	t.Helper()
	signer, err := nodeauth.LoadBootstrapSigner(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key"), filepath.Join(dir, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatal(err)
	}
	csr := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})
	certificate, err := signer.Sign("hk-01", csr)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalPKCS8PrivateKey(key)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	pair, err := tls.X509KeyPair(certificate, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(pair.Certificate[0])
	return pair, certificate, leaf.NotAfter
}

func TestRotationClientRenewsWithCurrentAndActivatesWithPendingIdentity(t *testing.T) {
	dir := credentialFixture(t)
	manager, err := OpenCredentialManager(dir, "hk-01")
	if err != nil {
		t.Fatal(err)
	}
	pending, certificate, expires := pendingCertificate(t, dir)
	pendingLeaf, _ := x509.ParseCertificate(pending.Certificate[0])
	currentFingerprint := manager.Current().Fingerprint
	pendingFingerprint := nodeauth.Fingerprint(pendingLeaf)
	seenRenew := ""
	seenActivate := ""
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fingerprint := nodeauth.Fingerprint(r.TLS.PeerCertificates[0])
		switch r.URL.Path {
		case "/agent/certificate/renew":
			seenRenew = fingerprint
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["csr"] == "" {
				t.Fatalf("renew request: %v %+v", err, body)
			}
			json.NewEncoder(w).Encode(map[string]any{"certificate": string(certificate), "expiresAt": expires})
		case "/agent/certificate/activate":
			seenActivate = fingerprint
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	})
	serverTLS, err := nodeauth.ServerTLS(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(handler)
	srv.TLS = serverTLS
	srv.StartTLS()
	defer srv.Close()
	clientTLS, _ := manager.TLSConfig()
	transport := &http.Transport{TLSClientConfig: clientTLS}
	defer transport.CloseIdleConnections()
	client := &Client{HTTP: &http.Client{Transport: transport}, BaseURL: srv.URL, NodeID: "hk-01"}
	result, err := client.Renew(context.Background(), []byte("csr-fixture"))
	if err != nil || string(result.Certificate) != string(certificate) || !result.ExpiresAt.Equal(expires) {
		t.Fatalf("renew result: %+v %v", result, err)
	}
	if err = client.Activate(context.Background(), &pending); err != nil {
		t.Fatal(err)
	}
	if seenRenew != currentFingerprint || seenActivate != pendingFingerprint {
		t.Fatalf("TLS identities renew=%q activate=%q", seenRenew, seenActivate)
	}
}

func TestRotationClientRejectsUnavailableTransport(t *testing.T) {
	client := &Client{BaseURL: "https://control.example.com", NodeID: "hk-01"}
	if _, err := client.Renew(context.Background(), []byte("csr")); err == nil {
		t.Fatal("renew without mTLS client accepted")
	}
	if err := client.Activate(context.Background(), &tls.Certificate{}); err == nil {
		t.Fatal("activation without mTLS transport accepted")
	}
}
