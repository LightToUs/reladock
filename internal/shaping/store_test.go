package shaping

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestFileStoreAndLockSurviveRestartAndRestoreOldVerifiedPlan(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "private")
	store, e := NewFileStore(dir)
	if e != nil {
		t.Fatal(e)
	}
	b := newBackend()
	a := adapter(t, b, store, store.Locker())
	old, e := a.Apply(ctx, config())
	if e != nil {
		t.Fatal(e)
	}
	cfg := config()
	cfg.Generation = 2
	cfg.Rules[0].BandwidthMbps = 20
	if _, e = a.Apply(ctx, cfg); e != nil {
		t.Fatal(e)
	}
	before, e := store.Load(ctx)
	if e != nil {
		t.Fatal(e)
	}
	store, e = NewFileStore(dir)
	if e != nil {
		t.Fatal(e)
	}
	after, e := store.Load(ctx)
	if e != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("persisted state changed", e)
	}
	info, e := os.Stat(filepath.Join(dir, "state.json"))
	if e != nil || info.Mode().Perm() != 0600 {
		t.Fatal("unsafe state mode", e)
	}
	a = adapter(t, b, store, store.Locker())
	if _, e = a.Restore(ctx, old); e != nil {
		t.Fatal("verified digest history lost after restart", e)
	}
	unlock, e := store.Locker().Lock(ctx)
	if e != nil {
		t.Fatal(e)
	}
	wait, cancel := context.WithTimeout(ctx, 25*time.Millisecond)
	defer cancel()
	if u, e := store.Locker().Lock(wait); !errors.Is(e, context.DeadlineExceeded) {
		if u != nil {
			u()
		}
		t.Fatal("independent lock did not serialize", e)
	}
	unlock()
}

func TestFileStoreRejectsNonprivatePathsSymlinksAndCorruptState(t *testing.T) {
	dir := t.TempDir()
	if e := os.Chmod(dir, 0755); e != nil {
		t.Fatal(e)
	}
	if _, e := NewFileStore(dir); e == nil {
		t.Fatal("nonprivate directory accepted")
	}
	private := filepath.Join(dir, "private")
	s, e := NewFileStore(private)
	if e != nil {
		t.Fatal(e)
	}
	link := filepath.Join(dir, "link")
	if e = os.Symlink(private, link); e != nil {
		t.Fatal(e)
	}
	if _, e = NewFileStore(link); e == nil {
		t.Fatal("symlink accepted")
	}
	if e = os.WriteFile(filepath.Join(private, "state.json"), []byte(`{"HighWater":1,"unknown":true}`), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Load(context.Background()); e == nil {
		t.Fatal("unknown state fields accepted")
	}
}
