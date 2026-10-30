package shaping

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

type Tool string

const (
	TC Tool = "tc"
	IP Tool = "ip"
)

type Runner interface {
	Run(context.Context, Tool, []string) ([]byte, error)
}
type SystemBackend struct {
	runner    Runner
	namespace func(context.Context) (string, error)
}

func NewSystemBackend(r Runner, namespace func(context.Context) (string, error)) *SystemBackend {
	return &SystemBackend{r, namespace}
}
func (b *SystemBackend) RunTC(c context.Context, lease Lease, args []string) error {
	if !validLease(lease) || !tcWriteAllowed(args, lease.Device) {
		return errors.New("unsupported network operation")
	}
	if args[0] == "qdisc" && args[1] == "replace" {
		if !lease.ReplaceDefaultQdisc {
			return errors.New("root replacement requires explicit lease authorization")
		}
		snap, e := b.Inspect(c, lease)
		if e != nil {
			return e
		}
		if !replaceableDefault(snap, lease) {
			return errors.New("kernel default qdisc fingerprint or interface lease changed")
		}
	}
	_, e := b.runner.Run(c, TC, args)
	return e
}

func array(data []byte) ([]map[string]json.RawMessage, error) {
	var v []map[string]json.RawMessage
	if e := json.Unmarshal(data, &v); e != nil {
		return nil, e
	}
	if v == nil {
		return nil, errors.New("missing JSON array")
	}
	return v, nil
}
func optionalArray(data []byte) ([]map[string]json.RawMessage, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return []map[string]json.RawMessage{}, nil
	}
	return array(data)
}

func tcRateBytes(value string) (uint64, error) {
	units := []struct {
		suffix     string
		multiplier uint64
	}{{"Tbit", 1_000_000_000_000}, {"Gbit", 1_000_000_000}, {"Mbit", 1_000_000}, {"Kbit", 1_000}, {"bit", 1}}
	for _, unit := range units {
		if !strings.HasSuffix(value, unit.suffix) {
			continue
		}
		number := strings.TrimSuffix(value, unit.suffix)
		if !decimal(number) {
			return 0, errors.New("invalid tc rate number")
		}
		n, err := strconv.ParseUint(number, 10, 64)
		if err != nil || n > ^uint64(0)/unit.multiplier {
			return 0, errors.New("tc rate overflow")
		}
		bits := n * unit.multiplier
		if bits%8 != 0 || bits/8 > 1<<60 {
			return 0, errors.New("tc rate is not representable")
		}
		return bits / 8, nil
	}
	return 0, errors.New("unsupported tc rate unit")
}

func tcByteQuantity(value string) error {
	if !strings.HasSuffix(value, "b") || !decimal(strings.TrimSuffix(value, "b")) {
		return errors.New("invalid legacy HTB byte quantity")
	}
	_, err := strconv.ParseUint(strings.TrimSuffix(value, "b"), 10, 64)
	if err != nil {
		return errors.New("legacy HTB byte quantity overflow")
	}
	return nil
}

func rawJSON(value any) json.RawMessage {
	b, _ := json.Marshal(value)
	return b
}

