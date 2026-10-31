package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type testHealthRequest struct {
	TaskID        string `json:"taskId"`
	Challenge     string `json:"challenge"`
	TargetVersion string `json:"targetVersion"`
}

func writeHealthRequest(t *testing.T, path string, request testHealthRequest) {
	t.Helper()
	raw, _ := json.Marshal(request)
	if err := os.WriteFile(path, raw, 0640); err != nil {
		t.Fatal(err)
	}
}

func TestUpgradeHealthRequiresBothSignalsInEitherOrder(t *testing.T) {
	root := t.TempDir()
	requestPath := filepath.Join(root, "request.json")
	markerDir := filepath.Join(root, "marker")
	request := testHealthRequest{TaskID: "11111111-1111-4111-8111-111111111111", Challenge: strings.Repeat("a", 64), TargetVersion: "v0.9.0"}
	writeHealthRequest(t, requestPath, request)
	for _, heartbeatFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "heartbeat-first", false: "configuration-first"}[heartbeatFirst], func(t *testing.T) {
			dir := filepath.Join(markerDir, map[bool]string{true: "h", false: "c"}[heartbeatFirst])
			health := NewUpgradeHealth(requestPath, dir, "v0.9.0")
			first, second := health.HeartbeatSucceeded, health.ConfigurationSucceeded
			if !heartbeatFirst {
				first, second = second, first
			}
			if err := first(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(filepath.Join(dir, "upgrade-health.json")); !os.IsNotExist(err) {
				t.Fatal("one signal published health")
			}
			if err := second(); err != nil {
				t.Fatal(err)
			}
			info, err := os.Lstat(filepath.Join(dir, "upgrade-health.json"))
			if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
				t.Fatalf("marker: %+v %v", info, err)
			}
			var marker struct {
				TaskID, Challenge, TargetVersion string
				CompletedAt                      time.Time
			}
			raw, _ := os.ReadFile(filepath.Join(dir, "upgrade-health.json"))
			if json.Unmarshal(raw, &marker) != nil || marker.TaskID != request.TaskID || marker.Challenge != request.Challenge || marker.TargetVersion != request.TargetVersion || marker.CompletedAt.IsZero() {
				t.Fatalf("marker = %s", raw)
			}
		})
	}
}

func TestUpgradeHealthResetsSignalsWhenTaskChanges(t *testing.T) {
	root := t.TempDir()
	requestPath := filepath.Join(root, "request.json")
	markerDir := filepath.Join(root, "marker")
	first := testHealthRequest{TaskID: "11111111-1111-4111-8111-111111111111", Challenge: strings.Repeat("a", 64), TargetVersion: "v0.9.0"}
	second := testHealthRequest{TaskID: "22222222-2222-4222-8222-222222222222", Challenge: strings.Repeat("b", 64), TargetVersion: "v0.9.0"}
	writeHealthRequest(t, requestPath, first)
	health := NewUpgradeHealth(requestPath, markerDir, "v0.9.0")
	if err := health.HeartbeatSucceeded(); err != nil {
		t.Fatal(err)
	}
	writeHealthRequest(t, requestPath, second)
	if err := health.ConfigurationSucceeded(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(markerDir, "upgrade-health.json")); !os.IsNotExist(err) {
		t.Fatal("signals from different tasks were combined")
	}
	if err := health.HeartbeatSucceeded(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(markerDir, "upgrade-health.json"))
	if err != nil || !strings.Contains(string(raw), second.TaskID) {
		t.Fatalf("second task marker: %s %v", raw, err)
	}
}

func TestUpgradeHealthRejectsUnsafeRequestsAndIgnoresOtherVersions(t *testing.T) {
	root := t.TempDir()
	requestPath := filepath.Join(root, "request.json")
	health := NewUpgradeHealth(requestPath, filepath.Join(root, "marker"), "v0.9.0")
	if err := health.HeartbeatSucceeded(); err != nil {
		t.Fatalf("missing request should be idle: %v", err)
	}
	for name, raw := range map[string]string{
		"unknown":  `{"taskId":"11111111-1111-4111-8111-111111111111","challenge":"` + strings.Repeat("a", 64) + `","targetVersion":"v0.9.0","extra":true}`,
		"trailing": `{"taskId":"11111111-1111-4111-8111-111111111111","challenge":"` + strings.Repeat("a", 64) + `","targetVersion":"v0.9.0"}{}`,
		"bad id":   `{"taskId":"../x","challenge":"` + strings.Repeat("a", 64) + `","targetVersion":"v0.9.0"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(requestPath, []byte(raw), 0640); err != nil {
				t.Fatal(err)
			}
			if err := health.HeartbeatSucceeded(); err == nil {
				t.Fatal("unsafe request accepted")
			}
		})
	}
	writeHealthRequest(t, requestPath, testHealthRequest{TaskID: "11111111-1111-4111-8111-111111111111", Challenge: strings.Repeat("a", 64), TargetVersion: "v0.10.0"})
	if err := health.HeartbeatSucceeded(); err != nil {
		t.Fatal(err)
	}
	if err := health.ConfigurationSucceeded(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(root, "marker", "upgrade-health.json")); !os.IsNotExist(err) {
		t.Fatal("old Agent marked another version healthy")
	}
	if err := os.WriteFile(requestPath, []byte(strings.Repeat("x", 65<<10)), 0640); err != nil {
		t.Fatal(err)
	}
	if err := health.HeartbeatSucceeded(); err == nil {
		t.Fatal("oversized request accepted")
	}
}
