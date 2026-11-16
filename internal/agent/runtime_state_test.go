package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"reladock/internal/platform"
	"reladock/internal/sourceflow"
)

func TestApplyJournalFitsMaximumConfigAndEndpointManifest(t *testing.T) {
	state, err := NewApplyState(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	rules := make([]sourceflow.AppliedRule, 0, 10000)
	for i := 0; i < 10000; i++ {
		rules = append(rules, sourceflow.AppliedRule{
			AllocationID: fmt.Sprintf("00000000-0000-4000-8000-%012d", i),
			TenantID:     "00000000-0000-4000-8000-000000000001",
			RuleID:       fmt.Sprintf("00000000-0000-4000-8001-%012d", i), RuleVersion: 1,
			SourceLoggingEnabled: true, Protocol: []string{"tcp", "udp"}[i%2],
			RelayIP: "203.0.113.254", RelayPort: i + 1,
			TargetIP: "198.51.100.254", TargetPort: 65535,
		})
	}
	manifest, digest, err := sourceflow.NewManifest("maximum-endpoints", 1, rules)
	if err != nil {
		t.Fatal(err)
	}
	payload := json.RawMessage(`"` + strings.Repeat("a", platform.MaxConfigPayloadBytes-2) + `"`)
	record := &applyRecord{
		Origin: "https://control.example", NodeID: "maximum-endpoints",
		Job:    platform.ConfigJob{ID: "00000000-0000-4000-8000-000000000001", NodeID: "maximum-endpoints", Generation: 1, Payload: payload},
		Report: &platform.ConfigReport{Outcome: "applied", SourceFlowManifest: &manifest, SourceFlowManifestDigest: digest},
	}
	if err = state.save(context.Background(), record); err != nil {
		t.Fatalf("save maximum bounded delivery journal: %v", err)
	}
	loaded, err := state.load(context.Background())
	if err != nil || loaded == nil || len(loaded.Job.Payload) != len(payload) || len(loaded.Report.SourceFlowManifest.Rules) != 10000 {
		t.Fatalf("load maximum bounded delivery journal: loaded=%v err=%v", loaded != nil, err)
	}
}

func TestApplyStateLockAcrossProcesses(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	state, e := NewApplyState(dir)
	if e != nil {
		t.Fatal(e)
	}
	unlock, e := state.Lock(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestApplyStateProcessHelper$")
	child.Env = append(os.Environ(), "RELADOCK_TEST_LOCK_DIR="+dir)
	if out, e := child.CombinedOutput(); e != nil {
		t.Fatalf("separate process bypassed lock: %s %v", out, e)
	}
}
func TestApplyStateProcessHelper(t *testing.T) {
	dir := os.Getenv("RELADOCK_TEST_LOCK_DIR")
	if dir == "" {
		t.Skip("subprocess helper")
	}
	state, e := NewApplyState(dir)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	unlock, e := state.Lock(ctx)
	if unlock != nil {
		unlock()
	}
	if !errors.Is(e, context.DeadlineExceeded) {
		t.Fatalf("cross-process lock error=%v", e)
	}
}
func TestApplyJournalRefusesTamperingAndControllerChangeBeforeNetwork(t *testing.T) {
	state, e := NewApplyState(filepath.Join(t.TempDir(), "state"))
	if e != nil {
		t.Fatal(e)
	}
	job := applyJob()
	record := &applyRecord{Origin: "https://other-control.test", NodeID: "hk-01", Job: job, BeginAttempted: true}
	if e = state.save(context.Background(), record); e != nil {
		t.Fatal(e)
	}
	c := Client{BaseURL: "https://control.test", NodeID: "hk-01", State: state, HTTP: &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid journal reached network")
		return nil, errors.New("unexpected network")
	})}, Applier: applyFunc(func(context.Context, platform.NodeConfig) error {
		t.Fatal("untrusted journal reached apply")
		return nil
	})}
	if _, e = c.Once(context.Background()); e == nil {
		t.Fatal("controller changed over pending application")
	}
	record.Origin = c.BaseURL
	record.Job.Payload = append([]byte(" "), record.Job.Payload...)
	if e = state.save(context.Background(), record); e != nil {
		t.Fatal(e)
	}
	if _, e = c.Once(context.Background()); e == nil {
		t.Fatal("tampered journal reached begin")
	}
	record.Report = &platform.ConfigReport{LeaseToken: job.LeaseToken, Checksum: job.Checksum, Outcome: "applied"}
	if e = state.save(context.Background(), record); e != nil {
		t.Fatal(e)
	}
	if _, e = c.Once(context.Background()); e == nil {
		t.Fatal("tampered applied receipt reached report")
	}
	record.Job = job
	record.Report = &platform.ConfigReport{LeaseToken: job.LeaseToken, Checksum: job.Checksum, Outcome: "failed", ErrorCode: "internal-error"}
	if e = state.save(context.Background(), record); e != nil {
		t.Fatal(e)
	}
	if _, e = c.Once(context.Background()); e == nil {
		t.Fatal("old failure receipt without rollback proof reached report")
	}
}

func TestApplyJournalPreservesExactChecksumBoundPayload(t *testing.T) {
	state, e := NewApplyState(filepath.Join(t.TempDir(), "state"))
	if e != nil {
		t.Fatal(e)
	}
	job := applyJob()
	job.Payload = append([]byte(" \n "), job.Payload...)
	sum := sha256.Sum256(job.Payload)
	job.Checksum = hex.EncodeToString(sum[:])
	if e = ValidateConfig("hk-01", job); e != nil {
		t.Fatal(e)
	}
	record := &applyRecord{Origin: "https://control.test", NodeID: "hk-01", Job: job, BeginAttempted: true, BeginConfirmed: true}
	if e = state.save(context.Background(), record); e != nil {
		t.Fatal(e)
	}
	loaded, e := state.load(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if string(loaded.Job.Payload) != string(job.Payload) || ValidateConfig("hk-01", loaded.Job) != nil {
		t.Fatal("journal changed exact config payload")
	}
}
