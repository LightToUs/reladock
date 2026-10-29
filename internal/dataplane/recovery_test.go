package dataplane

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func lastApplied(t *testing.T, r *fakeRunner) string {
	t.Helper()
	for i := len(r.calls) - 1; i >= 0; i-- {
		if reflect.DeepEqual(r.calls[i].args, []string{"-f", "-"}) {
			return r.calls[i].script
		}
	}
	t.Fatal("no kernel transaction attempted")
	return ""
}

func TestRollbackRetainsNewCandidateAllocationAcrossRestart(t *testing.T) {
	for _, initial := range []bool{false, true} {
		t.Run(map[bool]string{false: "existing-good", true: "initial-apply"}[initial], func(t *testing.T) {
			ctx := context.Background()
			store, e := NewFileStore(filepath.Join(t.TempDir(), "state"))
			if e != nil {
				t.Fatal(e)
			}
			r := &fakeRunner{}
			a := newTestAdapter(t, store, r, store.Locker(), goodVerify)
			cfg := fixture()
			if !initial {
				if _, e = a.Apply(ctx, cfg); e != nil {
					t.Fatal(e)
				}
				cfg.Generation = 2
			}
			cfg.Rules[0].ListenPort = 20002
			failedGeneration := cfg.Generation
			a.verify = func(_ context.Context, p Plan) error {
				if p.Generation == failedGeneration && len(p.Counters) > 0 && p.Config.Rules[0].ListenPort == 20002 {
					return errors.New("candidate runtime failed")
				}
				return nil
			}
			if _, e = a.Apply(ctx, cfg); e == nil {
				t.Fatal("candidate unexpectedly succeeded")
			}
			restored := lastApplied(t, r)
			if !strings.Contains(restored, "ct original proto-dst 20002 jump relay") || strings.Contains(restored, "dport 20002 dnat") || strings.Contains(restored, "20000-60000") {
				t.Fatalf("candidate allocation not fenced on rollback:\n%s", restored)
			}
			a = newTestAdapter(t, store, r, store.Locker(), goodVerify)
			next := fixture()
			next.Generation = failedGeneration + 1
			p, e := a.Apply(ctx, next)
			if e != nil {
				t.Fatal(e)
			}
			if !strings.Contains(p.Script, "ct original proto-dst 20002 jump relay") || strings.Contains(p.Script, "dport 20002 dnat") {
				t.Fatal("restart lost failed-candidate tombstone", p.Script)
			}
			next.Generation++
			next.Rules[0].ListenPort = 20002
			if _, e = a.Apply(ctx, next); e == nil {
				t.Fatal("failed allocation reused without conntrack reconciliation")
			}
		})
	}
}

type permanentFailureStore struct {
	memoryStore
	successes int
	fail      bool
}

func (s *permanentFailureStore) Save(c context.Context, v State) error {
	if s.fail {
		return errors.New("disk persistently unavailable")
	}
	if e := s.memoryStore.Save(c, v); e != nil {
		return e
	}
	s.successes++
	if v.Pending {
		s.fail = true
	}
	return nil
}

func TestPersistentDiskFailureStillAttemptsKernelRecovery(t *testing.T) {
	for _, failure := range []string{"runtime-verification", "state-commit"} {
		t.Run(failure, func(t *testing.T) {
			s := &permanentFailureStore{}
			r := &fakeRunner{}
			verify := goodVerify
			if failure == "runtime-verification" {
				verify = func(_ context.Context, p Plan) error {
					if len(p.Counters) > 0 {
						return errors.New("runtime failed")
					}
					return nil
				}
			}
			a := newTestAdapter(t, s, r, &mutexLock{}, verify)
			if _, e := a.Apply(context.Background(), fixture()); !errors.Is(e, ErrRecoveryRequired) {
				t.Fatalf("must report uncertain durable recovery: %v", e)
			}
			count := 0
			for _, c := range r.calls {
				if reflect.DeepEqual(c.args, []string{"-f", "-"}) {
					count++
				}
			}
			if count != 2 {
				t.Fatalf("disk error suppressed runtime rollback: %d apply attempts", count)
			}
			restored := lastApplied(t, r)
			if strings.Contains(restored, "dnat to") || !strings.Contains(restored, "ct original proto-dst 20000 jump relay") {
				t.Fatal("kernel was not restored to exact fail-closed guards", restored)
			}
			if !s.state.Pending {
				t.Fatal("durable pending flag cleared despite disk failure")
			}
			a = newTestAdapter(t, s, r, &mutexLock{}, goodVerify)
			next := fixture()
			next.Generation = 2
			if _, e := a.Apply(context.Background(), next); !errors.Is(e, ErrRecoveryRequired) {
				t.Fatalf("restart not blocked: %v", e)
			}
			if e := a.Recover(context.Background()); !errors.Is(e, ErrRecoveryRequired) {
				t.Fatalf("failed explicit recovery did not report pending durability: %v", e)
			}
		})
	}
}

func TestDurableCandidateSurvivesInterruptedApplyAndRecovery(t *testing.T) {
	ctx := context.Background()
	store, e := NewFileStore(filepath.Join(t.TempDir(), "state"))
	if e != nil {
		t.Fatal(e)
	}
	r := &fakeRunner{}
	a := newTestAdapter(t, store, r, store.Locker(), goodVerify)
	if _, e = a.Apply(ctx, fixture()); e != nil {
		t.Fatal(e)
	}
	cfg := fixture()
	cfg.Generation = 2
	cfg.Rules[0].ListenPort = 20004
	// The injected runner models process interruption after durable intent but
	// before apply/verification returns; neither adapter failure path runs.
	stop := errors.New("simulated interruption")
	wrapped := runnerFunc(func(c context.Context, args []string, in string) ([]byte, error) {
		if reflect.DeepEqual(args, []string{"-f", "-"}) {
			state, e := store.Load(ctx)
			if e != nil {
				t.Fatal(e)
			}
			if state.Candidate == nil || state.Candidate.Config.Rules[0].ListenPort != 20004 || !state.Pending {
				t.Fatal("candidate not durably saved before apply")
			}
			panic(stop)
		}
		return r.Run(c, args, in)
	})
	a = newTestAdapter(t, store, wrapped, store.Locker(), goodVerify)
	func() {
		defer func() {
			if got := recover(); got != stop {
				t.Fatalf("unexpected interruption %v", got)
			}
		}()
		_, _ = a.Apply(ctx, cfg)
	}()
	a = newTestAdapter(t, store, r, store.Locker(), goodVerify)
	if e = a.Recover(ctx); e != nil {
		t.Fatal(e)
	}
	state, e := store.Load(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if state.Pending || state.Candidate != nil || state.Good == nil || len(state.Tombstones) != 2 || !strings.Contains(state.Good.Script, "ct original proto-dst 20004 jump relay") {
		t.Fatalf("restart recovery discarded candidate allocations %+v", state)
	}
	cfg.Generation = 3
	if _, e = a.Apply(ctx, cfg); e == nil {
		t.Fatal("recovery tombstones reused without explicit reconciliation")
	}
}
