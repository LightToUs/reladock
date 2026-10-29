package metering

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"testing"
)

func TestCumulativeSurvivesPhysicalResetACKPruneAndRestart(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "wal")
	options := Options{NodeID: "hk-01"}
	w := openWAL(t, dir, options)
	check := func(w *WAL, rule, period string, wantUp, wantDown int64) {
		t.Helper()
		up, down, e := w.CumulativeFor(ctx, rule, period)
		if e != nil || up != wantUp || down != wantDown {
			t.Fatalf("%s/%s totals %d/%d want %d/%d: %v", rule, period, up, down, wantUp, wantDown, e)
		}
	}
	drain := func(w *WAL) {
		t.Helper()
		for {
			r, ok, e := w.Next()
			if e != nil {
				t.Fatal(e)
			}
			if !ok {
				return
			}
			if e = w.Ack(ctx, receipt(r)); e != nil {
				t.Fatal(e)
			}
		}
	}
	if _, e := w.BeginEpochFor(ctx, "apply-1", readings(ruleA, periodA, 100, 200)); e != nil {
		t.Fatal(e)
	}
	if e := w.Sample(ctx, readings(ruleA, periodA, 130, 250)); e != nil {
		t.Fatal(e)
	}
	check(w, ruleA, periodA, 130, 250)
	drain(w)
	if _, e := w.BeginEpochFor(ctx, "apply-2", readings(ruleA, periodA, 5, 7)); e != nil {
		t.Fatal(e)
	}
	check(w, ruleA, periodA, 135, 257)
	if _, e := w.BeginEpochFor(ctx, "apply-2", readings(ruleA, periodA, 5, 7)); e != nil {
		t.Fatal(e)
	}
	check(w, ruleA, periodA, 135, 257)
	if _, e := w.BeginEpochFor(ctx, "apply-2", readings(ruleA, periodA, 8, 9)); e != nil {
		t.Fatal(e)
	}
	check(w, ruleA, periodA, 138, 259)
	drain(w)
	if e := w.Close(); e != nil {
		t.Fatal(e)
	}
	w = openWAL(t, dir, options)
	check(w, ruleA, periodA, 138, 259)
	if _, e := w.BeginEpochFor(ctx, "apply-3", readings(ruleA, periodB, 11, 13)); e != nil {
		t.Fatal(e)
	}
	check(w, ruleA, periodA, 138, 259)
	check(w, ruleA, periodB, 11, 13)
	check(w, ruleB, periodB, 0, 0)
	drain(w)
	if _, e := w.BeginEpochFor(ctx, "apply-4", nil); e != nil {
		t.Fatal(e)
	}
	check(w, ruleA, periodA, 138, 259)
	check(w, ruleA, periodB, 11, 13)
	if e := w.Close(); e != nil {
		t.Fatal(e)
	}
	w = openWAL(t, dir, options)
	check(w, ruleA, periodA, 138, 259)
	check(w, ruleA, periodB, 11, 13)
}

func TestCumulativePeriodLimitNeverEvictsAcknowledgedTotals(t *testing.T) {
	ctx := context.Background()
	w := openWAL(t, filepath.Join(t.TempDir(), "wal"), Options{NodeID: "hk-01", MaxStreams: 1})
	if _, e := w.BeginEpochFor(ctx, "first", readings(ruleA, periodA, 3, 5)); e != nil {
		t.Fatal(e)
	}
	r, _, _ := w.Next()
	if e := w.Ack(ctx, receipt(r)); e != nil {
		t.Fatal(e)
	}
	if _, e := w.BeginEpochFor(ctx, "second", readings(ruleA, periodB, 7, 11)); !errors.Is(e, ErrFull) {
		t.Fatalf("old period evicted: %v", e)
	}
	up, down, e := w.CumulativeFor(ctx, ruleA, periodA)
	if e != nil || up != 3 || down != 5 {
		t.Fatalf("old quota reset: %d/%d %v", up, down, e)
	}
	up, down, e = w.CumulativeFor(ctx, ruleA, periodB)
	if e != nil || up != 0 || down != 0 {
		t.Fatalf("failed period partially added: %d/%d %v", up, down, e)
	}
	// Another physical reset within the existing period needs no new total entry.
	if _, e = w.BeginEpochFor(ctx, "same-period-reset", readings(ruleA, periodA, 7, 11)); e != nil {
		t.Fatal(e)
	}
	up, down, e = w.CumulativeFor(ctx, ruleA, periodA)
	if e != nil || up != 10 || down != 16 {
		t.Fatalf("existing-period reset failed: %d/%d %v", up, down, e)
	}
}

