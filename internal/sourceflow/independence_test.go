package sourceflow_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"reladock/internal/metering"
	"reladock/internal/sourceflow"
)

func TestSourceWALFailureDoesNotAffectMeteringWAL(t *testing.T) {
	root := t.TempDir()
	sourceDir := filepath.Join(root, "source")
	meterDir := filepath.Join(root, "metering")
	source, err := sourceflow.Open(sourceDir, sourceflow.Options{NodeID: "hk-01", MaxBytes: 16 << 10, MaxEvents: 1, MaxGaps: 4, GapReserveBytes: 2048})
	if err != nil {
		t.Fatal(err)
	}
	event := sourceflow.Event{
		EventID: "11111111-aaaa-4aaa-8aaa-111111111111", TenantID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", RuleID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", NodeID: "hk-01", AllocationID: "11111111-1111-4111-8111-111111111111",
		ConfigGeneration: 1, RuleVersion: 1, Protocol: "tcp", SourceIP: "192.0.2.40", SourcePort: 53000, RelayIP: "203.0.113.10", RelayPort: 24443, TargetIP: "198.51.100.20", TargetPort: 443,
		ObservedAt: time.Now().UTC(), KernelBootID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd",
	}
	if _, err = source.AppendEvent(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	event.EventID = "22222222-aaaa-4aaa-8aaa-222222222222"
	if _, err = source.AppendEvent(context.Background(), event); !errors.Is(err, sourceflow.ErrFull) {
		t.Fatalf("source WAL did not fill: %v", err)
	}
	meter, err := metering.Open(meterDir, metering.Options{NodeID: "hk-01"})
	if err != nil {
		t.Fatal(err)
	}
	defer meter.Close()
	if err = meter.BindPeriod(context.Background(), "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "", 0, 0); err != nil {
		t.Fatalf("source pressure blocked metering WAL: %v", err)
	}
	if err = source.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(sourceDir, "wal.json"), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = sourceflow.Open(sourceDir, sourceflow.Options{NodeID: "hk-01", MaxBytes: 16 << 10, MaxEvents: 1, MaxGaps: 4, GapReserveBytes: 2048}); !errors.Is(err, sourceflow.ErrCorrupt) {
		t.Fatalf("corrupt source WAL accepted: %v", err)
	}
	if err = meter.BindPeriod(context.Background(), "cccccccc-cccc-4ccc-8ccc-cccccccccccc", "", 0, 0); err != nil {
		t.Fatalf("corrupt source WAL affected metering WAL: %v", err)
	}
}
