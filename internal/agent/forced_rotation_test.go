package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reladock/internal/platform"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHeartbeatRequestsEarlyRotationAndStaleReplayDoesNotRotateAgain(t *testing.T) {
	_, manager, sign := rotationFixture(t)
	old := manager.Current().Fingerprint
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "certificateRenewalFingerprint": old})
	}))
	defer srv.Close()
	c := Client{HTTP: srv.Client(), BaseURL: srv.URL, NodeID: "hk-01", Credentials: manager}
	if err := c.ReportHeartbeat(context.Background(), platform.AgentHeartbeat{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-manager.RotationRequests():
	default:
		t.Fatal("heartbeat did not wake renewal")
	}
	fake := &rotationFake{sign: sign}
	if err := manager.ReconcileRotation(context.Background(), fake, time.Now()); err != nil {
		t.Fatal(err)
	}
	next := manager.Current().Fingerprint
	if next == old {
		t.Fatal("early request did not publish new identity")
	}
	if err := c.ReportHeartbeat(context.Background(), platform.AgentHeartbeat{}); err != nil {
		t.Fatal(err)
	}
	if err := manager.ReconcileRotation(context.Background(), fake, time.Now()); err != nil {
		t.Fatal(err)
	}
	if manager.Current().Fingerprint != next || fake.renewCalls != 1 {
		t.Fatal("stale heartbeat replay caused another rotation")
	}
}

func TestForcedRotationRecoversPersistedIdentityAfterRestart(t *testing.T) {
	for _, phase := range []string{"renew", "activate"} {
		t.Run(phase, func(t *testing.T) {
			dir, manager, sign := rotationFixture(t)
			old := manager.Current().Fingerprint
			if err := manager.RequestRotation(old); err != nil {
				t.Fatal(err)
			}
			fake := &rotationFake{sign: sign}
			if phase == "renew" {
				fake.renewErr = errors.New("lost response")
			} else {
				fake.activateErrs = []error{errors.New("lost response")}
			}
			if err := manager.ReconcileRotation(context.Background(), fake, time.Now()); err == nil {
				t.Fatal("injected failure not observed")
			}
			reopened, err := OpenCredentialManager(dir, "hk-01")
			if err != nil {
				t.Fatal(err)
			}
			if err = reopened.ReconcileRotation(context.Background(), fake, time.Now()); err != nil {
				t.Fatal(err)
			}
			if reopened.Current().Fingerprint == old {
				t.Fatal("restart lost forced rotation")
			}
			if phase == "renew" && string(fake.csrs[0]) != string(fake.csrs[1]) {
				t.Fatal("restart changed CSR")
			}
			if phase == "activate" && fake.renewCalls != 1 {
				t.Fatal("activation recovery reissued identity")
			}
		})
	}
}

func TestForcedRotationSerializesConcurrentReconcile(t *testing.T) {
	_, manager, sign := rotationFixture(t)
	old := manager.Current().Fingerprint
	if err := manager.RequestRotation(old); err != nil {
		t.Fatal(err)
	}
	fake := &rotationFake{sign: sign}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- manager.ReconcileRotation(context.Background(), fake, time.Now()) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if manager.Current().Fingerprint == old || fake.renewCalls != 1 || fake.activateCalls != 1 {
		t.Fatalf("duplicate rotation: renew=%d activate=%d", fake.renewCalls, fake.activateCalls)
	}
}

func TestHeartbeatReplyCompatibilityAndValidation(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"legacy", `{"ok":true}`, true},
		{"future extension", `{"ok":true,"future":1}`, true},
		{"missing acknowledgment", `{}`, false},
		{"wrong fingerprint", `{"ok":true,"certificateRenewalFingerprint":"not-a-fingerprint"}`, false},
		{"trailing", `{"ok":true}{}`, false},
		{"oversize", `{"ok":true,"padding":"` + strings.Repeat("x", 8192) + `"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(tc.body)) }))
			defer srv.Close()
			c := Client{HTTP: srv.Client(), BaseURL: srv.URL, NodeID: "hk-01"}
			err := c.ReportHeartbeat(context.Background(), platform.AgentHeartbeat{})
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}

func TestCapabilityHeaderKeepsHeartbeatCompatibleWithStrictLegacyController(t *testing.T) {
	// This mirrors the old wire schema, deliberately without capability fields.
	type legacyHeartbeat struct {
		Version             string                       `json:"version"`
		Mode                string                       `json:"mode"`
		StartedAt           time.Time                    `json:"startedAt"`
		CertificateNotAfter time.Time                    `json:"certificateNotAfter"`
		RotationState       string                       `json:"rotationState"`
		RotationErrorCode   string                       `json:"rotationErrorCode"`
		KernelBootID        string                       `json:"kernelBootId"`
		JournalSequence     int64                        `json:"journalSequence"`
		Resources           platform.ResourceObservation `json:"resources"`
	}
	for _, supported := range []bool{true, false} {
		t.Run(fmt.Sprint(supported), func(t *testing.T) {
			var receivedHeader string
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				receivedHeader = r.Header.Get("X-RelaDock-Certificate-Rotation")
				decoder := json.NewDecoder(r.Body)
				decoder.DisallowUnknownFields()
				var legacy legacyHeartbeat
				if err := decoder.Decode(&legacy); err != nil {
					http.Error(w, "legacy schema rejected heartbeat", 400)
					return
				}
				w.Write([]byte(`{"ok":true}`))
			}))
			defer srv.Close()
			c := Client{HTTP: srv.Client(), BaseURL: srv.URL, NodeID: "hk-01"}
			if err := c.ReportHeartbeat(context.Background(), platform.AgentHeartbeat{CertificateRotationSupported: supported}); err != nil {
				t.Fatalf("legacy controller: %v", err)
			}
			want := ""
			if supported {
				want = "1"
			}
			if receivedHeader != want {
				t.Fatalf("capability header=%q want=%q", receivedHeader, want)
			}
		})
	}
}

func TestRenewalWakeRunsPromptlyWithoutBlockingHeartbeat(t *testing.T) {
	_, manager, _ := rotationFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := make(chan struct{}, 3)
	release := make(chan struct{})
	activities, err := RuntimeActivities(RuntimeHooks{
		Configuration: func(context.Context) error { return nil }, Preflight: func(context.Context) error { return nil }, Heartbeat: func(context.Context) error { return nil },
		RenewalRequests: manager.RotationRequests(), Renewal: func(context.Context) error { calls <- struct{}{}; <-release; return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- RunSupervisor(ctx, activities, nil) }()
	defer func() { cancel(); close(release); <-done }()
	select {
	case <-calls:
	case <-time.After(time.Second):
		t.Fatal("initial renewal missing")
	}
	if err = manager.RequestRotation(manager.Current().Fingerprint); err != nil {
		t.Fatal(err)
	}
	release <- struct{}{}
	select {
	case <-calls:
	case <-time.After(time.Second):
		t.Fatal("forced renewal waited for hourly timer")
	}
}
