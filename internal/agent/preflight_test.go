package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestPreflightReadOnlyAndBounded(t *testing.T) {
	report, err := InspectHost(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(report)
	if err != nil || len(b) > 7000 {
		t.Fatal("unbounded report")
	}
	if report.Mode != "validation-only" || report.ForwardingReady {
		t.Fatal("inspection implies forwarding readiness")
	}
	if !strings.Contains(string(b), "checks") {
		t.Fatal("no checks")
	}
}
