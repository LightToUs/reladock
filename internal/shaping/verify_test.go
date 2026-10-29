package shaping

import (
	"context"
	"testing"
)

func TestVerifyOnlyReadsGoodAndCompleteRuntime(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"valid", "missing-root", "missing-filter", "changed-ceil", "changed-config", "pending"} {
		t.Run(mode, func(t *testing.T) {
			b, s := newBackend(), &memoryStore{}
			a := adapter(t, b, s, &mutexLock{})
			cfg := config()
			if _, e := a.Apply(ctx, cfg); e != nil {
				t.Fatal(e)
			}
			calls, saves := len(b.calls), s.saves
			switch mode {
			case "missing-root":
				b.snap.RootKind = "noqueue"
				b.snap.RootHandle = "0:"
				b.snap.Classes = nil
				b.snap.Filters = nil
			case "missing-filter":
				b.snap.Filters = nil
			case "changed-ceil":
				b.snap.Classes[len(b.snap.Classes)-1].CeilBits++
			case "changed-config":
				cfg.Rules[0].BandwidthMbps++
			case "pending":
				s.state.Pending = true
				s.state.Candidate = s.state.Good
			}
			e := a.Verify(ctx, cfg)
			if (e == nil) != (mode == "valid") {
				t.Fatalf("Verify(%s): %v", mode, e)
			}
			if len(b.calls) != calls || s.saves != saves {
				t.Fatalf("read-only verification modified runtime/state: calls %d->%d saves %d->%d", calls, len(b.calls), saves, s.saves)
			}
		})
	}
}

func TestVerifyWithoutGoodNeverInstallsRoot(t *testing.T) {
	b, s := newBackend(), &memoryStore{}
	if e := adapter(t, b, s, &mutexLock{}).Verify(context.Background(), config()); e == nil {
		t.Fatal("missing Good verified")
	}
	if len(b.calls) != 0 || s.saves != 0 {
		t.Fatal("Verify constructed a tree without Good")
	}
}
