package metering

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

func TestWALIndividualResetCannotHideBehindOtherProtocolGrowth(t *testing.T) {
	ctx := context.Background()
	w := openWAL(t, filepath.Join(t.TempDir(), "wal"), Options{NodeID: "hk-01"})
	initial := readings(ruleA, periodA, 100, 200)
	udp := readings(ruleA, periodA, 300, 400)
	for i := range udp {
		udp[i].Name += "_udp"
		udp[i].Protocol = "udp"
	}
	initial = append(initial, udp...)
	if _, e := w.BeginEpoch(ctx, initial); e != nil {
		t.Fatal(e)
	}
	changed := append([]Reading(nil), initial...)
	changed[0].Bytes--
	changed[2].Bytes += 100
	if e := w.Sample(ctx, changed); !errors.Is(e, ErrCounterReset) {
		t.Fatalf("TCP reset hidden by UDP growth: %v", e)
	}
	changed = append([]Reading(nil), initial...)
	changed[0].Bytes++
	changed[0].Packets--
	if e := w.Sample(ctx, changed); !errors.Is(e, ErrCounterReset) {
		t.Fatalf("packet reset hidden by bytes: %v", e)
	}
}

func TestWALByteBoundAndClosedCursorCompaction(t *testing.T) {
	ctx := context.Background()
	options := Options{NodeID: "hk-01", MaxBytes: 4096, MaxPending: 1000, MaxStreams: 2}
	dir := filepath.Join(t.TempDir(), "wal")
	w := openWAL(t, dir, options)
	if _, e := w.BeginEpoch(ctx, readings(ruleA, periodA, 0, 0)); e != nil {
		t.Fatal(e)
	}
	blocked := int64(0)
	accepted := int64(1)
	for i := int64(1); i < 100; i++ {
		e := w.Sample(ctx, readings(ruleA, periodA, i, i))
		if errors.Is(e, ErrFull) {
			blocked = i
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		accepted++
	}
	if blocked == 0 {
		t.Fatal("byte capacity never enforced")
	}
	files, e := os.ReadDir(dir)
	if e != nil {
		t.Fatal(e)
	}
	for _, f := range files {
		info, e := f.Info()
		if e != nil || info.Size() > options.MaxBytes {
			t.Fatalf("unbounded disk file: %s %v", f.Name(), e)
		}
	}
	var received []Report
	if e = w.Flush(ctx, func(_ context.Context, payload []byte) (Receipt, error) {
		var r Report
		if e := json.Unmarshal(payload, &r); e != nil {
			return Receipt{}, e
		}
		received = append(received, r)
		return Receipt{r.RuleID, r.SubscriptionID, r.Epoch, r.Sequence}, nil
	}); e != nil {
		t.Fatal(e)
	}
	if int64(len(received)) != accepted || received[len(received)-1].Sequence != accepted {
		t.Fatalf("byte pressure dropped events: %+v", received)
	}
	if e = w.Sample(ctx, readings(ruleA, periodA, blocked, blocked)); e != nil {
		t.Fatal(e)
	}
	next, _, _ := w.Next()
	if next.Report.Sequence != accepted+1 || next.Report.UploadBytes != blocked {
		t.Fatalf("capacity failure advanced cursor: %+v", next)
	}
	if e = w.Ack(ctx, receipt(next)); e != nil {
		t.Fatal(e)
	}
	previous := ""
	for i := 0; i < 20; i++ {
		epoch, e := w.BeginEpoch(ctx, readings(ruleA, periodA, 0, 0))
		if e != nil {
			t.Fatal(e)
		}
		if epoch == previous {
			t.Fatal("epoch reused after compaction")
		}
		previous = epoch
		head, _, _ := w.Next()
		if e = w.Ack(ctx, receipt(head)); e != nil {
			t.Fatal(e)
		}
	}
	if e = w.Close(); e != nil {
		t.Fatal(e)
	}
	w = openWAL(t, dir, options)
	epoch, e := w.BeginEpoch(ctx, readings(ruleA, periodA, 0, 0))
	if e != nil || epoch == previous {
		t.Fatalf("restart reused epoch: %s %v", epoch, e)
	}
}

func TestWALMultiRuleSampleIsAtomicAndConcurrentDuplicatesAreNotQueued(t *testing.T) {
	ctx := context.Background()
	w := openWAL(t, filepath.Join(t.TempDir(), "wal"), Options{NodeID: "hk-01"})
	values := append(readings(ruleA, periodA, 10, 20), readings(ruleB, periodB, 30, 40)...)
	if _, e := w.BeginEpoch(ctx, values); e != nil {
		t.Fatal(e)
	}
	// One bad rule must not persist another rule's earlier update in the batch.
	bad := append([]Reading(nil), values...)
	bad[0].Bytes++
	bad[2].Bytes--
	if e := w.Sample(ctx, bad); !errors.Is(e, ErrCounterReset) {
		t.Fatal(e)
	}
	values[0].Bytes++
	values[2].Bytes++
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- w.Sample(ctx, values) }()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	var reports []Report
	if e := w.Flush(ctx, func(_ context.Context, b []byte) (Receipt, error) {
		var r Report
		if e := json.Unmarshal(b, &r); e != nil {
			return Receipt{}, e
		}
		reports = append(reports, r)
		return Receipt{r.RuleID, r.SubscriptionID, r.Epoch, r.Sequence}, nil
	}); e != nil {
		t.Fatal(e)
	}
	if len(reports) != 4 || reports[2].Sequence != 2 || reports[2].UploadBytes != 11 || reports[3].UploadBytes != 31 {
		t.Fatalf("partial/duplicate sample: %+v", reports)
	}
}

