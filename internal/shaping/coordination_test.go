package shaping

import (
	"context"
	"errors"
	"testing"
)

func TestEnsureSameGenerationVerifiesAndRepairsButRejectsDifferentContent(t *testing.T) {
	ctx := context.Background()
	b, s, l := newBackend(), &memoryStore{}, &mutexLock{}
	a := adapter(t, b, s, l)
	p, e := a.Apply(ctx, config())
	if e != nil {
		t.Fatal(e)
	}
	before := len(b.calls)
	if _, e = a.Ensure(ctx, config()); e != nil || len(b.calls) != before {
		t.Fatalf("same generation wasn't verify-only %v", e)
	}
	b.snap.Filters = b.snap.Filters[:1]
	if _, e = a.ApplyVerified(ctx, config()); e != nil || len(b.calls) == before {
		t.Fatalf("same generation did not reassert missing classifier: %v", e)
	}
	cfg := config()
	cfg.Rules[0].BandwidthMbps = 20
	before = len(b.calls)
	if _, e = a.Ensure(ctx, cfg); e == nil || len(b.calls) != before {
		t.Fatal("same generation changed limit")
	}
	if s.state.HighWater != 1 || s.state.Good.Classes[0].Mark != p.Classes[0].Mark {
		t.Fatal("idempotent repair changed history")
	}
}

func TestRestoreVerifiedOldPlanDoesNotLowerHighWaterOrForgetCandidateMarks(t *testing.T) {
	ctx := context.Background()
	b, s, l := newBackend(), &memoryStore{}, &mutexLock{}
	a := adapter(t, b, s, l)
	old, e := a.Apply(ctx, config())
	if e != nil {
		t.Fatal(e)
	}
	cfg := config()
	cfg.Generation = 2
	cfg.Rules[0].BandwidthMbps = 20
	r := cfg.Rules[0]
	r.ID = "candidate"
	r.ListenPort = 20002
	cfg.Rules = append(cfg.Rules, r)
	if _, e = a.Apply(ctx, cfg); e != nil {
		t.Fatal(e)
	}
	restored, e := a.Restore(ctx, old)
	if e != nil {
		t.Fatal(e)
	}
	if s.state.HighWater != 2 || s.state.Pending || len(restored.Classes) != 2 || restored.Config.Generation != 1 {
		t.Fatalf("bad controlled restore %+v", s.state)
	}
	for _, c := range restored.Classes {
		if c.RuleID == "rule-a" && c.CeilMbps != 10 {
			t.Fatal("didn't restore old cap")
		}
		if c.RuleID == "candidate" && c.Active {
			t.Fatal("new mark was not retired")
		}
	}
	a = adapter(t, b, s, l)
	if _, e = a.Ensure(ctx, old.Config); e != nil {
		t.Fatal("restored Good cannot be reasserted", e)
	}
	if _, e = a.Apply(ctx, cfg); !errors.Is(e, ErrStale) {
		t.Fatal("restore lowered generation fence", e)
	}
	forged := old
	forged.Config = config()
	forged.Config.Rules[0].BandwidthMbps = 999
	if _, e = a.Restore(ctx, forged); e == nil {
		t.Fatal("unverified old plan accepted")
	}
}

func TestRestoreFailureRemainsRecoverableToRequestedOldPlan(t *testing.T) {
	ctx := context.Background()
	b, s, l := newBackend(), &memoryStore{}, &mutexLock{}
	a := adapter(t, b, s, l)
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
	b.failAt = len(b.calls) + 2
	if _, e = a.Restore(ctx, old); !errors.Is(e, ErrRecoveryRequired) {
		t.Fatalf("interrupted restore: %v", e)
	}
	a = adapter(t, b, s, l)
	if e = a.Recover(ctx); e != nil {
		t.Fatal(e)
	}
	if s.state.Good.Config.Generation != 1 || s.state.HighWater != 2 || s.state.Good.Classes[0].CeilMbps != 10 {
		t.Fatalf("recovery undid cross-stage rollback %+v", s.state)
	}
}
