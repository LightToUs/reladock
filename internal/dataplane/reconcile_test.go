package dataplane

import (
	"context"
	"errors"
	"testing"
)

func TestVerifiedRetryReadsKernelWithoutReapplyingOrResolvingDNS(t *testing.T) {
	ctx := context.Background()
	s := &memoryStore{}
	r := &fakeRunner{}
	a := newTestAdapter(t, s, r, &mutexLock{}, goodVerify)
	cfg := fixture()
	if _, e := a.Apply(ctx, cfg); e != nil {
		t.Fatal(e)
	}
	calls := len(r.calls)
	verified := false
	a.verify = func(context.Context, Plan) error { verified = true; return nil }
	if _, e := a.ApplyVerified(ctx, cfg); e != nil {
		t.Fatal(e)
	}
	if !verified || len(r.calls) != calls+2 {
		t.Fatal("retry must inspect ownership and verify persisted plan")
	}
	cfg.Rules[0].BandwidthMbps = 99
	if _, e := a.ApplyVerified(ctx, cfg); !errors.Is(e, ErrStale) {
		t.Fatalf("same generation changed payload accepted: %v", e)
	}
	cfg = fixture()
	a.verify = func(context.Context, Plan) error { return errors.New("kernel lost") }
	if _, e := a.ApplyVerified(ctx, cfg); e == nil {
		t.Fatal("false applied after kernel state loss")
	}
}
