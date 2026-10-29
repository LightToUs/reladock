package metering

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWALUncertainWriteStopsUseAndRestartPreservesReplay(t *testing.T) {
	for _, afterRename := range []bool{false, true} {
		name := "before-rename"
		if afterRename {
			name = "after-rename"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			dir := filepath.Join(t.TempDir(), "wal")
			options := Options{NodeID: "hk-01"}
			w := openWAL(t, dir, options)
			if _, e := w.BeginEpoch(ctx, readings(ruleA, periodA, 7, 11)); e != nil {
				t.Fatal(e)
			}
			first, _, _ := w.Next()
			actual := w.persist
			fault := errors.New("injected storage failure")
			w.persist = func(ctx context.Context, b []byte) error {
				if afterRename {
					if e := actual(ctx, b); e != nil {
						return e
					}
				} else {
					if e := os.WriteFile(filepath.Join(dir, "wal.next"), []byte("partial"), 0600); e != nil {
						return e
					}
				}
				return fault
			}
			if e := w.Sample(ctx, readings(ruleA, periodA, 13, 17)); !errors.Is(e, ErrUncertain) || !errors.Is(e, fault) {
				t.Fatalf("storage error hidden: %v", e)
			}
			if _, _, e := w.Next(); !errors.Is(e, ErrUncertain) {
				t.Fatalf("uncertain state still used: %v", e)
			}
			if e := w.Close(); e != nil {
				t.Fatal(e)
			}
			w = openWAL(t, dir, options)
			head, _, e := w.Next()
			if e != nil || !bytes.Equal(head.Payload, first.Payload) {
				t.Fatalf("lost unacknowledged head: %+v %v", head, e)
			}
			if e = w.Ack(ctx, receipt(head)); e != nil {
				t.Fatal(e)
			}
			if afterRename {
				second, ok, e := w.Next()
				if e != nil || !ok || second.Report.Sequence != 2 || second.Report.UploadBytes != 13 {
					t.Fatalf("visible replacement not recovered: %+v %v", second, e)
				}
			} else {
				if _, ok, e := w.Next(); e != nil || ok {
					t.Fatalf("scratch promoted without commit: %v %v", ok, e)
				}
				if e = w.Sample(ctx, readings(ruleA, periodA, 13, 17)); e != nil {
					t.Fatal(e)
				}
				second, _, _ := w.Next()
				if second.Report.Sequence != 2 || second.Report.UploadBytes != 13 {
					t.Fatalf("retry cursor: %+v", second)
				}
			}
		})
	}
}

func TestWALACKDurabilityFailureNeverDropsUnacceptedData(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "wal")
	options := Options{NodeID: "hk-01"}
	w := openWAL(t, dir, options)
	if _, e := w.BeginEpoch(ctx, readings(ruleA, periodA, 3, 5)); e != nil {
		t.Fatal(e)
	}
	record, _, _ := w.Next()
	actual := w.persist
	w.persist = func(ctx context.Context, b []byte) error {
		if e := actual(ctx, b); e != nil {
			return e
		}
		return errors.New("directory sync uncertain")
	}
	// The sender has already observed the matching successful network ACK.
	if e := w.Flush(ctx, func(context.Context, []byte) (Receipt, error) { return receipt(record), nil }); !errors.Is(e, ErrUncertain) {
		t.Fatalf("ACK commit error hidden: %v", e)
	}
	if e := w.Close(); e != nil {
		t.Fatal(e)
	}
	w = openWAL(t, dir, options)
	if _, ok, e := w.Next(); e != nil || ok {
		t.Fatalf("committed acknowledged entry resurrected: %v %v", ok, e)
	}
}

