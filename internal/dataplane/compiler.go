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
	NodeID               string
	SNATAddress          string
	Resolver             Resolver
	Shaping              *shaping.Options
	retired              []Allocation
	retiredGuards        []AllocationGuard
	quarantines          []AllocationGuard
	acceptedPredecessors map[string]platform.ConfigAllocation
	incarnation          string
	builtAt              time.Time
	quotaUsed            map[string]int64
}

// Allocation is an exact original conntrack tuple reserved by this platform.
// Historical entries remain guarded until an operator reconciles conntrack
// and allocation reuse; they are never inferred from a port range.
type Allocation struct {
	Address  string
	Protocol string
	Port     int
}

// AllocationGuard is an allocation-identity-bound kernel guard. RetirementID
// is set only for pre-conntrack quarantines and retiring forward guards.
type AllocationGuard struct {
	ID                      string
	RetirementID            string
	Address                 string
	Protocol                string
	Port                    int
	State                   string
	PredecessorRetirementID string
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
	NodeID        string
	Generation    int64
	Incarnation   string
	BuiltAt       time.Time
	QuotaUsed     map[string]int64
	SNATAddress   string
	Config        platform.NodeConfig
	Shaping       *shaping.Options
	Script        string
	Counters      []Counter
	Allocations   []Allocation
	Quarantines   []AllocationGuard
	ForwardGuards []AllocationGuard
}

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,79}$`)
var uuid = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
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

func guardTuple(address, protocol string, port int) string {
	return fmt.Sprintf("%s/%s/%d", address, protocol, port)
}

func Compile(ctx context.Context, cfg platform.NodeConfig, o Options) (Plan, error) {
	fail := func(msg string) (Plan, error) { return Plan{}, fmt.Errorf("invalid dataplane config: %s", msg) }
	if (cfg.SchemaVersion != 1 && cfg.SchemaVersion != 2) || cfg.Generation < 1 || !identifier.MatchString(cfg.NodeID) || cfg.NodeID != o.NodeID {
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
	cfg.Allocations = append([]platform.ConfigAllocation(nil), cfg.Allocations...)
	cfg.Retirements = append([]platform.ConfigRetirement(nil), cfg.Retirements...)
	ids := map[string]bool{}
	ports := map[string]bool{}
	ruleStatus := map[string]string{}
	for i := range cfg.Rules {
		r := &cfg.Rules[i]
		if !identifier.MatchString(r.ID) || !identifier.MatchString(r.TenantID) || ids[r.ID] || r.NodeID != cfg.NodeID || r.Version < 1 {
			return fail("rule identity/version")
		}
		ids[r.ID] = true
		ruleStatus[r.ID] = r.Status
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
	var forwardGuards, quarantines []AllocationGuard
	if cfg.SchemaVersion == 1 {
		if len(cfg.Allocations) != 0 || len(cfg.Retirements) != 0 {
			return fail("schema-v1 allocation metadata")
		}
	} else {
		expected := map[string]string{}
		activeByTuple := map[string]platform.ConfigAllocation{}
		for _, r := range cfg.Rules {
			for _, proto := range protocols(r.Protocol) {
				expected[fmt.Sprintf("%s:%d", proto, r.ListenPort)] = r.ID
			}
		}
		allocationIDs := map[string]bool{}
		allocationTuples := map[string]bool{}
		predecessorIDs := map[string]bool{}
		for _, a := range cfg.Allocations {
			key := fmt.Sprintf("%s:%d", a.Protocol, a.Port)
			if !uuid.MatchString(a.ID) || allocationIDs[a.ID] || !identifier.MatchString(a.RuleID) || a.Address != listen.String() || (a.Protocol != "tcp" && a.Protocol != "udp") || a.Port < 20000 || a.Port > 60000 || allocationTuples[key] || expected[key] != a.RuleID || a.PredecessorRetirementID != "" && (!uuid.MatchString(a.PredecessorRetirementID) || predecessorIDs[a.PredecessorRetirementID]) {
				return fail("allocation identity/tuple")
			}
			if a.PredecessorRetirementID != "" {
				predecessorIDs[a.PredecessorRetirementID] = true
			}
			allocationIDs[a.ID] = true
			allocationTuples[key] = true
			activeByTuple[guardTuple(a.Address, a.Protocol, a.Port)] = a
			forwardGuards = append(forwardGuards, AllocationGuard{ID: a.ID, Address: a.Address, Protocol: a.Protocol, Port: a.Port})
		}
		if len(allocationTuples) != len(expected) {
			return fail("incomplete allocation metadata")
		}
		retirementIDs := map[string]bool{}
		retirementTuples := map[string]bool{}
		retirementByTuple := map[string]platform.ConfigRetirement{}
		states := map[string]bool{"waiting-removal": true, "queued": true, "claimed": true, "purging": true, "failed": true}
		for _, r := range cfg.Retirements {
			key := fmt.Sprintf("%s:%d", r.Protocol, r.Port)
			if !uuid.MatchString(r.ID) || retirementIDs[r.ID] || !uuid.MatchString(r.AllocationID) || allocationIDs[r.AllocationID] || !identifier.MatchString(r.RuleID) || r.Address != listen.String() || (r.Protocol != "tcp" && r.Protocol != "udp") || r.Port < 20000 || r.Port > 60000 || allocationTuples[key] || retirementTuples[key] || r.QuarantineGeneration < 1 || r.QuarantineGeneration > cfg.Generation || !states[r.State] {
				return fail("retirement identity/tuple")
			}
			retirementIDs[r.ID] = true
			allocationIDs[r.AllocationID] = true
			retirementTuples[key] = true
			retirementByTuple[guardTuple(r.Address, r.Protocol, r.Port)] = r
			guard := AllocationGuard{ID: r.AllocationID, RetirementID: r.ID, Address: r.Address, Protocol: r.Protocol, Port: r.Port, State: r.State}
			quarantines = append(quarantines, guard)
			forwardGuards = append(forwardGuards, guard)
		}
		localQuarantines := map[string]AllocationGuard{}
		knownPredecessors := map[string]platform.ConfigAllocation{}
		for _, guard := range o.quarantines {
			tuple := guardTuple(guard.Address, guard.Protocol, guard.Port)
			if !uuid.MatchString(guard.ID) || !uuid.MatchString(guard.RetirementID) || guard.Address != listen.String() || (guard.Protocol != "tcp" && guard.Protocol != "udp") || guard.Port < 20000 || guard.Port > 60000 || (guard.State != "purged-pending-ack" && guard.State != "reusable") || localQuarantines[tuple].ID != "" {
				return fail("local quarantine identity/tuple")
			}
			localQuarantines[tuple] = guard
			if active, ok := activeByTuple[tuple]; ok {
				if guard.State != "reusable" {
					return fail("unacknowledged quarantine cannot be consumed")
				}
				if active.PredecessorRetirementID != guard.RetirementID {
					return fail("active allocation lacks exact quarantine predecessor")
				}
				knownPredecessors[guard.RetirementID] = active
				if ruleStatus[active.RuleID] == "pending-agent" {
					continue
				}
				quarantines = append(quarantines, guard)
				continue
			}
			if remote, ok := retirementByTuple[tuple]; ok {
				if remote.ID != guard.RetirementID || remote.AllocationID != guard.ID {
					return fail("local quarantine conflicts with retirement")
				}
				filtered := forwardGuards[:0]
				for _, candidate := range forwardGuards {
					if candidate.ID != guard.ID {
						filtered = append(filtered, candidate)
					}
				}
				forwardGuards = filtered
				for i := range quarantines {
					if quarantines[i].ID == guard.ID {
						quarantines[i] = guard
					}
				}
				continue
			}
			quarantines = append(quarantines, guard)
		}
		for _, guard := range o.retiredGuards {
			tuple := guardTuple(guard.Address, guard.Protocol, guard.Port)
			if !uuid.MatchString(guard.ID) || guard.RetirementID != "" || guard.PredecessorRetirementID != "" && !uuid.MatchString(guard.PredecessorRetirementID) || guard.Address != listen.String() || (guard.Protocol != "tcp" && guard.Protocol != "udp") || guard.Port < 20000 || guard.Port > 60000 || guard.State != "failed-candidate" {
				return fail("historical guard identity/tuple")
			}
			if active, ok := activeByTuple[tuple]; ok {
				if active.ID == guard.ID {
					continue
				}
				return fail("active allocation overlaps historical guard")
			}
			if remote, ok := retirementByTuple[tuple]; ok {
				if remote.AllocationID == guard.ID {
					continue
				}
				return fail("retirement overlaps historical guard")
			}
			if quarantine, ok := localQuarantines[tuple]; ok {
				if guard.PredecessorRetirementID != quarantine.RetirementID {
					return fail("quarantine overlaps unrelated historical guard")
				}
				forwardGuards = append(forwardGuards, guard)
				continue
			}
			for _, candidate := range forwardGuards {
				if candidate.ID == guard.ID || guardTuple(candidate.Address, candidate.Protocol, candidate.Port) == tuple {
					return fail("duplicate historical guard")
				}
			}
			forwardGuards = append(forwardGuards, guard)
		}
		for _, allocation := range cfg.Allocations {
			if allocation.PredecessorRetirementID != "" {
				known, local := knownPredecessors[allocation.PredecessorRetirementID]
				if local && (known.ID != allocation.ID || known.RuleID != allocation.RuleID || known.Address != allocation.Address || known.Protocol != allocation.Protocol || known.Port != allocation.Port) {
					return fail("allocation predecessor identity mismatch")
				}
				if local {
					continue
				}
				accepted, ok := o.acceptedPredecessors[allocation.PredecessorRetirementID]
				if !ok || accepted.ID != allocation.ID || accepted.RuleID != allocation.RuleID || accepted.Address != allocation.Address || accepted.Protocol != allocation.Protocol || accepted.Port != allocation.Port {
					return fail("unknown allocation predecessor")
				}
			}
		}
		sort.Slice(cfg.Allocations, func(i, j int) bool { return cfg.Allocations[i].ID < cfg.Allocations[j].ID })
		sort.Slice(cfg.Retirements, func(i, j int) bool { return cfg.Retirements[i].ID < cfg.Retirements[j].ID })
	}
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
	sort.Slice(forwardGuards, func(i, j int) bool { return forwardGuards[i].ID < forwardGuards[j].ID })
	sort.Slice(quarantines, func(i, j int) bool { return quarantines[i].ID < quarantines[j].ID })
	p := Plan{NodeID: cfg.NodeID, Generation: cfg.Generation, Incarnation: o.incarnation, SNATAddress: snat.String(), Config: cfg, Shaping: o.Shaping, ForwardGuards: forwardGuards, Quarantines: quarantines}
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
	if cfg.SchemaVersion == 2 {
		for _, v := range p.ForwardGuards {
			allocations[Allocation{v.Address, v.Protocol, v.Port}] = true
		}
		for _, v := range p.Quarantines {
			allocations[Allocation{v.Address, v.Protocol, v.Port}] = true
		}
		protected := map[Allocation]bool{}
		for _, v := range p.ForwardGuards {
			protected[Allocation{v.Address, v.Protocol, v.Port}] = true
		}
		for _, v := range p.Quarantines {
			protected[Allocation{v.Address, v.Protocol, v.Port}] = true
		}
		for _, v := range o.retired {
			if !protected[v] {
				return fail("historical allocation lacks identity-bound protection")
			}
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
	var nat, snatRules, forward, definitions, guards, quarantineRules strings.Builder
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
	if cfg.SchemaVersion == 1 {
		for _, v := range p.Allocations {
			fmt.Fprintf(&guards, "  ct status dnat meta l4proto %s ct original ip daddr %s ct original proto-dst %d jump relay\n", v.Protocol, v.Address, v.Port)
		}
	} else {
		for _, v := range p.Quarantines {
			fmt.Fprintf(&quarantineRules, "  ip daddr %s meta l4proto %s %s dport %d drop comment \"reladock-quarantine-%s\"\n", v.Address, v.Protocol, v.Protocol, v.Port, v.ID)
		}
		for _, v := range p.ForwardGuards {
			fmt.Fprintf(&guards, "  ct status dnat meta l4proto %s ct original ip daddr %s ct original proto-dst %d jump relay comment \"reladock-forward-%s\"\n", v.Protocol, v.Address, v.Port, v.ID)
		}
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
	quarantineChain := ""
	if cfg.SchemaVersion == 2 {
		quarantineChain = fmt.Sprintf(" chain quarantine {\n  type filter hook prerouting priority raw; policy accept;\n%s }\n", quarantineRules.String())
	}
	p.Script = fmt.Sprintf("add table ip %s\ndelete table ip %s\ntable ip %s {\n comment \"%s\"\n%s%s chain prerouting {\n  type nat hook prerouting priority dstnat; policy accept;\n%s }\n chain postrouting {\n  type nat hook postrouting priority srcnat; policy accept;\n%s }\n chain forward {\n  type filter hook forward priority filter; policy accept;\n%s }\n chain relay {\n%s  drop\n }\n}\n", Table, Table, Table, owner, definitions.String(), quarantineChain, nat.String(), snatRules.String(), guards.String(), forward.String())
	p.Script = strings.Replace(p.Script, "chain relay {\n", "chain relay {\n  comment \""+planToken(p)+"\"\n", 1)
	return p, nil
}