func TestWALSampleWhileSendingAndCancellationKeepOrderedPayloads(t *testing.T) {
	ctx := context.Background()
	w := openWAL(t, filepath.Join(t.TempDir(), "wal"), Options{NodeID: "hk-01"})
	if _, e := w.BeginEpoch(ctx, readings(ruleA, periodA, 1, 2)); e != nil {
		t.Fatal(e)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	var received []Report
	go func() {
		done <- w.Flush(ctx, func(_ context.Context, b []byte) (Receipt, error) {
			var r Report
			if e := json.Unmarshal(b, &r); e != nil {
				return Receipt{}, e
			}
			if len(received) == 0 {
				close(entered)
				<-release
			}
			received = append(received, r)
			return Receipt{r.RuleID, r.SubscriptionID, r.Epoch, r.Sequence}, nil
		})
	}()
	<-entered
	if e := w.Sample(ctx, readings(ruleA, periodA, 3, 4)); e != nil {
		t.Fatal(e)
	}
	close(release)
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	if len(received) != 2 || received[0].Sequence != 1 || received[1].Sequence != 2 {
		t.Fatalf("send ordering: %+v", received)
	}
	if e := w.Sample(ctx, readings(ruleA, periodA, 5, 6)); e != nil {
		t.Fatal(e)
	}
	head, _, _ := w.Next()
	canceled, cancel := context.WithCancel(ctx)
	if e := w.Flush(canceled, func(context.Context, []byte) (Receipt, error) { cancel(); return receipt(head), nil }); !errors.Is(e, context.Canceled) {
		t.Fatalf("canceled ACK commit: %v", e)
	}
	replay, _, _ := w.Next()
	if !bytes.Equal(head.Payload, replay.Payload) {
		t.Fatal("cancellation lost an uncertain network result")
	}
}

func TestWALCrashHelper(t *testing.T) {
	dir := os.Getenv("RELADOCK_METERING_CRASH_DIR")
	if dir == "" {
		t.Skip("subprocess-only crash fixture")
	}
	w, e := Open(dir, Options{NodeID: "hk-01"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = w.BeginEpochFor(context.Background(), "apply-crash", readings(ruleA, periodA, 7, 11)); e != nil {
		t.Fatal(e)
	}
	head, _, e := w.Next()
	if e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stdout.Write(head.Payload); e != nil {
		t.Fatal(e)
	}
	os.Exit(0) // Deliberately skip Close and Go cleanups, as on abrupt process exit.
}

func TestWALProcessExitReleasesLockAndReplaysExactPayload(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "wal")
	command := exec.Command(os.Args[0], "-test.run=^TestWALCrashHelper$")
	command.Env = append(os.Environ(), "RELADOCK_METERING_CRASH_DIR="+dir)
	payload, e := command.Output()
	if e != nil {
		t.Fatal(e)
	}
	w := openWAL(t, dir, Options{NodeID: "hk-01"})
	head, ok, e := w.Next()
	if e != nil || !ok || !bytes.Equal(payload, head.Payload) {
		t.Fatalf("process restart replay: %s %v", head.Payload, e)
	}
	epoch, token, e := w.Current()
	if e != nil || epoch != head.Report.Epoch || token != "apply-crash" {
		t.Fatalf("lost incarnation: %s %s %v", epoch, token, e)
	}
}
