package shaping

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
)

type memoryStore struct {
	state     State
	saves     int
	failAfter int
}

func (s *memoryStore) Load(context.Context) (State, error) { return s.state, nil }
func (s *memoryStore) Save(_ context.Context, v State) error {
	s.saves++
	if s.failAfter > 0 && s.saves > s.failAfter {
		return errors.New("persistent disk failure")
	}
	s.state = v
	return nil
}

type mutexLock struct{ sync.Mutex }

func (l *mutexLock) Lock(context.Context) (func(), error) { l.Mutex.Lock(); return l.Mutex.Unlock, nil }

type fakeBackend struct {
	snap             Snapshot
	calls            [][]string
	failAt           int
	foreignOnFailure bool
}

func newBackend() *fakeBackend {
	l := options().Lease
	return &fakeBackend{snap: Snapshot{NamespaceID: l.NamespaceID, Device: l.Device, IfIndex: l.IfIndex, Alias: l.Alias(), RootKind: "noqueue", RootHandle: "0:"}}
}
func (b *fakeBackend) Inspect(context.Context, Lease) (Snapshot, error) { return b.snap, nil }
func value(args []string, key string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == key {
			return args[i+1]
		}
	}
	return ""
}
func bits(s string) uint64 {
	if strings.HasSuffix(s, "mbit") {
		n, _ := strconv.ParseUint(strings.TrimSuffix(s, "mbit"), 10, 64)
		return n * 1000000
	}
	n, _ := strconv.ParseUint(strings.TrimSuffix(s, "bit"), 10, 64)
	return n
}
func (b *fakeBackend) RunTC(_ context.Context, args []string) error {
	b.calls = append(b.calls, append([]string(nil), args...))
	if len(b.calls) == b.failAt {
		if b.foreignOnFailure {
			b.snap.RootKind = "fq_codel"
		}
		return errors.New("tc runtime failure")
	}
	switch args[0] {
	case "qdisc":
		if b.snap.RootKind != "noqueue" && b.snap.RootKind != "" {
			return errors.New("root exists")
		}
		b.snap.RootKind = "htb"
		b.snap.RootHandle = RootHandle
		b.snap.RootDefault = 65535
	case "class":
		c := ClassState{ID: value(args, "classid"), Parent: value(args, "parent"), Kind: "htb", RateBits: bits(value(args, "rate")), CeilBits: bits(value(args, "ceil"))}
		found := false
		for i, old := range b.snap.Classes {
			if old.ID == c.ID {
				b.snap.Classes[i] = c
				found = true
			}
		}
		if !found {
			b.snap.Classes = append(b.snap.Classes, c)
		}
	case "filter":
		f := FilterState{Kind: "fw", Parent: RootHandle, Protocol: "ip", Priority: 10, ClassID: value(args, "classid")}
		if value(args, "prio") == "20" {
			f.Priority = 20
			f.Drop = true
		}
		h := strings.Split(value(args, "handle"), "/")
		n, _ := strconv.ParseUint(h[0], 0, 32)
		f.Mark = uint32(n)
		n, _ = strconv.ParseUint(h[1], 0, 32)
		f.Mask = uint32(n)
		found := false
		for i, old := range b.snap.Filters {
			if old.Mark == f.Mark && old.Mask == f.Mask {
				b.snap.Filters[i] = f
				found = true
			}
		}
		if !found {
			b.snap.Filters = append(b.snap.Filters, f)
		}
	}
	return nil
}
func adapter(t *testing.T, b Backend, s Store, l Locker) *Adapter {
	t.Helper()
	a, e := NewAdapter(options(), b, s, l)
	if e != nil {
		t.Fatal(e)
	}
	return a
}

func TestAdapterAppliesAndRejectsStaleAcrossRestart(t *testing.T) {
	ctx := context.Background()
	b, s, l := newBackend(), &memoryStore{}, &mutexLock{}
	a := adapter(t, b, s, l)
	p, e := a.Apply(ctx, config())
	if e != nil {
		t.Fatal(e)
	}
	if s.state.Pending || s.state.Good == nil || s.state.HighWater != 1 || len(b.snap.Classes) != 3 || len(b.snap.Filters) != 2 {
		t.Fatalf("state %+v snapshot %+v", s.state, b.snap)
	}
	if !reflect.DeepEqual(b.calls[0], []string{"qdisc", "add", "dev", "rd-egress", "root", "handle", RootHandle, "htb", "default", "ffff"}) {
		t.Fatal("root must be add, not replace", b.calls[0])
	}
	count := len(b.calls)
	a = adapter(t, b, s, l)
	if _, e = a.Apply(ctx, config()); !errors.Is(e, ErrStale) {
		t.Fatalf("stale: %v", e)
	}
	if len(b.calls) != count {
		t.Fatal("stale modified runtime")
	}
	if p.Classes[0].CeilMbps != 10 {
		t.Fatal(p)
	}
}

