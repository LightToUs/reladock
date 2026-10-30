// Package shaping plans isolated per-rule combined-direction HTB ceilings.
// It neither configures nft/routes nor reports platform applied/health ACKs.
package shaping

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"sort"

	"reladock/internal/platform"
)

const RootHandle = "5244:"
const ParentClass = "5244:1"
const DefaultClass = "5244:ffff"
const MarkMask uint32 = 0xffffff00

type Lease struct {
	Device                  string
	IfIndex                 int
	NamespaceID             string
	OwnerID                 string
	Dedicated               bool
	ReplaceDefaultQdisc     bool
	DefaultQdiscFingerprint string
	MarkNamespace           uint8
	LinkMbps                int
}

func (l Lease) Alias() string { return "reladock-shaping-v1:" + l.OwnerID }

type Options struct {
	NodeID         string
	Lease          Lease
	UploadEgress   string
	DownloadEgress string
}
type Class struct {
	RuleID   string
	TenantID string
	ClassID  string
	Mark     uint32
	Mask     uint32
	CeilMbps int
	Active   bool
}
type Marking struct {
	RuleID   string
	TenantID string
	Protocol string
	Mark     uint32
	Mask     uint32
	ClassID  string
	NFT      string
}
type Plan struct {
	Config   platform.NodeConfig
	Options  Options
	Classes  []Class
	Markings []Marking
	Commands [][]string
}

var id = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,79}$`)
var device = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,14}$`)
var qdiscFingerprint = regexp.MustCompile(`^[0-9a-f]{64}$`)

func validLease(l Lease) bool {
	validHandoff := !l.ReplaceDefaultQdisc && l.DefaultQdiscFingerprint == "" || l.ReplaceDefaultQdisc && qdiscFingerprint.MatchString(l.DefaultQdiscFingerprint)
	return id.MatchString(l.OwnerID) && device.MatchString(l.Device) && l.Device != "lo" && l.Dedicated && validHandoff && l.IfIndex >= 1 && len(l.NamespaceID) >= 1 && len(l.NamespaceID) <= 256 && l.MarkNamespace != 0 && l.LinkMbps >= 1 && l.LinkMbps <= 100000
}
func validateOptions(o Options) error {
	l := o.Lease
	if !id.MatchString(o.NodeID) || !validLease(l) || o.UploadEgress != l.Device || o.DownloadEgress != l.Device {
		return errors.New("explicit dedicated lease and same upload/download egress required")
	}
	return nil
}

func classFor(node string, r platform.ConfigRule, namespace uint8) Class {
	h := sha256.Sum256([]byte(node + "/" + r.TenantID + "/" + r.ID))
	slot := uint32(binary.BigEndian.Uint16(h[:2]))%65533 + 2
	return Class{RuleID: r.ID, TenantID: r.TenantID, ClassID: fmt.Sprintf("5244:%x", slot), Mark: uint32(namespace)<<24 | slot<<8, Mask: MarkMask, CeilMbps: r.BandwidthMbps, Active: r.Status == "pending-agent"}
}
func protocols(p string) []string {
	if p == "both" {
		return []string{"tcp", "udp"}
	}
	return []string{p}
}

func Compile(cfg platform.NodeConfig, o Options) (Plan, error) { return compile(cfg, o, nil) }

