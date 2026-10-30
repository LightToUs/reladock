package shaping

import (
	"context"
	"fmt"
	"reflect"
	"testing"
)

type runnerFunc func(context.Context, Tool, []string) ([]byte, error)

func (f runnerFunc) Run(c context.Context, t Tool, a []string) ([]byte, error) { return f(c, t, a) }

func TestTCWritePolicyAllowsOnlyExactDefaultRootHandoff(t *testing.T) {
	for _, allowed := range [][]string{
		{"qdisc", "replace", "dev", "rd-egress", "root", "handle", RootHandle, "htb", "default", "ffff"},
		{"class", "replace", "dev", "rd-egress", "parent", RootHandle, "classid", ParentClass, "htb", "rate", "1000mbit", "ceil", "1000mbit", "burst", "1250000", "cburst", "1250000"},
		{"class", "replace", "dev", "rd-egress", "parent", ParentClass, "classid", "5244:abcd", "htb", "rate", "8bit", "ceil", "10mbit", "quantum", "1514", "burst", "12500", "cburst", "12500"},
		{"filter", "replace", "dev", "rd-egress", "parent", RootHandle, "protocol", "ip", "prio", "10", "handle", "0x52012300/0xffffff00", "fw", "classid", "5244:abcd"},
		{"filter", "replace", "dev", "rd-egress", "parent", RootHandle, "protocol", "ip", "prio", "20", "handle", "0x52000000/0xff000000", "fw", "action", "drop"},
	} {
		if !tcWriteAllowed(allowed, "rd-egress") {
			t.Fatal("compiler command rejected", allowed)
		}
	}
	for _, bad := range [][]string{
		{"qdisc", "replace", "dev", "rd-egress", "root", "fq"},
		{"qdisc", "replace", "dev", "rd-egress", "root", "handle", "1:", "htb", "default", "ffff"},
		{"qdisc", "replace", "dev", "bad;device", "root", "handle", RootHandle, "htb", "default", "ffff"},
		{"qdisc", "delete", "dev", "rd-egress", "root"},
		{"class", "replace", "dev", "other0", "parent", RootHandle, "classid", ParentClass, "htb", "rate", "1000mbit", "ceil", "1000mbit", "burst", "1250000", "cburst", "1250000"},
		{"class", "replace", "dev", "rd-egress", "parent", RootHandle, "classid", ParentClass, "cake", "rate", "1000mbit", "ceil", "1000mbit", "burst", "1250000", "cburst", "1250000"},
		{"filter", "replace", "dev", "rd-egress", "parent", RootHandle, "protocol", "ip", "prio", "10", "handle", "0x52012300/0xffffff00", "fw", "action", "pass"},
	} {
		if tcWriteAllowed(bad, "rd-egress") {
			t.Fatal("unsafe root mutation allowed", bad)
		}
	}
}

func TestSystemBackendRejectsRootReplaceWithoutAuthorizedLease(t *testing.T) {
	writes := 0
	run := runnerFunc(func(_ context.Context, _ Tool, _ []string) ([]byte, error) {
		writes++
		return []byte(`[]`), nil
	})
	b := NewSystemBackend(run, func(context.Context) (string, error) { return "netns-test", nil })
	cmd := []string{"qdisc", "replace", "dev", "rd-egress", "root", "handle", RootHandle, "htb", "default", "ffff"}
	if e := b.RunTC(context.Background(), options().Lease, cmd); e == nil || writes != 0 {
		t.Fatalf("unauthorized backend root replacement reached tc: err=%v writes=%d", e, writes)
	}
}

func TestSystemBackendParsesOwnershipAndMaskedFWJSON(t *testing.T) {
	p, e := Compile(config(), options())
	if e != nil {
		t.Fatal(e)
	}
	c := p.Classes[0]
	run := runnerFunc(func(_ context.Context, tool Tool, args []string) ([]byte, error) {
		switch {
		case tool == IP:
			return []byte(`[{"ifindex":7,"ifname":"rd-egress","ifalias":"reladock-shaping-v1:node-owner"}]`), nil
		case reflect.DeepEqual(args, []string{"-j", "qdisc", "show", "dev", "rd-egress"}):
			return []byte(`[{"kind":"htb","handle":"5244:","root":true,"options":{"default":"0xffff"}},{"kind":"pfifo","handle":"0:","parent":"5244:abcd"}]`), nil
		case args[1] == "class":
			return []byte(fmt.Sprintf(`[{"kind":"htb","handle":"%s","parent":"5244:1","options":{"rate":1,"ceil":1250000}}]`, c.ClassID)), nil
		case args[1] == "filter":
			return []byte(fmt.Sprintf(`[{"protocol":"ip","pref":10,"kind":"fw","parent":"5244:","chain":0},{"protocol":"ip","pref":10,"kind":"fw","parent":"5244:","chain":0,"options":{"fw":{"mark":"0x%08x","mask":"0xffffff00"},"classid":"%s"}},{"protocol":"ip","pref":20,"kind":"fw","parent":"5244:","chain":0,"options":{"fw":{"mark":"0x52000000","mask":"0xff000000"},"actions":[{"kind":"gact","control_action":{"type":"drop"}}]}}]`, c.Mark, c.ClassID)), nil
		}
		t.Fatal(tool, args)
		return nil, nil
	})
	b := NewSystemBackend(run, func(context.Context) (string, error) { return "netns-test", nil })
	s, e := b.Inspect(context.Background(), options().Lease)
	if e != nil {
		t.Fatal(e)
	}
	if s.NamespaceID != "netns-test" || s.Alias != options().Lease.Alias() || s.RootDefault != 65535 || len(s.Classes) != 1 || s.Classes[0].CeilBits != 10000000 || len(s.Filters) != 2 || s.Filters[0].Mark != c.Mark || !s.Filters[1].Drop {
		t.Fatalf("incorrect tc JSON interpretation %+v", s)
	}
}