func TestCumulativeOverflowDoesNotCommitEpochOrSample(t *testing.T) {
	ctx := context.Background()
	w := openWAL(t, filepath.Join(t.TempDir(), "wal"), Options{NodeID: "hk-01"})
	old, e := w.BeginEpochFor(ctx, "first", readings(ruleA, periodA, math.MaxInt64-1, 0))
	if e != nil {
		t.Fatal(e)
	}
	r, _, _ := w.Next()
	if e = w.Ack(ctx, receipt(r)); e != nil {
		t.Fatal(e)
	}
	if _, e = w.BeginEpochFor(ctx, "overflow", readings(ruleA, periodA, 2, 0)); e == nil {
		t.Fatal("cumulative int64 overflow accepted")
	}
	epoch, _, e := w.Current()
	if e != nil || epoch != old {
		t.Fatalf("failed reset advanced epoch: %s %v", epoch, e)
	}
	up, down, e := w.CumulativeFor(ctx, ruleA, periodA)
	if e != nil || up != math.MaxInt64-1 || down != 0 {
		t.Fatalf("overflow damaged totals: %d/%d %v", up, down, e)
	}
	if e = w.Sample(ctx, readings(ruleA, periodA, math.MaxInt64, 0)); e != nil {
		t.Fatal(e)
	}
	up, _, e = w.CumulativeFor(ctx, ruleA, periodA)
	if e != nil || up != math.MaxInt64 {
		t.Fatalf("valid boundary rejected: %d %v", up, e)
	}
}

func TestBindPeriodFixedBaselineDoesNotReaddAcknowledgedLocalUsage(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "wal")
	o := Options{NodeID: "hk-01"}
	w := openWAL(t, dir, o)
	if e := w.BindPeriod(ctx, ruleA, periodA, 100, 200); e != nil {
		t.Fatal(e)
	}
	if _, e := w.BeginEpochFor(ctx, "first", readings(ruleA, periodA, 5, 7)); e != nil {
		t.Fatal(e)
	}
	up, down, e := w.CumulativeFor(ctx, ruleA, periodA)
	if e != nil || up != 105 || down != 207 {
		t.Fatalf("platform baseline not included: %d/%d %v", up, down, e)
	}
	r, _, _ := w.Next()
	if r.Report.UploadBytes != 5 || r.Report.DownloadBytes != 7 {
		t.Fatalf("baseline leaked into billable report: %+v", r.Report)
	}
	if e = w.Ack(ctx, receipt(r)); e != nil {
		t.Fatal(e)
	}
	// This later controller snapshot already includes the just-ACKed 5/7 bytes.
	if e = w.BindPeriod(ctx, ruleA, periodA, 105, 207); e != nil {
		t.Fatal(e)
	}
	up, down, e = w.CumulativeFor(ctx, ruleA, periodA)
	if e != nil || up != 105 || down != 207 {
		t.Fatalf("snapshot rebinding double-counted local usage: %d/%d %v", up, down, e)
	}
	if e = w.Close(); e != nil {
		t.Fatal(e)
	}
	w = openWAL(t, dir, o)
	if e = w.BindPeriod(ctx, ruleA, periodA, 999, 999); e != nil {
		t.Fatal(e)
	}
	up, down, e = w.CumulativeFor(ctx, ruleA, periodA)
	if e != nil || up != 105 || down != 207 {
		t.Fatalf("restart changed fixed baseline: %d/%d %v", up, down, e)
	}
	if _, e = w.BeginEpochFor(ctx, "reset", readings(ruleA, periodA, 3, 4)); e != nil {
		t.Fatal(e)
	}
	up, down, e = w.CumulativeFor(ctx, ruleA, periodA)
	if e != nil || up != 108 || down != 211 {
		t.Fatalf("reset quota credit: %d/%d %v", up, down, e)
	}
	if e = w.BindPeriod(ctx, ruleA, periodB, 11, 13); e != nil {
		t.Fatal(e)
	}
	up, down, e = w.CumulativeFor(ctx, ruleA, periodB)
	if e != nil || up != 11 || down != 13 {
		t.Fatalf("periods mixed: %d/%d %v", up, down, e)
	}
}

