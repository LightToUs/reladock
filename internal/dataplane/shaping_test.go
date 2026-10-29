package dataplane

import (
	"context"
	"reladock/internal/shaping"
	"strings"
	"testing"
)

func TestShapingMarksBothDirectionsOfOneRuleIntoSameClass(t *testing.T) {
	o := options()
	o.Shaping = &shaping.Options{NodeID: "hk-01", Lease: shaping.Lease{Device: "eth0", IfIndex: 2, NamespaceID: "net:[1]", OwnerID: "hk-01", Dedicated: true, MarkNamespace: 82, LinkMbps: 1000}, UploadEgress: "eth0", DownloadEgress: "eth0"}
	cfg := fixture()
	p, e := Compile(context.Background(), cfg, o)
	if e != nil {
		t.Fatal(e)
	}
	// All four TCP/UDP direction counters share one exact tc mark. Without
	// interface matching traffic could bypass the single owned shaping egress.
	if strings.Count(p.Script, "meta mark set (meta mark & 0x000000ff) | 0x") != 4 || strings.Count(p.Script, `oifname "eth0"`) != 4 {
		t.Fatal("unshaped forwarding path", p.Script)
	}
	s := &memoryStore{}
	r := &fakeRunner{}
	a, e := NewAdapter(o, r, s, &mutexLock{}, goodVerify)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = a.Apply(context.Background(), cfg); e != nil {
		t.Fatal(e)
	}
	if _, e = a.ApplyVerified(context.Background(), cfg); e != nil {
		t.Fatal("persisted marked plan failed validation", e)
	}
}
