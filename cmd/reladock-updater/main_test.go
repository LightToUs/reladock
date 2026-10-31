package main

import "testing"

func TestCommandRejectsArgumentsAndNonRootExecution(t *testing.T) {
	if err := run([]string{"--command", "anything"}, 0); err == nil {
		t.Fatal("arbitrary updater arguments accepted")
	}
	if err := run(nil, 1000); err == nil {
		t.Fatal("non-root updater execution accepted")
	}
}

func TestAgentStateDirDefaultsAndRejectsUnsafePaths(t *testing.T) {
	if got, err := agentStateDir(map[string]string{}); err != nil || got != "/var/lib/reladock-agent" {
		t.Fatalf("default state dir = %q, %v", got, err)
	}
	if got, err := agentStateDir(map[string]string{"RELADOCK_STATE_DIR": "/srv/reladock/state"}); err != nil || got != "/srv/reladock/state" {
		t.Fatalf("custom state dir = %q, %v", got, err)
	}
	for _, unsafe := range []string{"relative", "/", "/srv/../etc"} {
		if _, err := agentStateDir(map[string]string{"RELADOCK_STATE_DIR": unsafe}); err == nil {
			t.Fatalf("unsafe state dir accepted: %q", unsafe)
		}
	}
}