// classArray accepts JSON where iproute2 implements it and the strict HTB
// text emitted by iproute2 5.15 even when -j is requested. Other tc inventory
// remains JSON-only; this compatibility path does not relax lease validation.
func classArray(data []byte) ([]map[string]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return []map[string]json.RawMessage{}, nil
	}
	if trimmed[0] == '[' || trimmed[0] == '{' {
		return array(trimmed)
	}
	rows := make([]map[string]json.RawMessage, 0)
	seen := map[string]bool{}
	for _, line := range strings.Split(string(trimmed), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 7 || fields[0] != "class" || fields[1] != "htb" || !classID(fields[2]) || seen[fields[2]] {
			return nil, errors.New("invalid legacy HTB class row")
		}
		seen[fields[2]] = true
		row := map[string]json.RawMessage{"kind": rawJSON("htb"), "handle": rawJSON(fields[2])}
		start := 0
		switch fields[3] {
		case "root":
			row["root"] = rawJSON(true)
			start = 4
		case "parent":
			if len(fields) < 8 || !classID(fields[4]) {
				return nil, errors.New("invalid legacy HTB parent")
			}
			row["parent"] = rawJSON(fields[4])
			start = 5
		default:
			return nil, errors.New("legacy HTB class lacks root or parent")
		}
		var rate, ceil uint64
		attributes := map[string]bool{}
		for i := start; i < len(fields); i++ {
			name := fields[i]
			if attributes[name] {
				return nil, fmt.Errorf("duplicate legacy HTB %s", name)
			}
			attributes[name] = true
			switch name {
			case "root", "parent":
				return nil, errors.New("duplicate legacy HTB relation")
			case "rate", "ceil":
				if i+1 >= len(fields) {
					return nil, errors.New("legacy HTB rate lacks value")
				}
				v, err := tcRateBytes(fields[i+1])
				if err != nil {
					return nil, err
				}
				if name == "rate" {
					rate = v
				} else {
					ceil = v
				}
				i++
			case "prio":
				if i+1 >= len(fields) {
					return nil, errors.New("legacy HTB priority lacks value")
				}
				priority, err := strconv.ParseUint(fields[i+1], 10, 64)
				if err != nil {
					return nil, errors.New("invalid legacy HTB priority")
				}
				row["prio"] = rawJSON(priority)
				i++
			case "burst", "cburst":
				if i+1 >= len(fields) {
					return nil, fmt.Errorf("legacy HTB %s lacks value", name)
				}
				if err := tcByteQuantity(fields[i+1]); err != nil {
					return nil, err
				}
				i++
			default:
				return nil, fmt.Errorf("unsupported legacy HTB attribute %s", name)
			}
		}
		if !attributes["rate"] || !attributes["ceil"] {
			return nil, errors.New("legacy HTB class lacks rate or ceil")
		}
		row["rate"], row["ceil"] = rawJSON(rate), rawJSON(ceil)
		rows = append(rows, row)
	}
	return rows, nil
}
func str(m map[string]json.RawMessage, k string) string {
	var s string
	_ = json.Unmarshal(m[k], &s)
	return s
}
func obj(m map[string]json.RawMessage, k string) map[string]json.RawMessage {
	var v map[string]json.RawMessage
	_ = json.Unmarshal(m[k], &v)
	return v
}
func flag(m map[string]json.RawMessage, k string) bool {
	var v bool
	_ = json.Unmarshal(m[k], &v)
	return v
}
func number(v json.RawMessage) (uint64, error) {
	var n uint64
	if e := json.Unmarshal(v, &n); e == nil {
		return n, nil
	}
	var s string
	if e := json.Unmarshal(v, &s); e != nil {
		return 0, e
	}
	return strconv.ParseUint(s, 0, 64)
}