func TestSystemBackendFingerprintsExactKernelDefaultQdiscOutput(t *testing.T) {
	const raw = `[{"kind":"fq","handle":"0:","root":true,"refcnt":2,"options":{"limit":10000}}]`
	run := runnerFunc(func(_ context.Context, tool Tool, args []string) ([]byte, error) {
		if tool == IP {
			return []byte(`[{"ifindex":7,"ifname":"rd-egress","ifalias":"reladock-shaping-v1:node-owner"}]`), nil
		}
		if args[1] == "qdisc" {
			return []byte(raw), nil
		}
		return []byte(`[]`), nil
	})
	s, e := NewSystemBackend(run, func(context.Context) (string, error) { return "netns-test", nil }).Inspect(context.Background(), options().Lease)
	if e != nil {
		t.Fatal(e)
	}
	if s.RootFingerprint != "9d2b4892f3b23b6230a4b12a2818bf939af02f6ec0834cee744511f0d0972c62" {
		t.Fatal("raw qdisc fingerprint mismatch", s.RootFingerprint)
	}
}

func TestSystemBackendTreatsBlankClassAndFilterOutputAsEmpty(t *testing.T) {
	run := runnerFunc(func(_ context.Context, tool Tool, args []string) ([]byte, error) {
		if tool == IP {
			return []byte(`[{"ifindex":7,"ifname":"rd-egress","ifalias":"reladock-shaping-v1:node-owner"}]`), nil
		}
		if args[1] == "qdisc" {
			return []byte(`[{"kind":"noqueue","handle":"0:","root":true,"options":{}}]`), nil
		}
		return nil, nil
	})
	s, e := NewSystemBackend(run, func(context.Context) (string, error) { return "netns-test", nil }).Inspect(context.Background(), options().Lease)
	if e != nil {
		t.Fatal(e)
	}
	if len(s.Classes) != 0 || len(s.Filters) != 0 {
		t.Fatalf("blank tc inventory was not empty: %+v", s)
	}
}

func TestSystemBackendRejectsMalformedOrUnsupportedReadback(t *testing.T) {
	for _, bad := range []string{`null`, `{}`, `[{"kind":"htb","handle":"5244:","root":true,"options":{"default":"bogus"}}]`} {
		r := runnerFunc(func(_ context.Context, tool Tool, args []string) ([]byte, error) {
			if tool == IP {
				return []byte(`[{"ifindex":7,"ifname":"rd-egress","ifalias":"reladock-shaping-v1:node-owner"}]`), nil
			}
			if args[1] == "qdisc" {
				return []byte(bad), nil
			}
			return []byte(`[]`), nil
		})
		if _, e := NewSystemBackend(r, func(context.Context) (string, error) { return "netns-test", nil }).Inspect(context.Background(), options().Lease); e == nil {
			t.Fatal("bad tc JSON accepted", bad)
		}
	}
}

func TestSystemBackendAcceptsTopLevelHTBClassRates(t *testing.T) {
	r := runnerFunc(func(_ context.Context, tool Tool, args []string) ([]byte, error) {
		if tool == IP {
			return []byte(`[{"ifindex":7,"ifname":"rd-egress","ifalias":"reladock-shaping-v1:node-owner"}]`), nil
		}
		if args[1] == "qdisc" {
			return []byte(`[{"kind":"htb","handle":"5244:","root":true,"options":{"default":65535}}]`), nil
		}
		if args[1] == "class" {
			return []byte(`[{"class":"htb","handle":"5244:1","root":true,"rate":125000000,"ceil":125000000,"prio":0}]`), nil
		}
		return []byte(`[]`), nil
	})
	s, e := NewSystemBackend(r, func(context.Context) (string, error) { return "netns-test", nil }).Inspect(context.Background(), options().Lease)
	if e != nil || len(s.Classes) != 1 || s.Classes[0].RateBits != 1000000000 || s.Classes[0].Parent != RootHandle {
		t.Fatalf("top-level HTB class readback: %+v %v", s, e)
	}
}

func TestSystemBackendAcceptsParentScopedFlatFWRows(t *testing.T) {
	r := runnerFunc(func(_ context.Context, tool Tool, args []string) ([]byte, error) {
		if tool == IP {
			return []byte(`[{"ifindex":7,"ifname":"rd-egress","ifalias":"reladock-shaping-v1:node-owner"}]`), nil
		}
		if args[1] == "qdisc" {
			return []byte(`[{"kind":"htb","handle":"5244:","root":true,"options":{"default":65535}}]`), nil
		}
		if args[1] == "filter" {
			return []byte(`[{"protocol":"ip","pref":10,"kind":"fw","chain":0},{"protocol":"ip","pref":10,"kind":"fw","chain":0,"fw":{"mark":"0x52012300","mask":"0xffffff00"},"classid":"5244:123"}]`), nil
		}
		return []byte(`[]`), nil
	})
	s, e := NewSystemBackend(r, func(context.Context) (string, error) { return "netns-test", nil }).Inspect(context.Background(), options().Lease)
	if e != nil || s.Foreign || len(s.Filters) != 1 || s.Filters[0].Parent != RootHandle || s.Filters[0].Mark != 0x52012300 {
		t.Fatalf("parent-scoped flat fw readback %+v %v", s, e)
	}
}
