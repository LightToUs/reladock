package updater

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStateStorePersistsStrictPrivateStateAndRejectsUnsafeFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	store, err := OpenStateStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := &State{TaskID: "11111111-1111-4111-8111-111111111111", LeaseToken: strings.Repeat("b", 64), SHA256: strings.Repeat("a", 64), TargetVersion: "v0.9.0", PreviousVersion: "v0.8.0", Arch: "amd64", Phase: "claimed"}
	if err = store.Save(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(context.Background())
	if err != nil || *got != *want {
		t.Fatalf("loaded state = %+v, %v", got, err)
	}
	info, err := os.Lstat(filepath.Join(dir, "state.json"))
	if err != nil || info.Mode().Perm() != 0600 || !info.Mode().IsRegular() {
		t.Fatalf("state mode: %+v %v", info, err)
	}
	if err = os.Chmod(filepath.Join(dir, "state.json"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Load(context.Background()); err == nil {
		t.Fatal("world-readable updater state accepted")
	}
}

func TestStateStoreTracksConsecutiveAgentVersionsAndManualPackageUpgrade(t *testing.T) {
	store, err := OpenStateStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SetCurrentVersion("v0.9.0"); err != nil {
		t.Fatal(err)
	}
	if got, err := store.CurrentVersion("v0.8.0"); err != nil || got != "v0.9.0" {
		t.Fatalf("automatic marker = %q %v", got, err)
	}
	if got, err := store.CurrentVersion("v0.10.0"); err != nil || got != "v0.10.0" {
		t.Fatalf("manual package version = %q %v", got, err)
	}
}

func TestStateStoreRejectsUnknownPhaseAndSerializesExecutors(t *testing.T) {
	store, err := OpenStateStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	bad := &State{TaskID: "11111111-1111-4111-8111-111111111111", LeaseToken: strings.Repeat("b", 64), SHA256: strings.Repeat("a", 64), TargetVersion: "v0.9.0", PreviousVersion: "v0.8.0", Arch: "amd64", Phase: "invented"}
	if err = store.Save(context.Background(), bad); err == nil {
		t.Fatal("unknown phase saved")
	}
	taskRoot := filepath.Join(store.dir, "tasks", "11111111-1111-4111-8111-111111111111")
	bad = &State{TaskID: "11111111-1111-4111-8111-111111111111", LeaseToken: strings.Repeat("b", 64), SHA256: strings.Repeat("a", 64), TargetVersion: "v0.9.0", PreviousVersion: "v0.8.0", Arch: "amd64", Phase: "staged", Staged: StagedBinaries{Agent: "/tmp/attacker-agent", Dataplane: filepath.Join(taskRoot, "stage", "reladock-dataplane")}}
	if err = store.Save(context.Background(), bad); err == nil {
		t.Fatal("staged path outside task root accepted")
	}
	unlock, err := store.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = store.Lock(ctx); err == nil {
		t.Fatal("second updater acquired process lock")
	}
}
