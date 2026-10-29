package shaping

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This harness uses one existing local image and --network none. All interface,
// tc and nft changes are confined to a disposable container namespace. The
// dummy-device rate experiment is not an end-to-end relay topology test.
func TestDockerHTBReadbackAndCombinedRuleCeilings(t *testing.T) {
	if os.Getenv("RELADOCK_SHAPING_DOCKER") != "1" {
		t.Skip("isolated shaping Docker test requires explicit opt-in")
	}
	image := os.Getenv("RELADOCK_SHAPING_IMAGE")
	if image == "" {
		t.Fatal("explicit existing local image required; no image pulls")
	}
	docker, e := exec.LookPath("docker")
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	run := func(c context.Context, input string, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(c, docker, args...)
		cmd.Stdin = strings.NewReader(input)
		var out, errbuf bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &errbuf
		if e := cmd.Run(); e != nil {
			return nil, fmt.Errorf("docker %v: %w: %s", args, e, errbuf.String())
		}
		return out.Bytes(), nil
	}
	must := func(input string, args ...string) []byte {
		t.Helper()
		out, e := run(ctx, input, args...)
		if e != nil {
			t.Fatal(e)
		}
		return out
	}
	must("", "image", "inspect", image)
	var entropy [8]byte
	if _, e = rand.Read(entropy[:]); e != nil {
		t.Fatal(e)
	}
	name := "reladock-test-shaping-" + hex.EncodeToString(entropy[:])
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, e := run(c, "", "rm", "-f", name); e != nil {
			t.Errorf("container cleanup: %v", e)
		}
	})
	must("", "run", "-d", "--pull=never", "--network", "none", "--name", name, "--label", "reladock-test=true", "--cap-drop=ALL", "--cap-add=NET_ADMIN", "--security-opt", "no-new-privileges=true", "--user", "0:0", "--entrypoint", "sleep", image, "infinity")
	execute := func(tool string, args ...string) []byte {
		return must("", append([]string{"exec", name, tool}, args...)...)
	}
	execute("ip", "link", "add", "rd-egress", "type", "dummy")
	execute("ip", "link", "set", "dev", "rd-egress", "alias", "reladock-shaping-v1:integration-owner")
	execute("ip", "link", "set", "dev", "rd-egress", "up")
	execute("ip", "addr", "add", "198.18.0.1/24", "dev", "rd-egress")
	ns := strings.TrimSpace(string(execute("readlink", "/proc/self/ns/net")))
	links, e := array(execute("ip", "-j", "link", "show", "dev", "rd-egress"))
	if e != nil || len(links) != 1 {
		t.Fatal("fixture identity", e)
	}
	index, e := number(links[0]["ifindex"])
	if e != nil {
		t.Fatal(e)
	}
	o := options()
	o.Lease.IfIndex = int(index)
	o.Lease.NamespaceID = ns
	o.Lease.OwnerID = "integration-owner"
	r := runnerFunc(func(c context.Context, tool Tool, args []string) ([]byte, error) {
		return run(c, "", append([]string{"exec", name, string(tool)}, args...)...)
	})
	b := NewSystemBackend(r, func(c context.Context) (string, error) {
		out, e := run(c, "", "exec", name, "readlink", "/proc/self/ns/net")
		return strings.TrimSpace(string(out)), e
	})
	store, e := NewFileStore(filepath.Join(t.TempDir(), "state"))
	if e != nil {
		t.Fatal(e)
	}
	a, e := NewAdapter(o, b, store, store.Locker())
	if e != nil {
		t.Fatal(e)
	}
	cfg := config()
	cfg.Rules[0].BandwidthMbps = 2
	second := cfg.Rules[0]
	second.ID = "rule-b"
	second.ListenPort = 20001
	second.BandwidthMbps = 4
	second.Protocol = "udp"
	cfg.Rules = append(cfg.Rules, second)
	p, e := a.Apply(ctx, cfg)
	if e != nil {
		t.Fatal("real Linux HTB/fw apply/readback", e)
	}
	// Same-generation path must verify actual Linux readback, not return early
	// merely because a matching generation exists on disk.
	if _, e = a.ApplyVerified(ctx, cfg); e != nil {
		t.Fatal("real idempotent verification", e)
	}
	if e = a.Verify(ctx, cfg); e != nil {
		t.Fatal("real read-only post-nft verification", e)
	}
	var ca, cb Class
	for _, c := range p.Classes {
		if c.RuleID == "rule-a" {
			ca = c
		} else if c.RuleID == "rule-b" {
			cb = c
		}
	}
	if ca.Mark == 0 || cb.Mark == 0 {
		t.Fatal("missing rule classifiers")
	}
	script := fmt.Sprintf("table ip reladock_shaping_test {\n chain output {\n type filter hook output priority mangle; policy accept;\n udp dport {30001,30002} meta mark set (meta mark & 0x000000ff) | 0x%08x\n udp dport 30003 meta mark set (meta mark & 0x000000ff) | 0x%08x\n }\n}\n", ca.Mark, cb.Mark)
	must(script, "exec", "-i", name, "nft", "-c", "-f", "-")
	must(script, "exec", "-i", name, "nft", "-f", "-")
	readBytes := func() map[string]uint64 {
		t.Helper()
		items, e := array(execute("tc", "-j", "-s", "class", "show", "dev", "rd-egress"))
		if e != nil {
			t.Fatal(e)
		}
		counts := map[string]uint64{}
		for _, v := range items {
			stats := obj(v, "stats")
			if stats == nil {
				stats = v
			}
			n, e := number(stats["bytes"])
			if e != nil {
				t.Fatal("missing tc byte readback", e)
			}
			counts[str(v, "handle")] = n
		}
		return counts
	}
	before := readBytes()
	started := time.Now()
	execute("python3", "-c", dummyTraffic)
	after := readBytes()
	elapsed := time.Since(started).Seconds()
	rateA := float64(after[ca.ClassID]-before[ca.ClassID]) * 8 / elapsed
	rateB := float64(after[cb.ClassID]-before[cb.ClassID]) * 8 / elapsed
	// Two concurrent logical directions share A's 2mbit bucket; B independently
	// reaches 4mbit. Allow timing/token-bucket tolerance, never a 2x A ceiling.
	if rateA < 1000000 || rateA > 2800000 || rateB < 2600000 || rateB > 5200000 || rateB < rateA*1.3 {
		t.Fatalf("actual HTB ceilings: A(two flows)=%.2f Mbit/s B=%.2f Mbit/s; elapsed %.2fs", rateA/1e6, rateB/1e6, elapsed)
	}
	t.Logf("actual tc bytes: one shared class A(two synthetic directions)=%.2f Mbit/s; independent B=%.2f Mbit/s", rateA/1e6, rateB/1e6)
	// Exercise a real older verified plan after a newer generation, with full
	// runtime readback and no lowering of the persisted high-water mark.
	cfg.Generation = 2
	cfg.Rules[0].BandwidthMbps = 6
	if _, e = a.Apply(ctx, cfg); e != nil {
		t.Fatal(e)
	}
	if _, e = a.Restore(ctx, p); e != nil {
		t.Fatal("real controlled restore", e)
	}
	state, e := store.Load(ctx)
	if e != nil || state.HighWater != 2 || state.Pending {
		t.Fatal("restore high-water", e, state)
	}
	// An identically numbered HTB root on another interface is still foreign
	// when this lease has never installed/verified it.
	execute("ip", "link", "add", "rd-foreign", "type", "dummy")
	execute("ip", "link", "set", "dev", "rd-foreign", "alias", o.Lease.Alias())
	execute("ip", "link", "set", "dev", "rd-foreign", "up")
	execute("tc", "qdisc", "add", "dev", "rd-foreign", "root", "handle", RootHandle, "htb", "default", "ffff")
	foreignBefore := execute("tc", "-j", "qdisc", "show", "dev", "rd-foreign")
	links, e = array(execute("ip", "-j", "link", "show", "dev", "rd-foreign"))
	if e != nil || len(links) != 1 {
		t.Fatal(e)
	}
	index, e = number(links[0]["ifindex"])
	if e != nil {
		t.Fatal(e)
	}
	foreign := o
	foreign.Lease.Device = "rd-foreign"
	foreign.Lease.IfIndex = int(index)
	foreign.UploadEgress = "rd-foreign"
	foreign.DownloadEgress = "rd-foreign"
	fs, e := NewFileStore(filepath.Join(t.TempDir(), "foreign-state"))
	if e != nil {
		t.Fatal(e)
	}
	fa, e := NewAdapter(foreign, b, fs, fs.Locker())
	if e != nil {
		t.Fatal(e)
	}
	if _, e = fa.Apply(ctx, config()); e == nil {
		t.Fatal("adopted external root")
	}
	if !bytes.Equal(foreignBefore, execute("tc", "-j", "qdisc", "show", "dev", "rd-foreign")) {
		t.Fatal("external root changed")
	}
	t.Log("verified Linux JSON parsing, HTB/fw masked classification, combined per-rule bucket, independent ceilings, idempotent Ensure, controlled Restore and foreign-root preservation in network-none container; actual bidirectional relay topology remains unverified")
}

const dummyTraffic = `import socket,time
sockets={port:socket.socket(socket.AF_INET,socket.SOCK_DGRAM) for port in (30001,30002,30003)}
for s in sockets.values(): s.setblocking(False)
payload=b'x'*1200
end=time.monotonic()+4
while time.monotonic()<end:
 for port in (30001,30002,30003):
  try: sockets[port].sendto(payload,('198.18.0.2',port))
  except BlockingIOError: pass
 time.sleep(0.0001)
for s in sockets.values(): s.close()
`
