package agent

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"reladock/internal/platform"
)

func TestDrainStatusPreventsUnrequestedAttestationWork(t *testing.T) {
	client := Client{BaseURL: "https://control.test", NodeID: "hk-01", HTTP: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/agent/drain-status" || r.Method != http.MethodPost {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"draining":false}`))}, nil
	})}}
	draining, err := client.DrainRequested(context.Background())
	if err != nil || draining {
		t.Fatalf("unexpected drain status draining=%v err=%v", draining, err)
	}
}

func TestDrainReportIsBoundedAndDoesNotFollowRedirects(t *testing.T) {
	calls := 0
	client := Client{BaseURL: "https://control.test", NodeID: "hk-01", HTTP: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path != "/agent/drain-attestation" {
			t.Fatal(r.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusTemporaryRedirect, Header: http.Header{"Location": []string{"https://attacker.test"}}, Body: http.NoBody}, nil
	})}}
	_, digest, _ := platform.DrainQuarantineDigest(nil)
	value := platform.DrainAttestation{SchemaVersion: 1, NodeID: "hk-01", Generation: 1, CertificateFingerprint: strings.Repeat("a", 64), KernelBootID: "11111111-1111-4111-8111-111111111111", AgentStartedAt: time.Now(), AgentVersion: "v0.9.0", JournalSequence: 1, GoodDigest: strings.Repeat("b", 64), QuarantineDigest: digest, NoPendingDataplane: true, NoRetirementJournal: true, UsageWALDrained: true, UpdaterStable: true, ObservedAt: time.Now()}
	if err := client.ReportDrainAttestation(context.Background(), value); err == nil || calls != 1 {
		t.Fatalf("redirect followed or accepted: calls=%d err=%v", calls, err)
	}
}

func TestUpdaterStableRequiresAbsentRecoveryJournal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if !UpdaterStable(path) {
		t.Fatal("absent updater journal not stable")
	}
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if UpdaterStable(path) {
		t.Fatal("pending updater journal attested stable")
	}
}