func TestWALLocksUnsafeFilesCorruptionAndMissingStateFailClosed(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "wal")
	options := Options{NodeID: "hk-01"}
	w := openWAL(t, dir, options)
	if _, e := Open(dir, options); !errors.Is(e, ErrLocked) {
		t.Fatalf("two WAL owners: %v", e)
	}
	if _, e := w.BeginEpoch(ctx, readings(ruleA, periodA, 3, 5)); e != nil {
		t.Fatal(e)
	}
	if e := w.Close(); e != nil {
		t.Fatal(e)
	}
	if _, e := Open(dir, Options{NodeID: "sg-02"}); !errors.Is(e, ErrCorrupt) {
		t.Fatalf("node identity changed: %v", e)
	}
	if _, e := Open(dir, Options{NodeID: "hk-01", MaxPending: 2}); !errors.Is(e, ErrCorrupt) {
		t.Fatalf("capacity changed silently: %v", e)
	}
	path := filepath.Join(dir, "wal.json")
	original, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	corrupt := append([]byte(nil), original...)
	corrupt[len(corrupt)/2] ^= 1
	if e = os.WriteFile(path, corrupt, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = Open(dir, options); !errors.Is(e, ErrCorrupt) {
		t.Fatalf("corrupt WAL was reset: %v", e)
	}
	after, e := os.ReadFile(path)
	if e != nil || !bytes.Equal(after, corrupt) {
		t.Fatal("corrupt history overwritten")
	}
	if e = os.Remove(path); e != nil {
		t.Fatal(e)
	}
	if _, e = Open(dir, options); !errors.Is(e, ErrCorrupt) {
		t.Fatalf("missing history adopted as new: %v", e)
	}
	outside := filepath.Join(t.TempDir(), "private-data")
	if e = os.WriteFile(outside, []byte("sentinel"), 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.Symlink(outside, path); e != nil {
		t.Fatal(e)
	}
	if _, e = Open(dir, options); e == nil {
		t.Fatal("symlink accepted")
	}
	b, e := os.ReadFile(outside)
	if e != nil || string(b) != "sentinel" {
		t.Fatal("external file touched")
	}
}

func TestWALRejectsHardlinkedScratchWithoutModifyingOtherFile(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "wal")
	w := openWAL(t, dir, Options{NodeID: "hk-01"})
	outside := filepath.Join(t.TempDir(), "outside")
	if e := os.WriteFile(outside, []byte("sentinel"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.Link(outside, filepath.Join(dir, "wal.next")); e != nil {
		t.Fatal(e)
	}
	if _, e := w.BeginEpoch(ctx, readings(ruleA, periodA, 3, 5)); !errors.Is(e, ErrUncertain) {
		t.Fatalf("hardlinked scratch accepted: %v", e)
	}
	data, e := os.ReadFile(outside)
	if e != nil || string(data) != "sentinel" {
		t.Fatal("hardlink target modified")
	}
}

func TestWALMissingBaselineOrOldFormatNeverGuessesZero(t *testing.T) {
	for _, oldFormat := range []bool{false, true} {
		label := "missing-baseline"
		if oldFormat {
			label = "old-format"
		}
		t.Run(label, func(t *testing.T) {
			ctx := context.Background()
			dir := filepath.Join(t.TempDir(), "wal")
			o := Options{NodeID: "hk-01"}
			w := openWAL(t, dir, o)
			if _, e := w.BeginEpoch(ctx, readings(ruleA, periodA, 3, 5)); e != nil {
				t.Fatal(e)
			}
			state := clone(w.state)
			state.Totals[0].Baseline = nil
			if oldFormat {
				state.Version = 2
			}
			if e := w.Close(); e != nil {
				t.Fatal(e)
			}
			// Even a structurally valid checksum cannot make absent baseline facts safe.
			payload, e := encodeSnapshot(state)
			if e != nil {
				t.Fatal(e)
			}
			path := filepath.Join(dir, "wal.json")
			if e = os.WriteFile(path, payload, 0600); e != nil {
				t.Fatal(e)
			}
			if _, e = Open(dir, o); !errors.Is(e, ErrCorrupt) {
				t.Fatalf("old usage reset to zero: %v", e)
			}
			after, e := os.ReadFile(path)
			if e != nil || !bytes.Equal(after, payload) {
				t.Fatal("unreconciled old WAL was overwritten")
			}
		})
	}
}
