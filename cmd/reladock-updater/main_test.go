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
