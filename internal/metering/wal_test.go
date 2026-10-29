package metering

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func openWAL(t *testing.T, dir string, o Options) *WAL {
	t.Helper()
	w, e := Open(dir, o)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}
func receipt(r Record) Receipt {
	return Receipt{RuleID: r.Report.RuleID, SubscriptionID: r.Report.SubscriptionID, Epoch: r.Report.Epoch, Sequence: r.Report.Sequence}
}

func TestWALRestartExactPayloadSequenceAndACK(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "wal")
	o := Options{NodeID: "hk-01"}
	w := openWAL(t, dir, o)
	epoch, e := w.BeginEpochFor(ctx, "apply-1", readings(ruleA, periodA, 7, 11))
	if e != nil {
		t.Fatal(e)
	}
	first, ok, e := w.Next()
	if e != nil || !ok || first.Report.Sequence != 1 || first.Report.Epoch != epoch || first.Report.UploadBytes != 7 || first.Report.DownloadBytes != 11 {
		t.Fatalf("first: %+v %v", first, e)
	}
	if e = w.Sample(ctx, readings(ruleA, periodA, 13, 17)); e != nil {
		t.Fatal(e)
	}
	if e = w.Close(); e != nil {
		t.Fatal(e)
	}
	w = openWAL(t, dir, o)
	again, ok, e := w.Next()
	if e != nil || !ok || !bytes.Equal(first.Payload, again.Payload) {
		t.Fatalf("restart changed payload: %s %v", again.Payload, e)
	}
	bad := receipt(first)
	bad.SubscriptionID = periodB
	if e = w.Ack(ctx, bad); !errors.Is(e, ErrAck) {
		t.Fatalf("conflicting ACK: %v", e)
	}
	if e = w.Ack(ctx, receipt(first)); e != nil {
		t.Fatal(e)
	}
	second, ok, e := w.Next()
	if e != nil || !ok || second.Report.Sequence != 2 || second.Report.UploadBytes != 13 {
		t.Fatalf("second: %+v %v", second, e)
	}
	if e = w.Ack(ctx, receipt(second)); e != nil {
		t.Fatal(e)
	}
	if e = w.Sample(ctx, readings(ruleA, periodA, 19, 23)); e != nil {
		t.Fatal(e)
	}
	third, _, e := w.Next()
	if e != nil || third.Report.Sequence != 3 {
		t.Fatalf("ACK lost durable cursor: %+v %v", third, e)
	}
}

func TestWALCounterResetNeedsExplicitEpochAndOldPeriodRemainsBound(t *testing.T) {
	ctx := context.Background()
	w := openWAL(t, filepath.Join(t.TempDir(), "wal"), Options{NodeID: "hk-01"})
	oldEpoch, e := w.BeginEpochFor(ctx, "apply-1", readings(ruleA, periodA, 100, 200))
	if e != nil {
		t.Fatal(e)
	}
	if e = w.Sample(ctx, readings(ruleA, periodA, 99, 201)); !errors.Is(e, ErrCounterReset) {
		t.Fatalf("counter reset accepted: %v", e)
	}
	if e = w.Sample(ctx, readings(ruleA, periodB, 101, 201)); !errors.Is(e, ErrEpochRequired) {
		t.Fatalf("period relabel accepted: %v", e)
	}
	newEpoch, e := w.BeginEpochFor(ctx, "apply-2", readings(ruleA, periodB, 3, 5))
	if e != nil || newEpoch == oldEpoch {
		t.Fatalf("explicit reset: %s %v", newEpoch, e)
	}
	old, _, e := w.Next()
	if e != nil || old.Report.SubscriptionID != periodA || old.Report.UploadBytes != 100 {
		t.Fatalf("buffered old period relabeled: %+v %v", old, e)
	}
	if e = w.Ack(ctx, receipt(old)); e != nil {
		t.Fatal(e)
	}
	fresh, _, e := w.Next()
	if e != nil || fresh.Report.SubscriptionID != periodB || fresh.Report.UploadBytes != 3 || fresh.Report.Sequence != 1 {
		t.Fatalf("new period: %+v %v", fresh, e)
	}
	// Retrying the same physical incarnation must sample, not count it twice.
	same, e := w.BeginEpochFor(ctx, "apply-2", readings(ruleA, periodB, 3, 5))
	if e != nil || same != newEpoch {
		t.Fatalf("retry started another epoch: %s %v", same, e)
	}
	if e = w.Ack(ctx, receipt(fresh)); e != nil {
		t.Fatal(e)
	}
	if _, ok, e := w.Next(); e != nil || ok {
		t.Fatalf("duplicate incarnation queued twice: %v %v", ok, e)
	}
	for _, token := range []string{"", "\xff", "unsafe\x00token"} {
		if _, e := w.BeginEpochFor(ctx, token, readings(ruleA, periodB, 3, 5)); !errors.Is(e, ErrEpochRequired) {
			t.Fatalf("invalid incarnation accepted: %q %v", token, e)
		}
	}
}

