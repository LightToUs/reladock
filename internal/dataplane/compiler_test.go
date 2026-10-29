package dataplane

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"

	"reladock/internal/platform"
)

type resolverFunc func(context.Context, string, string) ([]netip.Addr, error)

func (f resolverFunc) LookupNetIP(c context.Context, n, h string) ([]netip.Addr, error) {
	return f(c, n, h)
}

func fixture() platform.NodeConfig {
	return platform.NodeConfig{SchemaVersion: 1, NodeID: "hk-01", Generation: 1, ListenAddress: "192.0.2.1", Rules: []platform.ConfigRule{{ID: "rule-a", TenantID: "tenant-a", RuleInput: platform.RuleInput{Name: "Relay", NodeID: "hk-01", Protocol: "both", TargetHost: "198.51.100.2", TargetPort: 443, BandwidthMbps: 100}, ListenPort: 20000, Version: 1, Status: "pending-agent"}}}
}
func options() Options { return Options{NodeID: "hk-01", SNATAddress: "192.0.2.2"} }

func TestCompileBothAndPaused(t *testing.T) {
	cfg := fixture()
	paused := cfg.Rules[0]
	paused.ID = "paused"
	paused.ListenPort = 20001
	paused.Status = "paused"
	cfg.Rules = append(cfg.Rules, paused)
	p, err := Compile(context.Background(), cfg, options())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"add table ip reladock\ndelete table ip reladock\n", "type filter hook forward", "tcp dport 20000 dnat to 198.51.100.2:443", "udp dport 20000 dnat to 198.51.100.2:443", "snat to 192.0.2.2", "ct direction original", "ct direction reply", "ct original ip daddr 192.0.2.1", "ct original proto-dst 20000 jump relay", "drop"} {
		if !strings.Contains(p.Script, s) {
			t.Errorf("missing %q in %s", s, p.Script)
		}
	}
	if strings.Contains(p.Script, "dport 20001 dnat") || strings.Contains(p.Script, "flush ruleset") || strings.Contains(p.Script, "20000-60000") {
		t.Fatal(p.Script)
	}
	if len(p.Counters) != 4 {
		t.Fatalf("counters: %+v", p.Counters)
	}
	nat := strings.Split(p.Script, "chain forward")[0]
	if strings.Contains(nat, "counter name") {
		t.Fatal("NAT counters used for traffic")
	}
	for _, c := range p.Counters {
		if c.RuleID != "rule-a" || c.TenantID != "tenant-a" {
			t.Fatal(c)
		}
	}
}

func TestForwardScopeOnlyExplicitAllocations(t *testing.T) {
	p, e := Compile(context.Background(), fixture(), options())
	if e != nil {
		t.Fatal(e)
	}
	forward := strings.Split(strings.Split(p.Script, "chain forward")[1], "chain relay")[0]
	if strings.Contains(forward, "20000-60000") || strings.Count(forward, "jump relay") != 2 || strings.Contains(forward, "20001") {
		t.Fatal("scope includes unowned ports:", forward)
	}
	for _, proto := range []string{"tcp", "udp"} {
		if !strings.Contains(forward, "meta l4proto "+proto+" ct original ip daddr 192.0.2.1 ct original proto-dst 20000 jump relay") {
			t.Fatal("missing exact allocation guard", forward)
		}
	}
}

