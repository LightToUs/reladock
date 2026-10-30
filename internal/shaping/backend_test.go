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

func inspectLegacyClasses(t *testing.T, raw string) (Snapshot, error) {
	t.Helper()
	r := runnerFunc(func(_ context.Context, tool Tool, args []string) ([]byte, error) {
		if tool == IP {
			return []byte(`[{"ifindex":7,"ifname":"rd-egress","ifalias":"reladock-shaping-v1:node-owner"}]`), nil
		}
		if args[1] == "qdisc" {
			return []byte(`[{"kind":"htb","handle":"5244:","root":true,"options":{"default":65535}}]`), nil
		}
		if args[1] == "class" {
			return []byte(raw), nil
		}
		return []byte(`[]`), nil
	})
	return NewSystemBackend(r, func(context.Context) (string, error) { return "netns-test", nil }).Inspect(context.Background(), options().Lease)
}

func TestSystemBackendAcceptsLegacyHTBClassTextFromIPRoute215(t *testing.T) {
	raw := "class htb 5244:1 root prio 0 rate 1Gbit ceil 1Gbit burst 1250000b cburst 1250000b\n" +
		"class htb 5244:11c0 parent 5244:1 prio 0 rate 8bit ceil 1000Mbit burst 1250000b cburst 1250000b\n"
	s, err := inspectLegacyClasses(t, raw)
	if err != nil {
		t.Fatal(err)
	}
	want := []ClassState{
		{ID: "5244:1", Parent: RootHandle, Kind: "htb", RateBits: 1_000_000_000, CeilBits: 1_000_000_000},
		{ID: "5244:11c0", Parent: "5244:1", Kind: "htb", RateBits: 8, CeilBits: 1_000_000_000},
	}
	if !reflect.DeepEqual(s.Classes, want) {
		t.Fatalf("legacy classes %+v, want %+v", s.Classes, want)
	}
}

func TestSystemBackendLegacyClassTextFailsClosed(t *testing.T) {
	tests := map[string]string{
		"malformed JSON-looking": `[{"kind":"htb"}`,
		"unknown class kind":     "class cake 5244:1 root rate 1Gbit ceil 1Gbit\n",
		"missing rate":           "class htb 5244:1 root ceil 1Gbit\n",
		"missing ceil":           "class htb 5244:1 root rate 1Gbit\n",
		"duplicate rate":         "class htb 5244:1 root rate 1Gbit rate 1Gbit ceil 1Gbit\n",
		"duplicate class":        "class htb 5244:1 root rate 1Gbit ceil 1Gbit\nclass htb 5244:1 root rate 1Gbit ceil 1Gbit\n",
		"unsupported unit":       "class htb 5244:1 root rate 1Gbps ceil 1Gbit\n",
		"sub-byte rate":          "class htb 5244:1 root rate 1bit ceil 1Gbit\n",
		"overflow":               "class htb 5244:1 root rate 18446744073709551615Tbit ceil 1Gbit\n",
		"nonzero priority":       "class htb 5244:1 root prio 7 rate 1Gbit ceil 1Gbit burst 1250000b cburst 1250000b\n",
		"duplicate priority":     "class htb 5244:1 root prio 0 prio 0 rate 1Gbit ceil 1Gbit burst 1250000b cburst 1250000b\n",
		"unknown attribute":      "class htb 5244:1 root prio 0 rate 1Gbit ceil 1Gbit frobnicate 1\n",
		"missing burst value":    "class htb 5244:1 root prio 0 rate 1Gbit ceil 1Gbit burst\n",
		"invalid burst value":    "class htb 5244:1 root prio 0 rate 1Gbit ceil 1Gbit burst nope\n",
		"trailing garbage":       "class htb 5244:1 root prio 0 rate 1Gbit ceil 1Gbit garbage\n",
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := inspectLegacyClasses(t, raw); err == nil {
				t.Fatal("unsafe legacy class output accepted")
			}
		})
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

func inspectLegacyFilters(t *testing.T, raw string) (Snapshot, error) {
	t.Helper()
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
		if args[1] == "filter" {
			return []byte(raw), nil
		}
		return []byte(`[]`), nil
	})
	return NewSystemBackend(r, func(context.Context) (string, error) { return "netns-test", nil }).Inspect(context.Background(), options().Lease)
}

func TestSystemBackendAcceptsLegacyIPRoute215HybridFWJSON(t *testing.T) {
	raw := `[{"protocol":"ip","pref":10,"kind":"fw","chain":0},{"protocol":"ip","pref":10,"kind":"fw","chain":0,"options":{handle 0x5211c000/0xffffff00 classid 5244:11c0 }},{"protocol":"ip","pref":20,"kind":"fw","chain":0},{"protocol":"ip","pref":20,"kind":"fw","chain":0,"options":{handle 0x52000000/0xff000000 ` + "\n" +
		`"actions":[{"order":1,"kind":"gact","control_action":{"type":"drop"},"prob":{"random_type":"none","control_action":{"type":"pass"},"val":0},"index":1,"ref":1,"bind":1}]}}]`
	s, err := inspectLegacyFilters(t, raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Filters) != 2 || s.Filters[0].Mark != 0x5211c000 || s.Filters[0].Mask != 0xffffff00 || s.Filters[0].ClassID != "5244:11c0" || s.Filters[0].Drop || s.Filters[1].Mark != 0x52000000 || s.Filters[1].Mask != 0xff000000 || !s.Filters[1].Drop {
		t.Fatalf("legacy hybrid filters: %+v", s.Filters)
	}
}

func TestSystemBackendLegacyHybridFWJSONFailsClosed(t *testing.T) {
	tests := map[string]string{
		"missing classid":  `[{"protocol":"ip","pref":10,"kind":"fw","chain":0,"options":{handle 0x5211c000/0xffffff00 classid }}]`,
		"oversized mark":   `[{"protocol":"ip","pref":10,"kind":"fw","chain":0,"options":{handle 0x15211c000/0xffffff00 classid 5244:11c0 }}]`,
		"unknown option":   `[{"protocol":"ip","pref":10,"kind":"fw","chain":0,"options":{handle 0x5211c000/0xffffff00 police nope classid 5244:11c0 }}]`,
		"trailing garbage": `[{"protocol":"ip","pref":10,"kind":"fw","chain":0,"options":{handle 0x5211c000/0xffffff00 classid 5244:11c0 garbage }}]`,
		"malformed action": `[{"protocol":"ip","pref":20,"kind":"fw","chain":0,"options":{handle 0x52000000/0xff000000 actions nope}}]`,
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := inspectLegacyFilters(t, raw); err == nil {
				t.Fatal("unsafe legacy hybrid filter output accepted")
			}
		})
	}
}