func (b *SystemBackend) Inspect(ctx context.Context, l Lease) (Snapshot, error) {
	var s Snapshot
	if b.runner == nil || b.namespace == nil || !device.MatchString(l.Device) {
		return s, errors.New("backend identity/runner missing or invalid device")
	}
	ns, e := b.namespace(ctx)
	if e != nil {
		return s, e
	}
	s.NamespaceID = ns
	raw, e := b.runner.Run(ctx, IP, []string{"-j", "-d", "link", "show", "dev", l.Device})
	if e != nil {
		return s, e
	}
	links, e := array(raw)
	if e != nil || len(links) != 1 {
		return s, errors.New("unambiguous link JSON required")
	}
	s.Device = str(links[0], "ifname")
	s.Alias = str(links[0], "ifalias")
	idx, e := number(links[0]["ifindex"])
	if e != nil || idx > 2147483647 {
		return s, errors.New("invalid ifindex")
	}
	s.IfIndex = int(idx)
	raw, e = b.runner.Run(ctx, TC, []string{"-j", "qdisc", "show", "dev", l.Device})
	if e != nil {
		return s, e
	}
	qdiscs, e := array(raw)
	if e != nil {
		return s, e
	}
	s.RootFingerprint = fmt.Sprintf("%x", sha256.Sum256(raw))
	roots := 0
	for _, q := range qdiscs {
		kind, handle := str(q, "kind"), str(q, "handle")
		if flag(q, "root") {
			roots++
			s.RootKind = kind
			s.RootHandle = handle
			if kind == "htb" {
				options := obj(q, "options")
				n, e := number(options["default"])
				if e != nil || n > 65535 {
					return s, errors.New("invalid HTB default class")
				}
				s.RootDefault = uint32(n)
				if flag(options, "offload") {
					s.Foreign = true
				}
			}
		} else if kind != "pfifo" || handle != "0:" || !strings.HasPrefix(str(q, "parent"), RootHandle) {
			return s, fmt.Errorf("unsupported child qdisc: kind=%s handle=%s parent=%s", kind, handle, str(q, "parent"))
		}
	}
	if roots > 1 {
		return s, errors.New("multiple root qdiscs")
	}
	raw, e = b.runner.Run(ctx, TC, []string{"-j", "class", "show", "dev", l.Device})
	if e != nil {
		return s, e
	}
	classes, e := classArray(raw)
	if e != nil {
		return s, e
	}
	for _, v := range classes {
		c := ClassState{ID: str(v, "handle"), Parent: str(v, "parent"), Kind: str(v, "kind")}
		if c.Kind == "" {
			c.Kind = str(v, "class")
		}
		if c.Parent == "" && flag(v, "root") {
			c.Parent = RootHandle
		}
		o := obj(v, "options")
		// iproute2 releases differ: HTB class attributes may be emitted at the
		// class object's top level, unlike the qdisc's nested options object.
		if o == nil {
			o = v
		}
		rate, e := number(o["rate"])
		if e != nil || rate > 1<<60 {
			encoded, _ := json.Marshal(v)
			return s, fmt.Errorf("invalid HTB rate for %s: %s", c.ID, encoded)
		}
		ceil, e := number(o["ceil"])
		if e != nil || ceil > 1<<60 {
			return s, errors.New("invalid HTB ceil")
		}
		c.RateBits = rate * 8
		c.CeilBits = ceil * 8
		if c.Kind != "htb" {
			return s, fmt.Errorf("unsupported class kind %s", c.Kind)
		}
		if prio, ok := o["prio"]; ok {
			n, e := number(prio)
			if e != nil || n != 0 {
				return s, fmt.Errorf("unsupported class priority: %s", o["prio"])
			}
		}
		s.Classes = append(s.Classes, c)
	}
	raw, e = b.runner.Run(ctx, TC, []string{"-j", "filter", "show", "dev", l.Device, "parent", RootHandle})
	if e != nil {
		return s, e
	}
	filters, e := optionalArray(raw)
	if e != nil {
		return s, e
	}
	for _, v := range filters {
		f := FilterState{Kind: str(v, "kind"), Parent: str(v, "parent"), Protocol: str(v, "protocol")}
		// This query is explicitly parent-scoped; some iproute2 releases omit
		// that redundant parent field and print fw attributes at the top level.
		if f.Parent == "" {
			f.Parent = RootHandle
		}
		if f.Kind != "fw" || f.Parent != RootHandle || f.Protocol != "ip" {
			encoded, _ := json.Marshal(v)
			return s, fmt.Errorf("unsupported filter header: %s", encoded)
		}
		pref, e := number(v["pref"])
		if e != nil || pref > 65535 {
			return s, errors.New("invalid filter priority")
		}
		f.Priority = int(pref)
		if ch, ok := v["chain"]; ok {
			n, e := number(ch)
			if e != nil || n != 0 {
				s.Foreign = true
			}
		}
		o := obj(v, "options")
		if o == nil && v["fw"] != nil {
			o = v
		}
		if len(o) == 0 {
			continue
		} // tc emits classifier-head summary rows too.
		fw := obj(o, "fw")
		mark, e := number(fw["mark"])
		if e != nil || mark > 0xffffffff {
			return s, errors.New("invalid fw mark")
		}
		mask, e := number(fw["mask"])
		if e != nil || mask > 0xffffffff {
			return s, errors.New("invalid fw mask")
		}
		f.Mark = uint32(mark)
		f.Mask = uint32(mask)
		f.ClassID = str(o, "classid")
		if str(o, "indev") != "" || o["police"] != nil {
			s.Foreign = true
		}
		if actions, ok := o["actions"]; ok {
			a, e := array(actions)
			if e != nil || len(a) != 1 {
				return s, errors.New("unsupported filter action list")
			}
			control := obj(a[0], "control_action")
			if str(a[0], "kind") != "gact" || str(control, "type") != "drop" {
				s.Foreign = true
			} else {
				f.Drop = true
			}
		}
		s.Filters = append(s.Filters, f)
	}
	if s.Foreign {
		q, _ := json.Marshal(qdiscs)
		return s, fmt.Errorf("unsupported HTB offload or fw action: qdiscs=%s filters=%s", q, raw)
	}
	return s, nil
}

// NewLinuxBackend reads only the caller's namespace and requires trusted
// absolute tool paths. No default backend is constructed by Compile.
func NewLinuxBackend(tcPath, ipPath string) (*SystemBackend, error) {
	if runtime.GOOS != "linux" || !filepath.IsAbs(tcPath) || !filepath.IsAbs(ipPath) {
		return nil, errors.New("Linux and absolute tc/ip paths required")
	}
	r := &execRunner{tcPath, ipPath}
	return NewSystemBackend(r, func(ctx context.Context) (string, error) {
		if e := ctx.Err(); e != nil {
			return "", e
		}
		return os.Readlink("/proc/self/ns/net")
	}), nil
}