func TestAdapterRefusesForeignRootAndLeaseDriftBeforeMutating(t *testing.T) {
	for _, kind := range []string{"fq_codel", "htb", "cake"} {
		t.Run(kind, func(t *testing.T) {
			b := newBackend()
			b.snap.RootKind = kind
			b.snap.RootHandle = RootHandle
			b.snap.RootDefault = 65535
			s := &memoryStore{}
			if _, e := adapter(t, b, s, &mutexLock{}).Apply(context.Background(), config()); e == nil || len(b.calls) != 0 || s.saves != 0 {
				t.Fatal("adopted external root", e, b.calls)
			}
		})
	}
	for _, field := range []string{"namespace", "ifindex", "alias"} {
		t.Run(field, func(t *testing.T) {
			b := newBackend()
			switch field {
			case "namespace":
				b.snap.NamespaceID = "other"
			case "ifindex":
				b.snap.IfIndex++
			case "alias":
				b.snap.Alias = "external"
			}
			if _, e := adapter(t, b, &memoryStore{}, &mutexLock{}).Apply(context.Background(), config()); e == nil || len(b.calls) != 0 {
				t.Fatal("lease drift accepted")
			}
		})
	}
}

func TestPartialFailureRestoresStricterCeilingAndRetainsCandidateMarks(t *testing.T) {
	ctx := context.Background()
	b, s, l := newBackend(), &memoryStore{}, &mutexLock{}
	a := adapter(t, b, s, l)
	if _, e := a.Apply(ctx, config()); e != nil {
		t.Fatal(e)
	}
	cfg := config()
	cfg.Generation = 2
	cfg.Rules[0].BandwidthMbps = 2
	r := cfg.Rules[0]
	r.ID = "new-rule"
	r.ListenPort = 20002
	r.BandwidthMbps = 7
	cfg.Rules = append(cfg.Rules, r)
	b.failAt = len(b.calls) + 5
	if _, e := a.Apply(ctx, cfg); e == nil {
		t.Fatal("runtime failure lost")
	}
	if s.state.Pending || s.state.HighWater != 2 || s.state.Good == nil || len(s.state.Good.Classes) != 2 {
		t.Fatalf("candidate reservation lost %+v", s.state)
	}
	for _, c := range s.state.Good.Classes {
		if c.RuleID == "rule-a" && c.CeilMbps != 2 {
			t.Fatal("rollback raised stricter cap", c)
		}
		if c.RuleID == "new-rule" && (c.CeilMbps != 7 || c.Active) {
			t.Fatal("candidate classifier not retained safely", c)
		}
	}
	a = adapter(t, b, s, l)
	cfg = config()
	cfg.Generation = 3
	p, e := a.Apply(ctx, cfg)
	if e != nil || len(p.Classes) != 2 {
		t.Fatalf("lost reserved mark on restart %+v %v", p, e)
	}
}

func TestPersistentDiskFailureDoesNotSuppressKernelRecovery(t *testing.T) {
	b, s := newBackend(), &memoryStore{failAfter: 1}
	a := adapter(t, b, s, &mutexLock{})
	if _, e := a.Apply(context.Background(), config()); !errors.Is(e, ErrRecoveryRequired) {
		t.Fatalf("must require recovery %v", e)
	}
	if !s.state.Pending || len(b.calls) < 2*len(s.state.Candidate.Commands) {
		t.Fatal("disk failure skipped runtime recovery", len(b.calls), s.state)
	}
}

func TestRollbackDoesNotOverwriteExternalRootThatAppearsMidApply(t *testing.T) {
	b, s := newBackend(), &memoryStore{}
	b.failAt = 3
	b.foreignOnFailure = true
	if _, e := adapter(t, b, s, &mutexLock{}).Apply(context.Background(), config()); !errors.Is(e, ErrRecoveryRequired) {
		t.Fatalf("foreign root should block recovery %v", e)
	}
	if !s.state.Pending || len(b.calls) != 3 || b.snap.RootKind != "fq_codel" {
		t.Fatal("foreign qdisc overwritten")
	}
}

func TestConcurrentAdaptersSerialize(t *testing.T) {
	b, s, l := newBackend(), &memoryStore{}, &mutexLock{}
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, e := adapter(t, b, s, l).Apply(context.Background(), config()); errs <- e }()
	}
	wg.Wait()
	close(errs)
	ok, stale := 0, 0
	for e := range errs {
		if e == nil {
			ok++
		} else if errors.Is(e, ErrStale) {
			stale++
		} else {
			t.Fatal(e)
		}
	}
	if ok != 1 || stale != 1 {
		t.Fatal(ok, stale)
	}
}
