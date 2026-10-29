package dataplane

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type transitionLog struct {
	events []string
	runner *fakeRunner
	fail   bool
}

func (h *transitionLog) BeforeReplace(_ context.Context, p Plan) error {
	if len(h.runner.calls) == 0 || !strings.Contains(h.runner.calls[len(h.runner.calls)-1].script, "reladock-meter-freeze") {
		return errors.New("counters sampled before traffic freeze")
	}
	h.events = append(h.events, "before")
	if h.fail {
		return errors.New("WAL full")
	}
	return nil
}
func (h *transitionLog) AfterReplace(context.Context, Plan) error {
	h.events = append(h.events, "after")
	return nil
}
func TestTransitionHooksFreezeBeforeReadingCounters(t *testing.T) {
	ctx := context.Background()
	r := &fakeRunner{}
	s := &memoryStore{}
	a := newTestAdapter(t, s, r, &mutexLock{}, goodVerify)
	h := &transitionLog{runner: r}
	a.SetTransitionHooks(h)
	if _, e := a.Apply(ctx, fixture()); e != nil {
		t.Fatal(e)
	}
	cfg := fixture()
	cfg.Generation = 2
	if _, e := a.Apply(ctx, cfg); e != nil {
		t.Fatal(e)
	}
	if strings.Join(h.events, ",") != "after,before,after" {
		t.Fatalf("hooks %+v", h.events)
	}
}

func TestTransitionMeterFailureCannotResumeOrDiscardUnrecordedTraffic(t *testing.T) {
	ctx := context.Background()
	r := &fakeRunner{}
	s := &memoryStore{}
	a := newTestAdapter(t, s, r, &mutexLock{}, goodVerify)
	h := &transitionLog{runner: r}
	a.SetTransitionHooks(h)
	if _, e := a.Apply(ctx, fixture()); e != nil {
		t.Fatal(e)
	}
	h.fail = true
	cfg := fixture()
	cfg.Generation = 2
	if _, e := a.Apply(ctx, cfg); e == nil {
		t.Fatal("lost WAL error")
	}
	if !s.state.Pending || !strings.Contains(r.calls[len(r.calls)-1].script, "reladock-meter-freeze") {
		t.Fatal("forwarded after losing final counter snapshot")
	}
	if e := a.Recover(ctx); e == nil {
		t.Fatal("recovery discarded unsaved counters")
	}
}

func TestDependencyVerificationFailureFreezesOwnedForwardingBeforeReturning(t *testing.T) {
	ctx := context.Background()
	r := &fakeRunner{}
	s := &memoryStore{}
	a := newTestAdapter(t, s, r, &mutexLock{}, goodVerify)
	if _, e := a.Apply(ctx, fixture()); e != nil {
		t.Fatal(e)
	}
	a.verify = func(context.Context, Plan) error { return errors.New("tc root disappeared") }
	called := false
	if e := a.Observe(ctx, func(context.Context, Plan) error { called = true; return nil }); e == nil {
		t.Fatal("lost dependency failure")
	}
	if called || !strings.Contains(r.calls[len(r.calls)-1].script, "reladock-meter-freeze") {
		t.Fatal("forwarding continued with unverified limiter")
	}
}

func TestRollbackCommitFailurePersistsPhysicalRecoveryIncarnationAcrossRestart(t *testing.T) {
	ctx := context.Background()
	r := &fakeRunner{}
	s := &memoryStore{}
	a := newTestAdapter(t, s, r, &mutexLock{}, goodVerify)
	h := &transitionLog{runner: r}
	a.SetTransitionHooks(h)
	if _, e := a.Apply(ctx, fixture()); e != nil {
		t.Fatal(e)
	}
	a.verify = func(_ context.Context, p Plan) error {
		if p.Generation == 2 {
			return errors.New("candidate failed")
		}
		return nil
	}
	s.failSave = 5
	cfg := fixture()
	cfg.Generation = 2
	if _, e := a.Apply(ctx, cfg); e == nil {
		t.Fatal("lost rollback commit failure")
	}
	if s.state.Recovery == nil || !s.state.Pending || r.token != planToken(*s.state.Recovery) {
		t.Fatal("physical recovery incarnation not journaled before execution")
	}
	s.failSave = 0
	a = newTestAdapter(t, s, r, &mutexLock{}, goodVerify)
	a.SetTransitionHooks(h)
	if e := a.Recover(ctx); e != nil {
		t.Fatal("restart could not identify recovery counters", e)
	}
	if s.state.Pending || s.state.Recovery != nil || s.state.Good.Generation != 1 {
		t.Fatal("recovery didn't restore old business config")
	}
}

type identifiedHooks struct {
	*transitionLog
	id string
}

func (h *identifiedHooks) MeterIdentity() (string, error) { return h.id, nil }
func TestDifferentWALCannotAdoptAndRebillExistingPhysicalCounters(t *testing.T) {
	ctx := context.Background()
	r := &fakeRunner{}
	s := &memoryStore{}
	a := newTestAdapter(t, s, r, &mutexLock{}, goodVerify)
	h := &identifiedHooks{transitionLog: &transitionLog{runner: r}, id: "wal-first"}
	a.SetTransitionHooks(h)
	if _, e := a.Apply(ctx, fixture()); e != nil {
		t.Fatal(e)
	}
	a = newTestAdapter(t, s, r, &mutexLock{}, goodVerify)
	h.id = "wal-empty-replacement"
	a.SetTransitionHooks(h)
	sampled := false
	if e := a.Observe(ctx, func(context.Context, Plan) error { sampled = true; return nil }); e == nil {
		t.Fatal("new WAL adopted old counters")
	}
	if sampled || !strings.Contains(r.calls[len(r.calls)-1].script, "reladock-meter-freeze") {
		t.Fatal("WAL mismatch neither stopped traffic nor prevented rebilling")
	}
}

type interruptedRecoveryHooks struct {
	*transitionLog
	failPrepare bool
}

func (h *interruptedRecoveryHooks) PrepareReplace(context.Context, Plan) error {
	if h.failPrepare {
		return errors.New("interrupted before kernel replacement")
	}
	return nil
}
func TestRepeatedRecoveryInterruptionRetainsTheActualPriorRecovery(t *testing.T) {
	ctx := context.Background()
	r := &fakeRunner{}
	s := &memoryStore{}
	h := &interruptedRecoveryHooks{transitionLog: &transitionLog{runner: r}}
	a := newTestAdapter(t, s, r, &mutexLock{}, goodVerify)
	a.SetTransitionHooks(h)
	if _, e := a.Apply(ctx, fixture()); e != nil {
		t.Fatal(e)
	}
	a.verify = func(_ context.Context, p Plan) error {
		if p.Generation == 2 {
			return errors.New("candidate failed")
		}
		return nil
	}
	s.failSave = 5
	cfg := fixture()
	cfg.Generation = 2
	if _, e := a.Apply(ctx, cfg); e == nil {
		t.Fatal("lost first commit failure")
	}
	actual := r.token
	s.failSave = 0
	h.failPrepare = true
	a = newTestAdapter(t, s, r, &mutexLock{}, goodVerify)
	a.SetTransitionHooks(h)
	if e := a.Recover(ctx); e == nil {
		t.Fatal("lost second interruption")
	}
	if actual != r.token {
		t.Fatal("test unexpectedly replaced old recovery")
	}
	h.failPrepare = false
	a = newTestAdapter(t, s, r, &mutexLock{}, goodVerify)
	a.SetTransitionHooks(h)
	if e := a.Recover(ctx); e != nil {
		t.Fatal("third recovery forgot actual physical plan", e)
	}
}
