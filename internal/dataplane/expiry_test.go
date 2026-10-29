package dataplane

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestPaidRuleExpiryRemainsInKernelEvenIfAgentStops(t *testing.T) {
	cfg := fixture()
	now := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	expires := now.Add(60 * time.Second)
	cfg.Rules[0].ExpiresAt = &expires
	o := options()
	o.builtAt = now
	p, e := Compile(context.Background(), cfg, o)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(p.Script, "timeout 25000ms") || strings.Count(p.Script, "ct original proto-dst @e_") != 4 {
		t.Fatal("forwarding ignores kernel entitlement expiry", p.Script)
	}
	o.builtAt = expires.Add(time.Second)
	expired, e := Compile(context.Background(), cfg, o)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(expired.Script, "elements =") {
		t.Fatal("expired rollback renewed entitlement", expired.Script)
	}
}