func TestCompileRejectsInvalidAndInjection(t *testing.T) {
	cases := map[string]func(*platform.NodeConfig){
		"node injection":   func(c *platform.NodeConfig) { c.NodeID = "hk; flush ruleset" },
		"rule injection":   func(c *platform.NodeConfig) { c.Rules[0].ID = "r\"; drop" },
		"tenant injection": func(c *platform.NodeConfig) { c.Rules[0].TenantID = "x\ny" },
		"host injection":   func(c *platform.NodeConfig) { c.Rules[0].TargetHost = "a.com;flush ruleset" },
		"url":              func(c *platform.NodeConfig) { c.Rules[0].TargetHost = "https://example.com" },
		"v6":               func(c *platform.NodeConfig) { c.Rules[0].TargetHost = "2001:db8::1" },
		"mapped v6":        func(c *platform.NodeConfig) { c.Rules[0].TargetHost = "::ffff:198.51.100.2" },
		"wildcard listen":  func(c *platform.NodeConfig) { c.ListenAddress = "0.0.0.0" },
		"multicast target": func(c *platform.NodeConfig) { c.Rules[0].TargetHost = "224.0.0.1" },
		"loopback target":  func(c *platform.NodeConfig) { c.Rules[0].TargetHost = "127.0.0.1" },
		"target local":     func(c *platform.NodeConfig) { c.Rules[0].TargetHost = c.ListenAddress },
		"status":           func(c *platform.NodeConfig) { c.Rules[0].Status = "active" },
		"port":             func(c *platform.NodeConfig) { c.Rules[0].TargetPort = 65536 },
		"listen port":      func(c *platform.NodeConfig) { c.Rules[0].ListenPort = 19999 },
		"protocol":         func(c *platform.NodeConfig) { c.Rules[0].Protocol = "tcp;drop" },
		"version":          func(c *platform.NodeConfig) { c.Rules[0].Version = 0 },
		"schema":           func(c *platform.NodeConfig) { c.SchemaVersion = 2 },
		"generation":       func(c *platform.NodeConfig) { c.Generation = 0 },
		"wrong node":       func(c *platform.NodeConfig) { c.Rules[0].NodeID = "other" },
		"bandwidth":        func(c *platform.NodeConfig) { c.Rules[0].BandwidthMbps = 0 },
		"duplicate":        func(c *platform.NodeConfig) { c.Rules = append(c.Rules, c.Rules[0]) },
		"reserved paused collision": func(c *platform.NodeConfig) {
			r := c.Rules[0]
			r.ID = "other"
			r.Status = "paused"
			c.Rules = append(c.Rules, r)
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			c := fixture()
			change(&c)
			if _, err := Compile(context.Background(), c, options()); err == nil {
				t.Fatal("accepted invalid configuration")
			}
		})
	}
	o := options()
	o.SNATAddress = "192.0.2.2; flush ruleset"
	if _, err := Compile(context.Background(), fixture(), o); err == nil {
		t.Fatal("SNAT injection accepted")
	}
}

func TestDNSResolutionIsPinnedAndStrict(t *testing.T) {
	c := fixture()
	c.Rules[0].TargetHost = "relay.example.com"
	for _, bad := range []struct {
		ips []netip.Addr
		err error
	}{{nil, errors.New("DNS failed")}, {nil, nil}, {[]netip.Addr{netip.MustParseAddr("198.51.100.2"), netip.MustParseAddr("2001:db8::1")}, nil}, {[]netip.Addr{netip.MustParseAddr("0.0.0.0")}, nil}} {
		o := options()
		o.Resolver = resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) { return bad.ips, bad.err })
		if _, err := Compile(context.Background(), c, o); err == nil {
			t.Fatal("bad DNS accepted")
		}
	}
	o := options()
	o.Resolver = resolverFunc(func(_ context.Context, n, h string) ([]netip.Addr, error) {
		if n != "ip" || h != "relay.example.com" {
			t.Fatalf("lookup %s %s", n, h)
		}
		return []netip.Addr{netip.MustParseAddr("198.51.100.3"), netip.MustParseAddr("198.51.100.2")}, nil
	})
	p, err := Compile(context.Background(), c, o)
	if err != nil {
		t.Fatal(err)
	}
	if p.Config.Rules[0].TargetHost != "198.51.100.2" || c.Rules[0].TargetHost != "relay.example.com" {
		t.Fatal("resolution not pinned or caller mutated")
	}
}

func TestSeparateTCPUDPAndEmptySnapshot(t *testing.T) {
	c := fixture()
	c.Rules[0].Protocol = "tcp"
	r := c.Rules[0]
	r.ID = "udp-rule"
	r.Protocol = "udp"
	c.Rules = append(c.Rules, r)
	if _, err := Compile(context.Background(), c, options()); err != nil {
		t.Fatal(err)
	}
	c.Rules = nil
	p, err := Compile(context.Background(), c, options())
	if err != nil || len(p.Counters) != 0 || strings.Contains(p.Script, "dnat to") {
		t.Fatalf("empty snapshot: %+v %v", p, err)
	}
}

func TestResolverNormalizesGoIPv4MappedRepresentation(t *testing.T) {
	c := fixture()
	c.Rules[0].TargetHost = "relay.example.com"
	o := options()
	// net.Resolver.LookupNetIP can represent A answers as 16-byte mapped
	// addresses. Normalize resolver output only; mapped config literals remain
	// unsupported and genuine IPv6 answers still fail the entire answer set.
	o.Resolver = resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("::ffff:198.51.100.2")}, nil
	})
	p, e := Compile(context.Background(), c, o)
	if e != nil {
		t.Fatal(e)
	}
	if p.Config.Rules[0].TargetHost != "198.51.100.2" {
		t.Fatal("A answer not normalized", p.Config.Rules[0].TargetHost)
	}
}
