package sourceflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSourceRecoveryAddsGapAfterUncleanEnabledRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "source-wal")
	options := Options{NodeID: "hk-01", CollectionEnabled: true}
	w := openSourceWAL(t, dir, options)
	before, err := w.Status()
	if err != nil {
		t.Fatal(err)
	}
	// Simulate process death: release only the OS lock without writing a clean
	// shutdown checkpoint.
	if err = w.unlock(); err != nil {
		t.Fatal(err)
	}
	w.closed = true
	restarted := openSourceWAL(t, dir, options)
	after, err := restarted.Status()
	if err != nil || after.CollectorEpoch == before.CollectorEpoch || !after.OpenGap || after.PendingGaps != 1 {
		t.Fatalf("restart status before=%+v after=%+v err=%v", before, after, err)
	}
	if err = restarted.CloseGap(context.Background(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	batch, ok, err := restarted.NextBatch()
	if err != nil || !ok || len(batch.Gaps) != 1 || batch.Gaps[0].Reason != "collector-restart" {
		t.Fatalf("restart gap: %+v %v", batch, err)
	}
}

func TestSourceRecoveryCleanDisabledStopCreatesNoGap(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "source-wal")
	options := Options{NodeID: "hk-01", CollectionEnabled: false}
	w := openSourceWAL(t, dir, options)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := openSourceWAL(t, dir, options)
	status, err := restarted.Status()
	if err != nil || status.OpenGap || status.PendingGaps != 0 {
		t.Fatalf("clean disabled restart: %+v %v", status, err)
	}
}

func TestSourceRecoveryRejectsTrailingOrTamperedSnapshot(t *testing.T) {
	for _, mutate := range []func(string) error{
		func(path string) error {
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = f.WriteString(" trailing")
			return err
		},
		func(path string) error {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			data[len(data)/2] ^= 1
			return os.WriteFile(path, data, 0600)
		},
	} {
		dir := filepath.Join(t.TempDir(), "source-wal")
		w := openSourceWAL(t, dir, Options{NodeID: "hk-01"})
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if err := mutate(filepath.Join(dir, "wal.json")); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(dir, Options{NodeID: "hk-01"}); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("corrupt snapshot accepted: %v", err)
		}
	}
}

func TestSourceWALRestartPreservesEventsGapsAndBatchBounds(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "source-wal")
	options := Options{NodeID: "hk-01", MaxBatch: 1, CollectionEnabled: true}
	w := openSourceWAL(t, dir, options)
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := w.AppendEvent(context.Background(), sourceEvent("11111111-aaaa-4aaa-8aaa-111111111111", now)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.OpenGap(context.Background(), "collector-error", now, 4); err != nil {
		t.Fatal(err)
	}
	if err := w.CloseGap(context.Background(), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	w = openSourceWAL(t, dir, options)
	batch, ok, err := w.NextBatch()
	if err != nil || !ok || len(batch.Gaps) != 1 || len(batch.Events) != 0 {
		t.Fatalf("first bounded batch: %+v %v", batch, err)
	}
	if err = w.Ack(context.Background(), batch.Receipt()); err != nil {
		t.Fatal(err)
	}
	batch, ok, err = w.NextBatch()
	if err != nil || !ok || len(batch.Events) != 1 {
		t.Fatalf("event replay: %+v %v", batch, err)
	}
}