func TestWALBoundedBackpressureNeverDiscardsUnacknowledgedReports(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "wal")
	o := Options{NodeID: "hk-01", MaxBytes: 8192, MaxPending: 2, MaxStreams: 4}
	w := openWAL(t, dir, o)
	if _, e := w.BeginEpoch(ctx, readings(ruleA, periodA, 1, 1)); e != nil {
		t.Fatal(e)
	}
	if e := w.Sample(ctx, readings(ruleA, periodA, 2, 2)); e != nil {
		t.Fatal(e)
	}
	if e := w.Sample(ctx, readings(ruleA, periodA, 3, 3)); !errors.Is(e, ErrFull) {
		t.Fatalf("unbounded queue: %v", e)
	}
	first, _, _ := w.Next()
	if e := w.Ack(ctx, receipt(first)); e != nil {
		t.Fatal(e)
	}
	if e := w.Sample(ctx, readings(ruleA, periodA, 3, 3)); e != nil {
		t.Fatal(e)
	}
	second, _, _ := w.Next()
	if second.Report.Sequence != 2 || second.Report.UploadBytes != 2 {
		t.Fatalf("lost pending event: %+v", second)
	}
	info, e := os.Stat(filepath.Join(dir, "wal.json"))
	if e != nil || info.Size() > o.MaxBytes {
		t.Fatalf("disk limit: %v %v", info, e)
	}
	if e = w.Close(); e != nil {
		t.Fatal(e)
	}
	w = openWAL(t, dir, o)
	restored, _, _ := w.Next()
	if !bytes.Equal(second.Payload, restored.Payload) {
		t.Fatal("capacity failure changed payload")
	}
}

func TestWALFlushRetainsOnNetworkFailureAndValidatesACK(t *testing.T) {
	ctx := context.Background()
	w := openWAL(t, filepath.Join(t.TempDir(), "wal"), Options{NodeID: "hk-01"})
	if _, e := w.BeginEpoch(ctx, readings(ruleA, periodA, 7, 11)); e != nil {
		t.Fatal(e)
	}
	record, _, _ := w.Next()
	network := errors.New("lost response")
	if e := w.Flush(ctx, func(context.Context, []byte) (Receipt, error) { return Receipt{}, network }); !errors.Is(e, network) {
		t.Fatal(e)
	}
	replay, _, _ := w.Next()
	if !bytes.Equal(record.Payload, replay.Payload) {
		t.Fatal("network failure lost WAL")
	}
	if e := w.Flush(ctx, func(context.Context, []byte) (Receipt, error) { r := receipt(record); r.Sequence++; return r, nil }); !errors.Is(e, ErrAck) {
		t.Fatalf("wrong response dropped WAL: %v", e)
	}
	if e := w.Flush(ctx, func(_ context.Context, p []byte) (Receipt, error) {
		if !bytes.Equal(p, record.Payload) {
			t.Error("payload changed")
		}
		return receipt(record), nil
	}); e != nil {
		t.Fatal(e)
	}
	if _, ok, e := w.Next(); e != nil || ok {
		t.Fatalf("accepted event not removed: %v %v", ok, e)
	}
}

func TestWALIdentityIsDurableAndDifferentForNewDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "wal")
	options := Options{NodeID: "hk-01"}
	w := openWAL(t, dir, options)
	first, err := w.Identity()
	if err != nil || first == "" {
		t.Fatal("new WAL must expose a nonempty durable identity")
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	if id, e := w.Identity(); id != "" || !errors.Is(e, ErrClosed) {
		t.Fatal("closed WAL exposed an identity")
	}
	w = openWAL(t, dir, options)
	restored, err := w.Identity()
	if err != nil || restored != first {
		t.Fatal("WAL identity changed across restart")
	}
	other := openWAL(t, filepath.Join(t.TempDir(), "wal"), options)
	fresh, err := other.Identity()
	if err != nil || fresh == "" || fresh == first {
		t.Fatal("new directory must have a different WAL identity")
	}
	w.persist = func(context.Context, []byte) error { return errors.New("storage unavailable") }
	if err = w.BindPeriod(context.Background(), ruleA, periodA, 0, 0); !errors.Is(err, ErrUncertain) {
		t.Fatal("fixture did not enter uncertain state")
	}
	if id, e := w.Identity(); id != "" || !errors.Is(e, ErrUncertain) {
		t.Fatal("uncertain WAL exposed an identity")
	}
}