type execRunner struct{ tcPath, ipPath string }
type limitedOutput struct{ bytes.Buffer }

func tcWriteAllowed(args []string, expectedDevice string) bool {
	if len(args) < 2 {
		return false
	}
	if !device.MatchString(expectedDevice) || len(args) < 4 || args[2] != "dev" || args[3] != expectedDevice {
		return false
	}
	if len(args) == 10 && args[0] == "qdisc" && (args[1] == "add" || args[1] == "replace") {
		return args[4] == "root" && args[5] == "handle" && args[6] == RootHandle && args[7] == "htb" && args[8] == "default" && args[9] == "ffff"
	}
	if args[0] == "class" && args[1] == "replace" {
		return classWriteAllowed(args)
	}
	if args[0] == "filter" && args[1] == "replace" {
		return filterWriteAllowed(args)
	}
	return false
}

func decimal(s string) bool {
	if len(s) == 0 || len(s) > 20 || s[0] == '0' && len(s) > 1 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
func mbit(s string) bool {
	return strings.HasSuffix(s, "mbit") && decimal(strings.TrimSuffix(s, "mbit"))
}
func classID(s string) bool {
	if !strings.HasPrefix(s, RootHandle) || len(s) <= len(RootHandle) || len(s) > len(RootHandle)+4 {
		return false
	}
	_, e := strconv.ParseUint(strings.TrimPrefix(s, RootHandle), 16, 16)
	return e == nil
}
func classWriteAllowed(a []string) bool {
	if len(a) == 17 && a[4] == "parent" && a[5] == RootHandle && a[6] == "classid" && a[7] == ParentClass && a[8] == "htb" && a[9] == "rate" && mbit(a[10]) && a[11] == "ceil" && a[12] == a[10] && a[13] == "burst" && decimal(a[14]) && a[15] == "cburst" && a[16] == a[14] {
		return true
	}
	return len(a) == 19 && a[4] == "parent" && a[5] == ParentClass && a[6] == "classid" && classID(a[7]) && a[8] == "htb" && a[9] == "rate" && a[10] == "8bit" && a[11] == "ceil" && mbit(a[12]) && a[13] == "quantum" && a[14] == "1514" && a[15] == "burst" && decimal(a[16]) && a[17] == "cburst" && a[18] == a[16]
}
func hexMark(s, mask string) bool {
	parts := strings.Split(s, "/")
	if len(parts) != 2 || parts[1] != mask || len(parts[0]) != 10 || !strings.HasPrefix(parts[0], "0x") {
		return false
	}
	_, e := strconv.ParseUint(strings.TrimPrefix(parts[0], "0x"), 16, 32)
	return e == nil
}
func filterWriteAllowed(a []string) bool {
	if len(a) != 15 || a[4] != "parent" || a[5] != RootHandle || a[6] != "protocol" || a[7] != "ip" || a[8] != "prio" || a[10] != "handle" || a[12] != "fw" {
		return false
	}
	if a[9] == "10" {
		return hexMark(a[11], "0xffffff00") && a[13] == "classid" && classID(a[14])
	}
	return a[9] == "20" && hexMark(a[11], "0xff000000") && strings.HasSuffix(strings.Split(a[11], "/")[0], "000000") && a[13] == "action" && a[14] == "drop"
}

func (b *limitedOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 8*1024*1024 {
		return 0, errors.New("tool output too large")
	}
	return b.Buffer.Write(p)
}
func (r *execRunner) Run(ctx context.Context, tool Tool, args []string) ([]byte, error) {
	if runtime.GOOS != "linux" {
		return nil, errors.New("network execution supported only on Linux")
	}
	path := ""
	switch tool {
	case TC:
		path = r.tcPath
	case IP:
		path = r.ipPath
	default:
		return nil, errors.New("unsupported tool")
	}
	if len(args) < 2 {
		return nil, errors.New("unsupported command")
	}
	read := args[0] == "-j"
	write := tool == TC && len(args) >= 4 && tcWriteAllowed(args, args[3])
	if !read && !write {
		return nil, errors.New("unsupported network operation")
	}
	cmd := exec.CommandContext(ctx, path, args...)
	out, stderr := &limitedOutput{}, &limitedOutput{}
	cmd.Stdout = out
	cmd.Stderr = stderr
	if e := cmd.Run(); e != nil {
		return nil, fmt.Errorf("%s: %w: %s", tool, e, stderr.String())
	}
	return out.Bytes(), nil
}
