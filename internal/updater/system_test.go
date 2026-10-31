package updater

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLocalHostControlsOnlyKnownAgentUnits(t *testing.T) {
	var calls []string
	host := &LocalHost{Command: func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		if len(args) == 3 && args[0] == "is-active" && args[1] == "--quiet" && args[2] == "reladock-agent.service" {
			return nil, nil
		}
		return nil, errors.New("inactive")
	}}
	units, err := host.ActiveUnits(context.Background())
	if err != nil || len(units) != 1 || units[0] != "reladock-agent.service" {
		t.Fatalf("active units: %+v %v", units, err)
	}
	if err = host.Stop(context.Background(), "attacker.service"); err == nil {
		t.Fatal("arbitrary service accepted")
	}
	if err = host.Start(context.Background(), "reladock-agent-apply.service"); err == nil {
		t.Fatalf("fake command unexpectedly succeeded: %v", calls)
	}
	for _, call := range calls {
		if strings.ContainsAny(call, ";|&`$") {
			t.Fatalf("shell syntax reached command runner: %q", call)
		}
	}
}

func TestLocalHostBacksUpPublishesAndRestoresBothBinaries(t *testing.T) {
	root := t.TempDir()
	install := filepath.Join(root, "opt")
	state := filepath.Join(root, "state")
	if err := os.MkdirAll(install, 0755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"reladock-agent": "old-agent", "reladock-dataplane": "old-dataplane"} {
		if err := os.WriteFile(filepath.Join(install, name), []byte(content), 0755); err != nil {
			t.Fatal(err)
		}
	}
	stagedDir := filepath.Join(root, "staged")
	if err := os.Mkdir(stagedDir, 0700); err != nil {
		t.Fatal(err)
	}
	staged := StagedBinaries{Agent: filepath.Join(stagedDir, "agent"), Dataplane: filepath.Join(stagedDir, "dataplane")}
	_ = os.WriteFile(staged.Agent, []byte("new-agent"), 0755)
	_ = os.WriteFile(staged.Dataplane, []byte("new-dataplane"), 0755)
	host := &LocalHost{InstallDir: install, StateDir: state}
	backup, err := host.Backup(context.Background(), "11111111-1111-4111-8111-111111111111")
	if err != nil || !hexSHA256.MatchString(backup.AgentSHA256) || !hexSHA256.MatchString(backup.DataplaneSHA256) {
		t.Fatalf("backup: %+v %v", backup, err)
	}
	if err = host.Publish(context.Background(), staged); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(filepath.Join(install, "reladock-agent")); string(raw) != "new-agent" {
		t.Fatalf("publish = %q", raw)
	}
	if err = host.Restore(context.Background(), backup); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(filepath.Join(install, "reladock-agent")); string(raw) != "old-agent" {
		t.Fatalf("restore = %q", raw)
	}
}

func TestLocalHostRefusesPendingDeliveryAndRequiresExactHealthMarker(t *testing.T) {
	root := t.TempDir()
	control := filepath.Join(root, "control")
	if err := os.MkdirAll(control, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(control, "delivery.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	host := &LocalHost{ControlDir: control, HealthDir: filepath.Join(root, "health")}
	if err := host.EnsureControlIdle(context.Background()); err == nil {
		t.Fatal("pending delivery accepted")
	}
	if err := os.Remove(filepath.Join(control, "delivery.json")); err != nil {
		t.Fatal(err)
	}
	if err := host.EnsureControlIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	request := HealthRequest{TaskID: "11111111-1111-4111-8111-111111111111", Challenge: strings.Repeat("c", 64), TargetVersion: "v0.9.0"}
	if err := os.MkdirAll(host.HealthDir, 0750); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(host.HealthDir, "upgrade-health.json", HealthMarker{TaskID: request.TaskID, Challenge: strings.Repeat("d", 64), TargetVersion: request.TargetVersion, CompletedAt: time.Now()}, 0600); err != nil {
		t.Fatal(err)
	}
	if err := host.WaitHealth(context.Background(), request, 20*time.Millisecond); err == nil {
		t.Fatal("mismatched health marker accepted")
	}
	if err := writeJSONAtomic(host.HealthDir, "upgrade-health.json", HealthMarker{TaskID: request.TaskID, Challenge: request.Challenge, TargetVersion: request.TargetVersion, CompletedAt: time.Now()}, 0600); err != nil {
		t.Fatal(err)
	}
	if err := host.WaitHealth(context.Background(), request, time.Second); err != nil {
		t.Fatal(err)
	}
}
