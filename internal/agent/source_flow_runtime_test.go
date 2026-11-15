package agent

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"reladock/internal/sourceflow"
)

type runtimeEventSource struct{ event sourceflow.KernelEvent }

func (s runtimeEventSource) Run(ctx context.Context, ready func(), emit func(sourceflow.KernelEvent) bool) error {
	ready()
	emit(s.event)
	<-ctx.Done()
	return ctx.Err()
}

func TestSourceFlowRuntimeRequiresExplicitWALAndUsesLastGoodManifest(t *testing.T) {
	if err := (&NodeEngine{}).ConfigureSourceFlow("dddddddd-dddd-4ddd-8ddd-dddddddddddd"); err == nil {
		t.Fatal("unconfigured source-flow runtime accepted")
	}
	w, err := sourceflow.Open(filepath.Join(t.TempDir(), "source-flow"), sourceflow.Options{NodeID: "hk-01"})
	if err != nil {
		t.Fatal(err)
	}
	manifest, digest, err := sourceflow.NewManifest("hk-01", 9, []sourceflow.AppliedRule{{
		AllocationID: "11111111-1111-4111-8111-111111111111", TenantID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		RuleID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", RuleVersion: 4, SourceLoggingEnabled: true,
		Protocol: "tcp", RelayIP: "203.0.113.10", RelayPort: 24443, TargetIP: "198.51.100.20", TargetPort: 443,
	}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	engine := &NodeEngine{NodeID: "hk-01", SourceFlowWAL: w, sourceFlowSource: runtimeEventSource{event: sourceflow.KernelEvent{Protocol: "tcp", SourceIP: "192.0.2.40", SourcePort: 53000, DestinationIP: "203.0.113.10", DestinationPort: 24443, ObservedAt: now, ID: 71}}, sourceFlowManifest: &manifest, sourceFlowManifestDigest: digest}
	t.Cleanup(func() { _ = engine.Close() })
	if err = engine.ConfigureSourceFlow("dddddddd-dddd-4ddd-8ddd-dddddddddddd"); err != nil || !engine.SourceFlowAvailable() {
		t.Fatalf("configure err=%v available=%v", err, engine.SourceFlowAvailable())
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- engine.CaptureSourceFlows(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		status, statusErr := w.Status()
		if statusErr != nil {
			t.Fatal(statusErr)
		}
		if status.PendingEvents == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	batch, ok, err := w.NextBatch()
	if err != nil || !ok || len(batch.Events) != 1 || batch.Events[0].ConfigGeneration != 9 {
		t.Fatalf("batch=%+v ok=%v err=%v", batch, ok, err)
	}
}

func TestRuntimeActivitiesRequirePairedIndependentSourceFlowWork(t *testing.T) {
	run := func(context.Context) error { return nil }
	base := RuntimeHooks{Configuration: run, Heartbeat: run, Preflight: run, Renewal: run}
	base.SourceFlowCapture = run
	if _, err := RuntimeActivities(base); err == nil {
		t.Fatal("capture without uploader accepted")
	}
	base.SourceFlowUpload = run
	activities, err := RuntimeActivities(base)
	if err != nil || len(activities) != 6 {
		t.Fatalf("activities=%d err=%v", len(activities), err)
	}
	if activities[4].Name != "source-flow-capture" || activities[5].Name != "source-flow-upload" {
		t.Fatalf("source activities=%+v", activities[4:])
	}
}

func TestSourceFlowWALPressureRequiresContinuousNinetySeconds(t *testing.T) {
	start := time.Now().UTC()
	engine := &NodeEngine{}
	if engine.sourceFlowPressureDegraded(start, 80, 100) || engine.sourceFlowPressureDegraded(start.Add(89*time.Second), 90, 100) {
		t.Fatal("WAL pressure degraded before 90 continuous seconds")
	}
	if engine.sourceFlowPressureDegraded(start.Add(89*time.Second), 79, 100) {
		t.Fatal("recovery threshold remained degraded")
	}
	if engine.sourceFlowPressureDegraded(start.Add(2*time.Minute), 80, 100) || !engine.sourceFlowPressureDegraded(start.Add(210*time.Second), 80, 100) {
		t.Fatal("pressure reset or sustained threshold was not honored")
	}
	if (&NodeEngine{}).sourceFlowPressureDegraded(start.Add(210*time.Second), 90, 100) {
		t.Fatal("fresh process inherited an unproven pressure interval")
	}
}
