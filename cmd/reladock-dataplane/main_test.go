package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"reladock/internal/dataplane"
	"reladock/internal/platform"
)

func configFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	c := platform.NodeConfig{SchemaVersion: 1, NodeID: "hk-01", Generation: 1, ListenAddress: "192.0.2.1", Rules: []platform.ConfigRule{{ID: "rule-a", TenantID: "tenant-a", RuleInput: platform.RuleInput{Name: "Relay", NodeID: "hk-01", Protocol: "tcp", TargetHost: "198.51.100.2", TargetPort: 443, BandwidthMbps: 100}, ListenPort: 20000, Version: 1, Status: "pending-agent"}}}
	b, e := json.Marshal(c)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(p, b, 0600); e != nil {
		t.Fatal(e)
	}
	return p
}
func argsFor(p string) []string {
	return []string{"--config", p, "--node", "hk-01", "--snat", "192.0.2.2"}
}

func TestDefaultPlanNeverConstructsRunnerOrState(t *testing.T) {
	p := configFile(t)
	dir := filepath.Join(t.TempDir(), "must-not-exist")
	var out bytes.Buffer
	d := dependencies{goos: "linux", runner: func(string) (dataplane.Runner, error) { t.Fatal("dry-run constructed runner"); return nil, nil }}
	if e := run(context.Background(), append(argsFor(p), "--state-dir", dir), &out, d); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(out.String(), "dnat to 198.51.100.2:443") || !strings.Contains(out.String(), "NOT enforced") {
		t.Fatal(out.String())
	}
	if _, e := os.Stat(dir); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("dry-run wrote state")
	}
}

func TestApplyRequiresLinuxAndExplicitStateDirectory(t *testing.T) {
	p := configFile(t)
	for _, osName := range []string{"darwin", "linux"} {
		t.Run(osName, func(t *testing.T) {
			d := dependencies{goos: osName, runner: func(string) (dataplane.Runner, error) { t.Fatal("unsafe runner construction"); return nil, nil }}
			if e := run(context.Background(), append(argsFor(p), "--apply"), &bytes.Buffer{}, d); e == nil {
				t.Fatal("unsafe apply accepted")
			}
		})
	}
}

func TestCLIStrictJSONAndConflictingFlags(t *testing.T) {
	p := configFile(t)
	if e := os.WriteFile(p, []byte(`{"schemaVersion":1,"unexpected":"x"}`), 0600); e != nil {
		t.Fatal(e)
	}
	if e := run(context.Background(), argsFor(p), &bytes.Buffer{}, dependencies{}); e == nil {
		t.Fatal("unknown fields accepted")
	}
	if e := run(context.Background(), append(argsFor(configFile(t)), "--apply", "--dry-run"), &bytes.Buffer{}, dependencies{goos: "linux"}); e == nil {
		t.Fatal("conflicting flags accepted")
	}
}

type cliRunner struct{ calls int }

func (r *cliRunner) Run(_ context.Context, args []string, _ string) ([]byte, error) {
	r.calls++
	if len(args) > 2 && args[1] == "list" {
		return []byte(`{"nftables":[]}`), nil
	}
	return nil, errors.New("mock check failure")
}
func TestExplicitApplyReachesMockCheckOnly(t *testing.T) {
	p := configFile(t)
	r := &cliRunner{}
	d := dependencies{goos: "linux", runner: func(string) (dataplane.Runner, error) { return r, nil }}
	e := run(context.Background(), append(argsFor(p), "--apply", "--state-dir", filepath.Join(t.TempDir(), "state")), &bytes.Buffer{}, d)
	if e == nil || r.calls != 2 {
		t.Fatalf("apply: %v calls %d", e, r.calls)
	}
}
