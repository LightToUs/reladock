package web

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"reladock/internal/nodeauth"
)

func TestMTLSRetirementEndpointsAreStrictAndCertificateBound(t *testing.T) {
	store := testWebStore(t)
	dir := filepath.Join(t.TempDir(), "pki")
	if err := nodeauth.CreateLocalBundle(dir, "hk-01"); err != nil {
		t.Fatal(err)
	}
	cert, _ := nodeauth.ReadCertificate(filepath.Join(dir, "agent.crt"))
	if err := store.EnrollNode(context.Background(), "hk-01", nodeauth.Fingerprint(cert)); err != nil {
		t.Fatal(err)
	}
	serverTLS, _ := nodeauth.ServerTLS(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key"))
	server := httptest.NewUnstartedServer(NewAgent(store, nil))
	server.TLS = serverTLS
	server.StartTLS()
	defer server.Close()
	clientTLS, _ := nodeauth.ClientTLS(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "agent.crt"), filepath.Join(dir, "agent.key"))
	transport := &http.Transport{TLSClientConfig: clientTLS}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	request := func(method, path, body, origin string) int {
		t.Helper()
		req, _ := http.NewRequest(method, server.URL+path, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", origin)
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		return response.StatusCode
	}
	if status := request(http.MethodPost, "/agent/retirements/claim", "", ""); status != http.StatusNoContent {
		t.Fatalf("empty claim: %d", status)
	}
	if status := request(http.MethodPost, "/agent/retirements/claim", `{}`, ""); status != http.StatusBadRequest {
		t.Fatalf("claim body accepted: %d", status)
	}
	if status := request(http.MethodPost, "/agent/retirements/claim", "", "https://browser.example"); status != http.StatusForbidden {
		t.Fatalf("browser origin accepted: %d", status)
	}
	begin := `{"taskId":"11111111-1111-4111-8111-111111111111","leaseToken":"` + strings.Repeat("a", 64) + `"}`
	if status := request(http.MethodPost, "/agent/retirements/begin", begin[:len(begin)-1]+`,"extra":true}`, ""); status != http.StatusBadRequest {
		t.Fatalf("unknown begin field accepted: %d", status)
	}
	if status := request(http.MethodPost, "/agent/retirements/begin", begin, ""); status != http.StatusNotFound {
		t.Fatalf("unknown task status: %d", status)
	}
	if status := request(http.MethodPost, "/agent/retirements/report", `{}`, ""); status != http.StatusBadRequest {
		t.Fatalf("invalid report accepted: %d", status)
	}
	if status := request(http.MethodGet, "/agent/retirements/claim", "", ""); status != http.StatusMethodNotAllowed {
		t.Fatalf("GET accepted: %d", status)
	}
}
