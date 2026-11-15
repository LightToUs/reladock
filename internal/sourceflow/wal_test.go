package sourceflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func sourceEvent(id string, observed time.Time) Event {
	return Event{
		EventID: id, TenantID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", RuleID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
		NodeID: "hk-01", AllocationID: "11111111-1111-4111-8111-111111111111", ConfigGeneration: 7, RuleVersion: 3,
		Protocol: "tcp", SourceIP: "192.0.2.40", SourcePort: 53000, RelayIP: "203.0.113.10", RelayPort: 24443,
		TargetIP: "198.51.100.20", TargetPort: 443, ObservedAt: observed, KernelBootID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd", ConntrackID: 42,
	}
}

func openSourceWAL(t *testing.T, dir string, options Options) *WAL {
	t.Helper()
	w, err := Open(dir, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

func TestSourceWALBoundsEventsAndReservesGapCapacity(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "source-wal")
	options := Options{NodeID: "hk-01", MaxBytes: 16 << 10, MaxEvents: 2, MaxGaps: 8, GapReserveBytes: 2048, MaxBatch: 2, CollectionEnabled: true}
	w := openSourceWAL(t, dir, options)
	now := time.Now().UTC().Truncate(time.Microsecond)
	first, err := w.AppendEvent(ctx, sourceEvent("11111111-aaaa-4aaa-8aaa-111111111111", now))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.AppendEvent(ctx, sourceEvent("22222222-aaaa-4aaa-8aaa-222222222222", now.Add(time.Microsecond))); err != nil {
		t.Fatal(err)
	}
	if _, err = w.AppendEvent(ctx, sourceEvent("33333333-aaaa-4aaa-8aaa-333333333333", now.Add(2*time.Microsecond))); !errors.Is(err, ErrFull) {
		t.Fatalf("event limit not enforced: %v", err)
	}
	gap, err := w.OpenGap(ctx, "wal-full", now.Add(3*time.Microsecond), 1)
	if err != nil || gap.Reason != "wal-full" || gap.DroppedCount != 1 {
		t.Fatalf("reserved gap: %+v %v", gap, err)
	}
	status, err := w.Status()
	if err != nil || status.PendingEvents != 2 || status.PendingGaps != 1 || !status.OpenGap || status.BytesUsed > status.MaxBytes {
		t.Fatalf("status: %+v %v", status, err)
	}
	if err = w.CloseGap(ctx, now.Add(4*time.Microsecond)); err != nil {
		t.Fatal(err)
	}
	batch, ok, err := w.NextBatch()
	if err != nil || !ok || len(batch.Gaps) != 1 || len(batch.Events) != 1 || batch.Events[0].EventID != first.EventID {
		t.Fatalf("bounded batch: %+v %v", batch, err)
	}
	bad := batch.Receipt()
	bad.EventIDs[0] = "99999999-aaaa-4aaa-8aaa-999999999999"
	if err = w.Ack(ctx, bad); !errors.Is(err, ErrAck) {
		t.Fatalf("mismatched ACK: %v", err)
	}
	receipt := batch.Receipt()
	if err = w.Ack(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	if err = w.Ack(ctx, receipt); err != nil {
		t.Fatalf("duplicate ACK was not idempotent: %v", err)
	}
	next, ok, err := w.NextBatch()
	if err != nil || !ok || len(next.Gaps) != 0 || len(next.Events) != 1 {
		t.Fatalf("next batch: %+v %v", next, err)
	}
}

func TestSourceGapValidationAccumulationAndClose(t *testing.T) {
	ctx := context.Background()
	w := openSourceWAL(t, filepath.Join(t.TempDir(), "source-wal"), Options{NodeID: "hk-01", CollectionEnabled: true})
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := w.OpenGap(ctx, "invented", now, 1); !errors.Is(err, ErrGap) {
		t.Fatalf("unknown reason: %v", err)
	}
	if _, err := w.OpenGap(ctx, "queue-full", time.Time{}, 1); !errors.Is(err, ErrGap) {
		t.Fatalf("zero time: %v", err)
	}
	first, err := w.OpenGap(ctx, "queue-full", now, 3)
	if err != nil {
		t.Fatal(err)
	}
	again, err := w.OpenGap(ctx, "queue-full", now.Add(time.Second), 2)
	if err != nil || again.Sequence != first.Sequence || again.DroppedCount != 5 {
		t.Fatalf("gap accumulation: %+v %v", again, err)
	}
	if err = w.CloseGap(ctx, now.Add(-time.Second)); !errors.Is(err, ErrGap) {
		t.Fatalf("backward close: %v", err)
	}
	if err = w.CloseGap(ctx, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err = w.CloseGap(ctx, now.Add(2*time.Second)); err != nil {
		t.Fatalf("duplicate close: %v", err)
	}
	batch, ok, err := w.NextBatch()
	if err != nil || !ok || len(batch.Gaps) != 1 || batch.Gaps[0].ClosedAt == nil || batch.Gaps[0].DroppedCount != 5 {
		t.Fatalf("closed gap batch: %+v %v", batch, err)
	}
}

func TestSourceWALUploadsOpenGapSnapshotsAndLaterClosure(t *testing.T) {
	ctx := context.Background()
	w := openSourceWAL(t, filepath.Join(t.TempDir(), "source-wal"), Options{NodeID: "hk-01", CollectionEnabled: true})
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := w.OpenGap(ctx, "queue-full", now, 2); err != nil {
		t.Fatal(err)
	}
	batch, ok, err := w.NextBatch()
	if err != nil || !ok || len(batch.Gaps) != 1 || batch.Gaps[0].ClosedAt != nil {
		t.Fatalf("open gap batch=%+v ok=%v err=%v", batch, ok, err)
	}
	if err = w.Ack(ctx, batch.Receipt()); err != nil {
		t.Fatal(err)
	}
	if batch, ok, err = w.NextBatch(); err != nil || ok {
		t.Fatalf("acknowledged snapshot repeated: %+v %v %v", batch, ok, err)
	}
	if _, err = w.OpenGap(ctx, "queue-full", now.Add(time.Second), 3); err != nil {
		t.Fatal(err)
	}
	batch, ok, err = w.NextBatch()
	if err != nil || !ok || batch.Gaps[0].DroppedCount != 5 || batch.Gaps[0].ClosedAt != nil {
		t.Fatalf("advanced open gap batch=%+v ok=%v err=%v", batch, ok, err)
	}
	if err = w.Ack(ctx, batch.Receipt()); err != nil {
		t.Fatal(err)
	}
	if err = w.CloseGap(ctx, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	batch, ok, err = w.NextBatch()
	if err != nil || !ok || batch.Gaps[0].ClosedAt == nil {
		t.Fatalf("closed gap batch=%+v ok=%v err=%v", batch, ok, err)
	}
	if err = w.Ack(ctx, batch.Receipt()); err != nil {
		t.Fatal(err)
	}
	status, err := w.Status()
	if err != nil || status.PendingGaps != 0 || status.OldestObservedAt != nil {
		t.Fatalf("final status=%+v err=%v", status, err)
	}
}

func TestSourceWALPrivatePathLockAndChangedIdentityFailClosed(t *testing.T) {
	if _, err := Open("relative", Options{NodeID: "hk-01"}); err == nil {
		t.Fatal("relative WAL path accepted")
	}
	dir := filepath.Join(t.TempDir(), "source-wal")
	w := openSourceWAL(t, dir, Options{NodeID: "hk-01"})
	if _, err := Open(dir, Options{NodeID: "hk-01"}); !errors.Is(err, ErrLocked) {
		t.Fatalf("second owner: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, Options{NodeID: "sg-01"}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("node identity changed: %v", err)
	}
	if _, err := Open(dir, Options{NodeID: "hk-01", MaxEvents: 3}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("limits changed: %v", err)
	}
	unsafe := filepath.Join(t.TempDir(), "unsafe")
	if err := os.Mkdir(unsafe, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(unsafe, Options{NodeID: "hk-01"}); err == nil {
		t.Fatal("public WAL directory accepted")
	}
}
