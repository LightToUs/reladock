package shaping

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"reladock/internal/platform"
)

func config() platform.NodeConfig {
	return platform.NodeConfig{SchemaVersion: 1, NodeID: "hk-01", Generation: 1, ListenAddress: "192.0.2.1", Rules: []platform.ConfigRule{{ID: "rule-a", TenantID: "tenant-a", RuleInput: platform.RuleInput{NodeID: "hk-01", Name: "relay", Protocol: "both", TargetHost: "198.51.100.2", TargetPort: 443, BandwidthMbps: 10}, ListenPort: 20000, Version: 1, Status: "pending-agent"}}}
}
func options() Options {
	return Options{NodeID: "hk-01", Lease: Lease{Device: "rd-egress", IfIndex: 7, NamespaceID: "netns-test", OwnerID: "node-owner", Dedicated: true, MarkNamespace: 0x52, LinkMbps: 1000}, UploadEgress: "rd-egress", DownloadEgress: "rd-egress"}
}

func TestCompileOneSharedClassBothProtocolsAndDirections(t *testing.T) {
	p, e := Compile(config(), options())
	if e != nil {
		t.Fatal(e)
	}
	if len(p.Classes) != 1 || len(p.Markings) != 2 {
		t.Fatalf("one class for both protocols/directions: %+v", p)
	}
	c := p.Classes[0]
	if c.CeilMbps != 10 || c.Mark&0xff != 0 || c.Mark>>24 != 0x52 || c.Mask != 0xffffff00 {
		t.Fatal(c)
	}
	for _, m := range p.Markings {
		if m.Mark != c.Mark || m.ClassID != c.ClassID || !strings.Contains(m.NFT, "oifname \"rd-egress\"") || !strings.Contains(m.NFT, "meta mark & 0x000000ff") || !strings.Contains(m.NFT, "ct direction { original, reply }") {
			t.Fatal(m)
		}
	}
	for _, cmd := range p.Commands {
		if strings.Contains(strings.Join(cmd, " "), "qdisc replace") || strings.Contains(strings.Join(cmd, " "), "qdisc del") {
			t.Fatal("unsafe root mutation", cmd)
		}
	}
	if !strings.Contains(join(p.Commands), "ceil 10mbit") {
		t.Fatal(join(p.Commands))
	}
}
func join(cmds [][]string) string {
	var b strings.Builder
	for _, c := range cmds {
		b.WriteString(strings.Join(c, " "))
		b.WriteByte('\n')
	}
	return b.String()
}

func TestCompileStableAcrossReorderGenerationRateAndPause(t *testing.T) {
	cfg := config()
	r := cfg.Rules[0]
	r.ID = "rule-b"
	r.ListenPort = 20001
	cfg.Rules = append(cfg.Rules, r)
	p, e := Compile(cfg, options())
	if e != nil {
		t.Fatal(e)
	}
	cfg.Generation = 2
	cfg.Rules[0], cfg.Rules[1] = cfg.Rules[1], cfg.Rules[0]
	cfg.Rules[0].BandwidthMbps = 20
	q, e := Compile(cfg, options())
	if e != nil {
		t.Fatal(e)
	}
	for i := range p.Classes {
		if p.Classes[i].Mark != q.Classes[i].Mark || p.Classes[i].ClassID != q.Classes[i].ClassID {
			t.Fatal("unstable classifier")
		}
	}
	cfg.Rules[0].Status = "paused"
	q, e = Compile(cfg, options())
	if e != nil {
		t.Fatal(e)
	}
	if len(q.Markings) != 2 || len(q.Classes) != 2 {
		t.Fatal("paused rule emits marks or loses reservation")
	}
}

func TestCompileRejectsInvalidOwnershipTopologyAndInjection(t *testing.T) {
	for name, mutate := range map[string]func(*platform.NodeConfig, *Options){
		"not dedicated":    func(_ *platform.NodeConfig, o *Options) { o.Lease.Dedicated = false },
		"split egress":     func(_ *platform.NodeConfig, o *Options) { o.DownloadEgress = "other0" },
		"ifindex":          func(_ *platform.NodeConfig, o *Options) { o.Lease.IfIndex = 0 },
		"namespace":        func(_ *platform.NodeConfig, o *Options) { o.Lease.NamespaceID = "" },
		"mark namespace":   func(_ *platform.NodeConfig, o *Options) { o.Lease.MarkNamespace = 0 },
		"owner injection":  func(_ *platform.NodeConfig, o *Options) { o.Lease.OwnerID = "x;root" },
		"device injection": func(_ *platform.NodeConfig, o *Options) { o.Lease.Device = "x\nqdisc del" },
		"loopback":         func(_ *platform.NodeConfig, o *Options) { o.Lease.Device = "lo" },
		"unknown status":   func(c *platform.NodeConfig, _ *Options) { c.Rules[0].Status = "active" },
		"rule injection":   func(c *platform.NodeConfig, _ *Options) { c.Rules[0].ID = "x\";drop" },
		"generation":       func(c *platform.NodeConfig, _ *Options) { c.Generation = 0 },
		"schema":           func(c *platform.NodeConfig, _ *Options) { c.SchemaVersion = 2 },
		"bandwidth":        func(c *platform.NodeConfig, _ *Options) { c.Rules[0].BandwidthMbps = 0 },
		"capacity":         func(c *platform.NodeConfig, o *Options) { o.Lease.LinkMbps = 1; c.Rules[0].BandwidthMbps = 2 },
		"protocol":         func(c *platform.NodeConfig, _ *Options) { c.Rules[0].Protocol = "ip" },
		"duplicate":        func(c *platform.NodeConfig, _ *Options) { c.Rules = append(c.Rules, c.Rules[0]) },
		"port collision": func(c *platform.NodeConfig, _ *Options) {
			r := c.Rules[0]
			r.ID = "other"
			c.Rules = append(c.Rules, r)
		},
		"wildcard": func(c *platform.NodeConfig, _ *Options) { c.ListenAddress = "0.0.0.0" },
		"family":   func(c *platform.NodeConfig, _ *Options) { c.ListenAddress = "2001:db8::1" },
	} {
		t.Run(name, func(t *testing.T) {
			c, o := config(), options()
			mutate(&c, &o)
			if _, e := Compile(c, o); e == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}

func TestCompileRejectsMarkCollisionRatherThanReassigning(t *testing.T) {
	// Find a deterministic collision independently of the compiler API.
	ids := map[uint16]string{}
	first, second := "", ""
	for i := 0; i < 2000; i++ {
		id := fmt.Sprintf("r-%d", i)
		h := sha256.Sum256([]byte("hk-01/tenant-a/" + id))
		slot := uint16(uint32(binary.BigEndian.Uint16(h[:2]))%65533 + 2)
		if old, ok := ids[slot]; ok {
			first, second = old, id
			break
		}
		ids[slot] = id
	}
	if first == "" {
		t.Fatal("test fixture collision not found")
	}
	c := config()
	c.Rules[0].ID = first
	r := c.Rules[0]
	r.ID = second
	r.ListenPort = 20001
	c.Rules = append(c.Rules, r)
	if _, e := Compile(c, options()); e == nil {
		t.Fatal("mark collision accepted")
	}
}

func TestCompileDoesNotMutateInput(t *testing.T) {
	c := config()
	before := config()
	if _, e := Compile(c, options()); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(c, before) {
		t.Fatal("caller config mutated")
	}
}
