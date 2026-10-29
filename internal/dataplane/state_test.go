package dataplane

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestFileStateRestartAndCorruption(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	s, e := NewFileStore(dir)
	if e != nil {
		t.Fatal(e)
	}
	p, e := Compile(context.Background(), fixture(), options())
	if e != nil {
		t.Fatal(e)
	}
	want := State{NodeID: "hk-01", HighWater: 2, Pending: true, Good: &p}
	if e = s.Save(context.Background(), want); e != nil {
		t.Fatal(e)
	}
	s, e = NewFileStore(dir)
	if e != nil {
		t.Fatal(e)
	}
	got, e := s.Load(context.Background())
	if e != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("restart: %+v %v", got, e)
	}
	info, e := os.Stat(filepath.Join(dir, "state.json"))
	if e != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("state permissions %v %v", info, e)
	}
	// Corruption is an external failure fixture, not a production state writer.
	if e = os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"HighWater":99,"unknown":true}`), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Load(context.Background()); e == nil {
		t.Fatal("unknown fields accepted")
	}
}

func TestFileStateRejectsSymlinksAndNonprivateDirectory(t *testing.T) {
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
		t.Fatal("symlink directory accepted")
	}
	if e = os.Symlink(filepath.Join(dir, "missing"), filepath.Join(private, "state.json")); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Load(context.Background()); e == nil {
		t.Fatal("symlink state accepted")
	}
}

func TestFileLockSerializesIndependentInstancesAndHonorsCancellation(t *testing.T) {
	s, e := NewFileStore(filepath.Join(t.TempDir(), "private"))
	if e != nil {
		t.Fatal(e)
	}
	l1, l2 := s.Locker(), s.Locker()
	unlock, e := l1.Lock(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if release, e := l2.Lock(ctx); !errors.Is(e, context.DeadlineExceeded) {
		if release != nil {
			release()
		}
		t.Fatalf("lock did not block: %v", e)
	}
	unlock()
	release, e := l2.Lock(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	release()
}

func TestFileStateRecoverySurvivesAdapterRestart(t *testing.T) {
	s, e := NewFileStore(filepath.Join(t.TempDir(), "private"))
	if e != nil {
		t.Fatal(e)
	}
	r := &fakeRunner{}
	a := newTestAdapter(t, s, r, s.Locker(), goodVerify)
	if _, e = a.Apply(context.Background(), fixture()); e != nil {
		t.Fatal(e)
	}
	state, e := s.Load(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	state.HighWater = 2
	state.Pending = true
	candidateConfig := fixture()
	candidateConfig.Generation = 2
	candidateConfig.Rules[0].ListenPort = 20002
	candidate, e := Compile(context.Background(), candidateConfig, options())
	if e != nil {
		t.Fatal(e)
	}
	state.Candidate = &candidate
	if e = s.Save(context.Background(), state); e != nil {
		t.Fatal(e)
	}
	a = newTestAdapter(t, s, r, s.Locker(), goodVerify)
	c := fixture()
	c.Generation = 3
	if _, e = a.Apply(context.Background(), c); !errors.Is(e, ErrRecoveryRequired) {
		t.Fatal(e)
	}
	if e = a.Recover(context.Background()); e != nil {
		t.Fatal(e)
	}
	if _, e = a.Apply(context.Background(), c); e != nil {
		t.Fatal(e)
	}
}