// history reserves every class/mark ever used on this lease, including failed
// candidates, so hash collisions never silently transfer an old classifier.
func compile(cfg platform.NodeConfig, o Options, history []Class) (Plan, error) {
	if e := validateOptions(o); e != nil {
		return Plan{}, e
	}
	bad := func() (Plan, error) {
		return Plan{}, errors.New("invalid shaping configuration or historical classifier collision")
	}
	addr, e := netip.ParseAddr(cfg.ListenAddress)
	if e != nil || !addr.Is4() || !addr.IsGlobalUnicast() || addr.IsLoopback() || cfg.SchemaVersion != 1 || cfg.Generation < 1 || cfg.NodeID != o.NodeID || len(cfg.Rules) > 65533 {
		return bad()
	}
	cfg.Rules = append([]platform.ConfigRule(nil), cfg.Rules...)
	sort.Slice(cfg.Rules, func(i, j int) bool { return cfg.Rules[i].ID < cfg.Rules[j].ID })
	byIdentity := map[string]Class{}
	marks := map[uint32]string{}
	for _, c := range history {
		if !id.MatchString(c.RuleID) || !id.MatchString(c.TenantID) || c.CeilMbps < 1 || c.CeilMbps > 1000 || c.CeilMbps > o.Lease.LinkMbps {
			return bad()
		}
		expected := classFor(cfg.NodeID, platform.ConfigRule{ID: c.RuleID, TenantID: c.TenantID, RuleInput: platform.RuleInput{BandwidthMbps: c.CeilMbps}}, o.Lease.MarkNamespace)
		if expected.Mark != c.Mark || c.Mask != MarkMask || expected.ClassID != c.ClassID {
			return bad()
		}
		key := c.TenantID + "/" + c.RuleID
		if prev, ok := marks[c.Mark]; ok && prev != key {
			return bad()
		}
		marks[c.Mark] = key
		c.Active = false
		byIdentity[key] = c
	}
	seen := map[string]bool{}
	ports := map[string]bool{}
	var markings []Marking
	for _, r := range cfg.Rules {
		if !id.MatchString(r.ID) || !id.MatchString(r.TenantID) || seen[r.ID] || r.NodeID != cfg.NodeID || r.Version < 1 || (r.Status != "pending-agent" && r.Status != "paused") || (r.Protocol != "tcp" && r.Protocol != "udp" && r.Protocol != "both") || r.ListenPort < 20000 || r.ListenPort > 60000 || r.BandwidthMbps < 1 || r.BandwidthMbps > 1000 || r.BandwidthMbps > o.Lease.LinkMbps {
			return bad()
		}
		seen[r.ID] = true
		c := classFor(cfg.NodeID, r, o.Lease.MarkNamespace)
		key := c.TenantID + "/" + c.RuleID
		if prev, ok := marks[c.Mark]; ok && prev != key {
			return bad()
		}
		marks[c.Mark] = key
		byIdentity[key] = c
		for _, proto := range protocols(r.Protocol) {
			port := fmt.Sprintf("%s/%d", proto, r.ListenPort)
			if ports[port] {
				return bad()
			}
			ports[port] = true
			if c.Active {
				markings = append(markings, Marking{RuleID: c.RuleID, TenantID: c.TenantID, Protocol: proto, Mark: c.Mark, Mask: c.Mask, ClassID: c.ClassID, NFT: fmt.Sprintf("oifname \"%s\" ct status dnat meta l4proto %s ct original ip daddr %s ct original proto-dst %d ct direction { original, reply } meta mark set (meta mark & 0x000000ff) | 0x%08x", o.Lease.Device, proto, addr, r.ListenPort, c.Mark)})
			}
		}
	}
	p := Plan{Config: cfg, Options: o, Markings: markings}
	for _, c := range byIdentity {
		p.Classes = append(p.Classes, c)
	}
	sort.Slice(p.Classes, func(i, j int) bool {
		a, b := p.Classes[i], p.Classes[j]
		if a.TenantID != b.TenantID {
			return a.TenantID < b.TenantID
		}
		return a.RuleID < b.RuleID
	})
	p.Commands = commands(p)
	return p, nil
}

func burst(mbps int) string {
	n := int64(mbps) * 1000000 / 8 / 100
	if n < 1600 {
		n = 1600
	}
	return fmt.Sprint(n)
}
func commands(p Plan) [][]string {
	dev := p.Options.Lease.Device
	link := fmt.Sprintf("%dmbit", p.Options.Lease.LinkMbps)
	cmds := [][]string{{"class", "replace", "dev", dev, "parent", RootHandle, "classid", ParentClass, "htb", "rate", link, "ceil", link, "burst", burst(p.Options.Lease.LinkMbps), "cburst", burst(p.Options.Lease.LinkMbps)},
		{"class", "replace", "dev", dev, "parent", ParentClass, "classid", DefaultClass, "htb", "rate", "8bit", "ceil", link, "quantum", "1514", "burst", burst(p.Options.Lease.LinkMbps), "cburst", burst(p.Options.Lease.LinkMbps)}}
	for _, c := range p.Classes {
		cmds = append(cmds, []string{"class", "replace", "dev", dev, "parent", ParentClass, "classid", c.ClassID, "htb", "rate", "8bit", "ceil", fmt.Sprintf("%dmbit", c.CeilMbps), "quantum", "1514", "burst", burst(c.CeilMbps), "cburst", burst(c.CeilMbps)})
		cmds = append(cmds, []string{"filter", "replace", "dev", dev, "parent", RootHandle, "protocol", "ip", "prio", "10", "handle", fmt.Sprintf("0x%08x/0xffffff00", c.Mark), "fw", "classid", c.ClassID})
	}
	// Classifiers from this reserved mark namespace must never fall through to
	// the unclassified default when the exact fw filter/class is absent.
	cmds = append(cmds, []string{"filter", "replace", "dev", dev, "parent", RootHandle, "protocol", "ip", "prio", "20", "handle", fmt.Sprintf("0x%08x/0xff000000", uint32(p.Options.Lease.MarkNamespace)<<24), "fw", "action", "drop"})
	return cmds
}