func TestBindPeriodDefaultZeroIsFixedAndBaselinesAreBounded(t *testing.T) {
	ctx := context.Background()
	w := openWAL(t, filepath.Join(t.TempDir(), "wal"), Options{NodeID: "hk-01", MaxStreams: 1})
	if _, e := w.BeginEpoch(ctx, readings(ruleA, periodA, 3, 5)); e != nil {
		t.Fatal(e)
	}
	if e := w.BindPeriod(ctx, ruleA, periodA, 100, 200); e != nil {
		t.Fatal(e)
	}
	up, down, e := w.CumulativeFor(ctx, ruleA, periodA)
	if e != nil || up != 3 || down != 5 {
		t.Fatalf("late bind changed implicit zero baseline: %d/%d %v", up, down, e)
	}
	if e = w.BindPeriod(ctx, ruleA, periodB, 7, 11); !errors.Is(e, ErrFull) {
		t.Fatalf("unbounded baseline entries: %v", e)
	}
	for _, pair := range [][2]int64{{-1, 0}, {0, -1}, {math.MaxInt64, 1}} {
		if e = w.BindPeriod(ctx, ruleA, periodA, pair[0], pair[1]); e == nil {
			t.Fatalf("invalid baseline: %v", pair)
		}
	}
}

func TestBindPeriodBeforeFirstEpochSurvivesRestartAndStorageFailure(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "wal")
	o := Options{NodeID: "hk-01"}
	w := openWAL(t, dir, o)
	if e := w.BindPeriod(ctx, ruleA, periodA, 100, 200); e != nil {
		t.Fatal(e)
	}
	if e := w.Close(); e != nil {
		t.Fatal(e)
	}
	w = openWAL(t, dir, o)
	up, down, e := w.CumulativeFor(ctx, ruleA, periodA)
	if e != nil || up != 100 || down != 200 {
		t.Fatalf("pre-epoch baseline missing: %d/%d %v", up, down, e)
	}
	w.persist = func(context.Context, []byte) error { return errors.New("storage unavailable") }
	if e = w.BindPeriod(ctx, ruleA, periodB, 11, 13); !errors.Is(e, ErrUncertain) {
		t.Fatalf("failed binding succeeded: %v", e)
	}
	if e = w.Close(); e != nil {
		t.Fatal(e)
	}
	w = openWAL(t, dir, o)
	up, down, e = w.CumulativeFor(ctx, ruleA, periodB)
	if e != nil || up != 0 || down != 0 {
		t.Fatalf("failed baseline committed: %d/%d %v", up, down, e)
	}
	if e = w.BindPeriod(ctx, ruleA, periodB, 17, 19); e != nil {
		t.Fatal(e)
	}
	up, down, e = w.CumulativeFor(ctx, ruleA, periodB)
	if e != nil || up != 17 || down != 19 {
		t.Fatalf("retry baseline not fixed: %d/%d %v", up, down, e)
	}
}

func TestBindPeriodAndLocalCountersCannotOverflowCombinedUsage(t *testing.T) {
	ctx := context.Background()
	w := openWAL(t, filepath.Join(t.TempDir(), "wal"), Options{NodeID: "hk-01"})
	if e := w.BindPeriod(ctx, ruleA, periodA, math.MaxInt64-5, 0); e != nil {
		t.Fatal(e)
	}
	if _, e := w.BeginEpochFor(ctx, "first", readings(ruleA, periodA, 3, 0)); e != nil {
		t.Fatal(e)
	}
	if e := w.Sample(ctx, readings(ruleA, periodA, 6, 0)); !errors.Is(e, ErrFull) {
		t.Fatalf("baseline+local overflow accepted: %v", e)
	}
	up, _, e := w.CumulativeFor(ctx, ruleA, periodA)
	if e != nil || up != math.MaxInt64-2 {
		t.Fatalf("overflow mutated effective usage: %d %v", up, e)
	}
	if e = w.Sample(ctx, readings(ruleA, periodA, 4, 0)); e != nil {
		t.Fatal(e)
	}
	up, _, e = w.CumulativeFor(ctx, ruleA, periodA)
	if e != nil || up != math.MaxInt64-1 {
		t.Fatalf("valid retry after overflow: %d %v", up, e)
	}
}
