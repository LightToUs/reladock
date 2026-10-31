package recovery

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func nodeFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"etc/reladock/node-pki", "var/lib/reladock-agent/control", "var/lib/reladock-updater"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for name, data := range map[string]string{"etc/reladock/node-pki/agent.crt": "certificate", "etc/reladock/agent.env": "env", "var/lib/reladock-agent/control/journal.json": "{}", "var/lib/reladock-updater/state.json": "{}"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestCollectNodeSourcesRejectsSymlinkAndHardLink(t *testing.T) {
	root := nodeFixture(t)
	if _, err := collectNodeSources(root); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "etc/reladock/agent.env"), filepath.Join(root, "etc/reladock/link")); err != nil {
		t.Fatal(err)
	}
	if _, err := collectNodeSources(root); err == nil {
		t.Fatal("accepted node-state symlink")
	}
	if err := os.Remove(filepath.Join(root, "etc/reladock/link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(root, "etc/reladock/agent.env"), filepath.Join(root, "etc/reladock/hard")); err != nil {
		t.Fatal(err)
	}
	if _, err := collectNodeSources(root); err == nil {
		t.Fatal("accepted multiply-linked node state")
	}
}

func TestNodeStagingNeverTargetsLiveRoots(t *testing.T) {
	for _, p := range []string{"/etc/recovery", "/var/lib/recovery", "/etc/reladock", "relative"} {
		if err := safeNodeStaging(p); err == nil {
			t.Fatalf("accepted %q", p)
		}
	}
	parent, err := os.MkdirTemp("/tmp", "reladock-node-stage-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(parent)
	stage := filepath.Join(parent, "new-stage")
	if err := safeNodeStaging(stage); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	if err := safeNodeStaging(stage); err == nil {
		t.Fatal("accepted existing stage")
	}
	alias := filepath.Join(parent, "etc-alias")
	if err := os.Symlink("/etc", alias); err != nil {
		t.Fatal(err)
	}
	if err := safeNodeStaging(filepath.Join(alias, "stage")); err == nil {
		t.Fatal("accepted staging path resolving into /etc")
	}
}

func TestNodePreflightAndControlLock(t *testing.T) {
	if err := nodePreflight("darwin", 0); err == nil {
		t.Fatal("accepted non-Linux host")
	}
	if err := nodePreflight("linux", 1000); err == nil {
		t.Fatal("accepted non-root user")
	}
	if err := nodePreflight("linux", 0); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "control.lock")
	if err := os.WriteFile(path, nil, 0640); err != nil {
		t.Fatal(err)
	}
	first, err := lockControl(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := lockControl(path); err == nil {
		t.Fatal("acquired busy control lock")
	}
}

func TestInactiveUnitGate(t *testing.T) {
	inactive := filepath.Join(t.TempDir(), "systemctl")
	if err := os.WriteFile(inactive, []byte("#!/bin/sh\nexit 3\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := requireInactiveUnits(context.Background(), inactive); err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(t.TempDir(), "systemctl")
	if err := os.WriteFile(active, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := requireInactiveUnits(context.Background(), active); err == nil {
		t.Fatal("accepted active unit")
	}
}
