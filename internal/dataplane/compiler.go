// Package dataplane compiles isolated IPv4 nftables transactions. It does not
// report platform ACKs, health, billing usage, or bandwidth enforcement.
package dataplane

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math"
	"net/netip"
	"regexp"
	"sort"
	"strings"
	"time"

	"reladock/internal/platform"
	"reladock/internal/shaping"
)

const Table = "reladock"
const owner = "reladock-dataplane-v1"

// Kernel TTLs expire conservatively. Apply/recovery are bounded to 30 seconds,
// so slow preparation must not renew an already expiring paid entitlement.
const ExpirySafetyMargin = 35 * time.Second

type Resolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}
type Options struct {
	NodeID      string
	SNATAddress string
	Resolver    Resolver
	Shaping     *shaping.Options
	retired     []Allocation
	incarnation string
	builtAt     time.Time
	quotaUsed   map[string]int64
}

// Allocation is an exact original conntrack tuple reserved by this platform.
// Historical entries remain guarded until an operator reconciles conntrack
// and allocation reuse; they are never inferred from a port range.
type Allocation struct {
	Address  string
	Protocol string
	Port     int
}
type Counter struct {
	Name      string
	RuleID    string
	TenantID  string
	Protocol  string
	Direction string
}

// Plan freezes DNS answers. Script is diagnostic output, never accepted as an
// application input; Apply compiles a NodeConfig itself.
type Plan struct {
	NodeID      string
	Generation  int64
	Incarnation string
	BuiltAt     time.Time
	QuotaUsed   map[string]int64
	SNATAddress string
	Config      platform.NodeConfig
	Shaping     *shaping.Options
	Script      string
	Counters    []Counter
	Allocations []Allocation
}

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,79}$`)
var hostname = regexp.MustCompile(`^(?i:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)(?:\.(?i:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?))+$`)

func ipv4(s string) (netip.Addr, error) {
	a, e := netip.ParseAddr(s)
	if e != nil || !a.Is4() || !a.IsGlobalUnicast() || a.IsLoopback() || a.String() == "255.255.255.255" {
		return netip.Addr{}, fmt.Errorf("unsupported IPv4 address %q", s)
	}
	return a, nil
}

func target(ctx context.Context, h string, r Resolver) (string, error) {
	if a, e := netip.ParseAddr(h); e == nil {
		v, e := ipv4(a.String())
		return v.String(), e
	}
	if len(h) > 253 || !hostname.MatchString(h) {
		return "", fmt.Errorf("invalid target host %q", h)
	}
	if r == nil {
		return "", fmt.Errorf("DNS resolver required for %q", h)
	}
	ips, e := r.LookupNetIP(ctx, "ip", h)
	if e != nil {
		return "", fmt.Errorf("resolve %s: %w", h, e)
	}
	if len(ips) == 0 {
		return "", fmt.Errorf("no addresses for %s", h)
	}
	// Reject the entire answer set, including mixed IPv4/IPv6, rather than
	// silently selecting a different address family or an unsafe answer.
	values := make([]string, 0, len(ips))
	for _, a := range ips {
		// Go's system resolver may encode an A answer as IPv4-mapped IPv6.
		// Normalize that representation here, never config literals; real IPv6
		// answers still reject the complete mixed-family result.
		v, e := ipv4(a.Unmap().String())
		if e != nil {
			return "", e
		}
		values = append(values, v.String())
	}
	sort.Strings(values)
	return values[0], nil
}

func protocols(p string) []string {
	if p == "both" {
		return []string{"tcp", "udp"}
	}
	return []string{p}
}

func Compile(ctx context.Context, cfg platform.NodeConfig, o Options) (Plan, error) {
	fail := func(msg string) (Plan, error) { return Plan{}, fmt.Errorf("invalid dataplane config: %s", msg) }
	if cfg.SchemaVersion != 1 || cfg.Generation < 1 || !identifier.MatchString(cfg.NodeID) || cfg.NodeID != o.NodeID {
		return fail("schema, generation or node identity")
	}
	listen, e := ipv4(cfg.ListenAddress)
	if e != nil {
		return Plan{}, e
	}
	snat, e := ipv4(o.SNATAddress)
	if e != nil {
		return Plan{}, e
	}
	cfg.ListenAddress = listen.String()
	cfg.Rules = append([]platform.ConfigRule(nil), cfg.Rules...)
	ids := map[string]bool{}
	ports := map[string]bool{}
	for i := range cfg.Rules {
		r := &cfg.Rules[i]
		if !identifier.MatchString(r.ID) || !identifier.MatchString(r.TenantID) || ids[r.ID] || r.NodeID != cfg.NodeID || r.Version < 1 {
			return fail("rule identity/version")
		}
		ids[r.ID] = true
		if r.Status != "pending-agent" && r.Status != "paused" {
			return fail("rule status")
		}
		if r.Protocol != "tcp" && r.Protocol != "udp" && r.Protocol != "both" {
			return fail("protocol")
		}
		if r.ListenPort < 20000 || r.ListenPort > 60000 || r.TargetPort < 1 || r.TargetPort > 65535 || r.BandwidthMbps < 1 || r.BandwidthMbps > 1000 {
			return fail("port/bandwidth range")
		}
		for _, proto := range protocols(r.Protocol) {
			key := fmt.Sprintf("%s:%d", proto, r.ListenPort)
			if ports[key] {
				return fail("duplicate reserved port")
			}
			ports[key] = true
		}
		// Paused rules still undergo literal validation but need no DNS request.
		if _, e := netip.ParseAddr(r.TargetHost); e == nil {
			if _, e = ipv4(r.TargetHost); e != nil {
				return Plan{}, e
			}
		} else if len(r.TargetHost) > 253 || !hostname.MatchString(r.TargetHost) {
			return fail("target host")
		}
		if r.Status == "paused" {
			continue
		}
		ip, e := target(ctx, r.TargetHost, o.Resolver)
		if e != nil {
			return Plan{}, e
		}
		if ip == listen.String() || ip == snat.String() {
			return fail("target equals ingress or SNAT address")
		}
		r.TargetHost = ip
	}
	sort.Slice(cfg.Rules, func(i, j int) bool { return cfg.Rules[i].ID < cfg.Rules[j].ID })
	marks := map[string]string{}
	if o.Shaping != nil {
		sp, err := shaping.Compile(cfg, *o.Shaping)
		if err != nil {
			return Plan{}, err
		}
		for _, c := range sp.Classes {
			marks[c.RuleID] = fmt.Sprintf("oifname \"%s\" meta mark set (meta mark & 0x000000ff) | 0x%08x ", o.Shaping.Lease.Device, c.Mark)
		}
	}
	if o.incarnation != "" && !identifier.MatchString(o.incarnation) {
		return fail("invalid kernel incarnation")
	}
	p := Plan{NodeID: cfg.NodeID, Generation: cfg.Generation, Incarnation: o.incarnation, SNATAddress: snat.String(), Config: cfg, Shaping: o.Shaping}
	allocations := map[Allocation]bool{}
	for _, v := range o.retired {
		if _, e := ipv4(v.Address); e != nil || v.Address != listen.String() || (v.Protocol != "tcp" && v.Protocol != "udp") || v.Port < 20000 || v.Port > 60000 {
			return fail("invalid historical allocation")
		}
		allocations[v] = true
	}
	for _, r := range cfg.Rules {
		for _, proto := range protocols(r.Protocol) {
			allocations[Allocation{listen.String(), proto, r.ListenPort}] = true
		}
	}
	for v := range allocations {
		p.Allocations = append(p.Allocations, v)
	}
	sort.Slice(p.Allocations, func(i, j int) bool {
		a, b := p.Allocations[i], p.Allocations[j]
		if a.Address != b.Address {
			return a.Address < b.Address
		}
		if a.Protocol != b.Protocol {
			return a.Protocol < b.Protocol
		}
		return a.Port < b.Port
	})
	var nat, snatRules, forward, definitions, guards strings.Builder
	quotaMatch := map[string]string{}
	exhausted := map[string]bool{}
	for _, r := range cfg.Rules {
		if r.QuotaBytes < 0 || r.UsageUploadBytes < 0 || r.UsageDownloadBytes < 0 || r.UsageUploadBytes > math.MaxInt64-r.UsageDownloadBytes {
			return fail("invalid quota/usage")
		}
		if r.Status != "pending-agent" || r.QuotaBytes == 0 {
			continue
		}
		used := r.UsageUploadBytes + r.UsageDownloadBytes
		if local, ok := o.quotaUsed[r.ID]; ok {
			if local < used {
				return fail("local quota baseline below snapshot")
			}
			used = local
		}
		if p.QuotaUsed == nil {
			p.QuotaUsed = map[string]int64{}
		}
		p.QuotaUsed[r.ID] = used
		if used >= r.QuotaBytes {
			exhausted[r.ID] = true
			continue
		}
		h := sha256.Sum256([]byte(r.ID))
		name := fmt.Sprintf("q_%x", h[:8])
		fmt.Fprintf(&definitions, " quota %s { until %d bytes; }\n", name, r.QuotaBytes-used)
		quotaMatch[r.ID] = "quota name " + name + " "
	}
	expiryMatch := map[string]string{}
	for _, r := range cfg.Rules {
		if r.Status != "pending-agent" || r.ExpiresAt == nil {
			continue
		}
		if p.BuiltAt.IsZero() {
			p.BuiltAt = o.builtAt
			if p.BuiltAt.IsZero() {
				p.BuiltAt = time.Now().UTC()
			}
		}
		h := sha256.Sum256([]byte(r.ID))
		name := fmt.Sprintf("e_%x", h[:8])
		ttl := r.ExpiresAt.Sub(p.BuiltAt) - ExpirySafetyMargin
		fmt.Fprintf(&definitions, " set %s { type inet_service; flags timeout;", name)
		if ttl.Milliseconds() > 0 {
			timeout := fmt.Sprintf("%dms", ttl.Milliseconds())
			if ttl.Milliseconds() > math.MaxInt32 {
				timeout = fmt.Sprintf("%ds", ttl/time.Second)
			}
			fmt.Fprintf(&definitions, " elements = { %d timeout %s };", r.ListenPort, timeout)
		}
		definitions.WriteString(" }\n")
		expiryMatch[r.ID] = "ct original proto-dst @" + name + " "
	}
	for _, v := range p.Allocations {
		fmt.Fprintf(&guards, "  ct status dnat meta l4proto %s ct original ip daddr %s ct original proto-dst %d jump relay\n", v.Protocol, v.Address, v.Port)
	}
	for _, r := range cfg.Rules {
		if r.Status == "paused" || exhausted[r.ID] {
			continue
		}
		for _, proto := range protocols(r.Protocol) {
			fmt.Fprintf(&nat, "  ip daddr %s %s dport %d dnat to %s:%d\n", listen, proto, r.ListenPort, r.TargetHost, r.TargetPort)
			tuple := fmt.Sprintf("meta l4proto %s ct original ip daddr %s ct original proto-dst %d", proto, listen, r.ListenPort)
			fmt.Fprintf(&snatRules, "  ct status dnat %s ip daddr %s %s dport %d snat to %s\n", tuple, r.TargetHost, proto, r.TargetPort, snat)
			for _, dir := range []string{"original", "reply"} {
				hash := sha256.Sum256([]byte(r.TenantID + "/" + r.ID + "/" + proto + "/" + dir))
				name := fmt.Sprintf("c_%x", hash[:16])
				p.Counters = append(p.Counters, Counter{Name: name, RuleID: r.ID, TenantID: r.TenantID, Protocol: proto, Direction: dir})
				fmt.Fprintf(&definitions, " counter %s {}\n", name)
				addr, port := "daddr", "dport"
				if dir == "reply" {
					addr, port = "saddr", "sport"
				}
				fmt.Fprintf(&forward, "  %s ct direction %s ip %s %s %s %s %d %s%s%scounter name %s accept\n", tuple, dir, addr, r.TargetHost, proto, port, r.TargetPort, expiryMatch[r.ID], marks[r.ID], quotaMatch[r.ID], name)
			}
		}
	}
	// add is idempotent, delete + recreate is in the same nft transaction. Only
	// this table is replaced, including named counters and all chain objects.
	p.Script = fmt.Sprintf("add table ip %s\ndelete table ip %s\ntable ip %s {\n comment \"%s\"\n%s chain prerouting {\n  type nat hook prerouting priority dstnat; policy accept;\n%s }\n chain postrouting {\n  type nat hook postrouting priority srcnat; policy accept;\n%s }\n chain forward {\n  type filter hook forward priority filter; policy accept;\n%s }\n chain relay {\n%s  drop\n }\n}\n", Table, Table, Table, owner, definitions.String(), nat.String(), snatRules.String(), guards.String(), forward.String())
	p.Script = strings.Replace(p.Script, "chain relay {\n", "chain relay {\n  comment \""+planToken(p)+"\"\n", 1)
	return p, nil
}
