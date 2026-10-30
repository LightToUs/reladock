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

func TestLongPaidRuleExpiryUsesNftSafeSeconds(t *testing.T) {
	cfg := fixture()
	now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	expires := now.Add(365 * 24 * time.Hour)
	cfg.Rules[0].ExpiresAt = &expires
	o := options()
	o.builtAt = now
	p, err := Compile(context.Background(), cfg, o)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Script, "timeout 31535965s") {
		t.Fatal("long expiry was emitted as an nft-unsafe millisecond value", p.Script)
	}
	if strings.Contains(p.Script, "timeout 31535965000ms") {
		t.Fatal("long expiry still exceeds nft's millisecond parser range", p.Script)
	}
}
