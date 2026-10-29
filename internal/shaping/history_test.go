package shaping

import (
	"context"
	"path/filepath"
	"testing"
)

func TestRestoreConfigUsesPersistedPriorGoodAfterRestart(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "private")
	s, e := NewFileStore(dir)
	if e != nil {
		t.Fatal(e)
	}
	b := newBackend()
	a := adapter(t, b, s, s.Locker())
	old := config()
	if _, e = a.Apply(ctx, old); e != nil {
		t.Fatal(e)
	}
	next := config()
	next.Generation = 2
	next.Rules[0].BandwidthMbps = 20
	if _, e = a.Apply(ctx, next); e != nil {
		t.Fatal(e)
	}
	s, e = NewFileStore(dir)
	if e != nil {
		t.Fatal(e)
	}
	a = adapter(t, b, s, s.Locker())
	state, e := s.Load(ctx)
	if e != nil || state.PriorGood == nil || state.PriorGood.Config.Generation != 1 {
		t.Fatal("prior plan not durable", e)
	}
	restored, e := a.RestoreConfig(ctx, old)
	if e != nil {
		t.Fatal(e)
	}
	if restored.Config.Generation != 1 {
		t.Fatal("wrong plan restored")
	}
	before := len(b.calls)
	bad := config()
	bad.Rules[0].BandwidthMbps = 999
	if _, e = a.RestoreConfig(ctx, bad); e == nil || len(b.calls) != before {
		t.Fatal("arbitrary old config restored")
	}
	state, e = s.Load(ctx)
	if e != nil || state.HighWater != 2 {
		t.Fatal("high-water lowered", e)
	}
}

func TestPrepareEmptyOnlyVerifiesAndDoesNotConsumeGeneration(t *testing.T) {
	b, s := newBackend(), &memoryStore{}
	a := adapter(t, b, s, &mutexLock{})
	if _, e := a.Apply(context.Background(), config()); e != nil {
		t.Fatal(e)
	}
	calls, saves := len(b.calls), s.saves
	if e := a.PrepareEmpty(context.Background()); e != nil {
		t.Fatal(e)
	}
	if len(b.calls) != calls || s.saves != saves || s.state.HighWater != 1 {
		t.Fatal("empty-DP preparation mutated tc state")
	}
	b.snap.Filters = nil
	if e := a.PrepareEmpty(context.Background()); e == nil {
		t.Fatal("empty preparation failed to verify actual runtime")
	}
	if len(b.calls) != calls || s.saves != saves {
		t.Fatal("verify-only empty preparation attempted repair")
	}
}
