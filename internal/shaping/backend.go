package shaping

import (
	"bytes"
	"context"
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
func (b *SystemBackend) RunTC(c context.Context, args []string) error {
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
	classes, e := array(raw)
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
	filters, e := array(raw)
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
	write := tool == TC && ((args[0] == "qdisc" && args[1] == "add") || (args[0] == "class" && args[1] == "replace") || (args[0] == "filter" && args[1] == "replace"))
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
