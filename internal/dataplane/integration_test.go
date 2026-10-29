package dataplane

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"reladock/internal/metering"
	"reladock/internal/platform"
	"reladock/internal/shaping"
	"strings"
	"testing"
	"time"
)

// This is an explicit opt-in harness, never a host nft runner. The operator
// supplies an existing local image and a pre-authorized internal bridge.
// It neither pulls images nor creates/deletes Docker networks.
func TestDockerIntegration(t *testing.T) {
	if os.Getenv("RELADOCK_DATAPLANE_INTEGRATION") != "1" {
		t.Skip("Docker integration not verified: set RELADOCK_DATAPLANE_INTEGRATION=1 with a local image and dedicated network")
	}
	image, network := os.Getenv("RELADOCK_DATAPLANE_IMAGE"), os.Getenv("RELADOCK_DATAPLANE_NETWORK")
	if image == "" || !strings.HasPrefix(network, "reladock-test-") {
		t.Fatal("explicit local image and reladock-test-* network are required; no pulls or network provisioning")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	dockerPath, e := exec.LookPath("docker")
	if e != nil {
		t.Fatal("Docker unavailable; integration NOT verified:", e)
	}
	docker := func(c context.Context, input string, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(c, dockerPath, args...)
		cmd.Stdin = strings.NewReader(input)
		var out, stderr bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &stderr
		if e := cmd.Run(); e != nil {
			return out.Bytes(), fmt.Errorf("docker %v: %w: %s", args, e, stderr.String())
		}
		return out.Bytes(), nil
	}
	must := func(input string, args ...string) []byte {
		t.Helper()
		out, e := docker(ctx, input, args...)
		if e != nil {
			t.Fatal(e)
		}
		return out
	}
	must("", "image", "inspect", image)
	var networks []struct {
		Name, Driver string
		Internal     bool
		Labels       map[string]string
	}
	if e = json.Unmarshal(must("", "network", "inspect", network), &networks); e != nil || len(networks) != 1 || networks[0].Name != network || networks[0].Driver != "bridge" || !networks[0].Internal || networks[0].Labels["reladock-test"] != "true" {
		t.Fatal("requires pre-existing internal bridge labeled reladock-test=true", e)
	}
	var seed [8]byte
	if _, e = rand.Read(seed[:]); e != nil {
		t.Fatal(e)
	}
	prefix := "reladock-test-" + hex.EncodeToString(seed[:])
	names := []string{prefix + "-relay", prefix + "-target", prefix + "-client", prefix + "-initial-relay", prefix + "-composed-relay"}
	for i, name := range names {
		// Register cleanup before launch, including uncertain CLI outcomes. Only
		// exact cryptographically unique names from this invocation are removed.
		t.Cleanup(func() {
			cleanup, c := context.WithTimeout(context.Background(), 15*time.Second)
			defer c()
			if _, e := docker(cleanup, "", "rm", "-f", name); e != nil {
				t.Logf("container cleanup %s: %v", name, e)
			}
		})
		args := []string{"run", "-d", "--pull=never", "--name", name, "--label", "reladock-test=true", "--network", network, "--user", "0:0", "--cap-drop=ALL", "--security-opt", "no-new-privileges=true"}
		if i == 0 || i == 3 || i == 4 {
			args = append(args, "--cap-add=NET_ADMIN", "--sysctl", "net.ipv4.ip_forward=1")
		}
		args = append(args, "--entrypoint", "sleep", image, "infinity")
		must("", args...)
	}
	// The test never mounts the Docker socket, host paths, or host namespaces.
	address := func(name string) string {
		var items []struct {
			NetworkSettings struct {
				Networks map[string]struct{ IPAddress string }
			}
		}
		if e := json.Unmarshal(must("", "inspect", name), &items); e != nil || len(items) != 1 {
			t.Fatal("container inspect", e)
		}
		ip := items[0].NetworkSettings.Networks[network].IPAddress
		if _, e := ipv4(ip); e != nil {
			t.Fatal("IPv4 Docker address required", e)
		}
		return ip
	}
	relay, target := address(names[0]), address(names[1])
	r := &containerRunner{docker: docker, name: names[0]}
	must("", "exec", names[0], "nft", "--version")
	must("", "exec", names[1], "python3", "--version")
	must("", "exec", names[2], "python3", "--version")
	// Echo server startup is readiness-checked with a direct client, not sleep.
	must("", "exec", "-d", names[1], "python3", "-u", "-c", echoServer)
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, e = docker(ctx, "", "exec", names[2], "python3", "-c", echoClient, target, "9000", "tcp", "ready")
		if e == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("echo startup:", e)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	cfg := fixture()
	cfg.ListenAddress = relay
	cfg.Rules[0].TargetHost = target
	cfg.Rules[0].TargetPort = 9000
	opts := Options{NodeID: cfg.NodeID, SNATAddress: relay}
	private := filepath.Join(t.TempDir(), "state")
	store, e := NewFileStore(private)
	if e != nil {
		t.Fatal(e)
	}
	a, e := NewAdapter(opts, r, store, store.Locker(), ReadbackVerifier(r))
	if e != nil {
		t.Fatal(e)
	}
	// Foreign DNAT deliberately uses 20001, inside the platform's numeric
	// allocation range but outside any allocation in this snapshot.
	foreign := fmt.Sprintf("table ip reladock_foreign {\n comment \"foreign-preserve\"\n chain pre { type nat hook prerouting priority -110; policy accept; ip daddr %s tcp dport 20001 dnat to %s:9000; }\n chain post { type nat hook postrouting priority 110; policy accept; ct original ip daddr %s ct original proto-dst 20001 ip daddr %s tcp dport 9000 snat to %s; }\n}\n", relay, target, relay, target, relay)
	must(foreign, "exec", "-i", names[0], "nft", "-f", "-")
	foreignBefore := must("", "exec", names[0], "nft", "-j", "list", "table", "ip", "reladock_foreign")
	p, e := a.Apply(ctx, cfg)
	if e != nil {
		t.Fatal("real nft check/apply/readback:", e)
	}
	if r.checks < 1 || r.applies < 1 {
		t.Fatal("actual check/apply not observed")
	}
	echo := func(port, proto string) {
		t.Helper()
		must("", "exec", names[2], "python3", "-c", echoClient, relay, port, proto, "relay-payload")
	}
	echo("20000", "tcp")
	echo("20000", "udp")
	echo("20001", "tcp")
	raw := must("", "exec", names[0], "nft", "-j", "list", "table", "ip", Table)
	var doc struct {
		NFTables []struct {
			Counter *struct {
				Name           string
				Packets, Bytes uint64
			} `json:"counter"`
		} `json:"nftables"`
	}
	if e = json.Unmarshal(raw, &doc); e != nil {
		t.Fatal(e)
	}
	counts := map[string]uint64{}
	for _, v := range doc.NFTables {
		if v.Counter != nil {
			counts[v.Counter.Name] = v.Counter.Packets
		}
	}
	for _, c := range p.Counters {
		if counts[c.Name] < 5 {
			t.Fatalf("full forward counter %s only saw %d packets; want multiple packets of same connection", c.Name, counts[c.Name])
		}
	}
	invalid := "add table ip reladock_invalid\nthis is deliberately invalid\n"
	if _, e = r.Run(ctx, []string{"-c", "-f", "-"}, invalid); e == nil {
		t.Fatal("invalid nft check accepted")
	}
	if _, e = r.Run(ctx, []string{"-f", "-"}, invalid); e == nil {
		t.Fatal("invalid nft transaction accepted")
	}
	// Establish a TCP flow on a newly introduced candidate port, and keep the
	// very same socket alive across verification failure and rollback. Replaying
	// the old script without its new allocation guard would let this flow echo.
	var candidateFlow *heldTCPFlow
	defer func() {
		if candidateFlow != nil {
			candidateFlow.close()
		}
	}()
	candidateWasEchoing := false
	a.verify = func(c context.Context, p Plan) error {
		if e := ReadbackVerifier(r)(c, p); e != nil {
			return e
		}
		if p.Generation == 2 {
			var e error
			candidateFlow, e = startHeldTCPFlow(ctx, dockerPath, names[2], relay, "20002")
			if e != nil {
				return fmt.Errorf("candidate connection: %w", e)
			}
			if e = candidateFlow.expect("ECHO"); e != nil {
				return e
			}
			candidateWasEchoing = true
			return errors.New("injected post-apply runtime failure")
		}
		return nil
	}
	cfg.Generation = 2
	newRule := cfg.Rules[0]
	newRule.ID = "candidate-rule"
	newRule.ListenPort = 20002
	newRule.Protocol = "tcp"
	cfg.Rules = append(cfg.Rules, newRule)
	if _, e = a.Apply(ctx, cfg); e == nil {
		t.Fatal("runtime failure not surfaced")
	}
	if candidateFlow == nil || !candidateWasEchoing {
		t.Fatal("regression did not establish the failed candidate's new-port flow")
	}
	if e = candidateFlow.expect("BLOCKED"); e != nil {
		t.Fatal("failed candidate existing flow survived rollback:", e)
	}
	assertNewBlocked := func(ip, port string) {
		t.Helper()
		if got := strings.TrimSpace(string(must("", "exec", names[2], "python3", "-c", newTCPMustBlock, ip, port))); got != "BLOCKED" {
			t.Fatal("new candidate connection not blocked:", got)
		}
	}
	assertNewBlocked(relay, "20002")
	state, e := store.Load(ctx)
	if e != nil || state.Pending || state.HighWater != 2 || state.Good == nil || state.Good.Generation != 1 {
		t.Fatalf("rollback state %+v %v", state, e)
	}
	if e = ReadbackVerifier(r)(ctx, *state.Good); e != nil {
		t.Fatal("rollback readback", e)
	}
	echo("20000", "tcp")
	echo("20000", "udp")
	echo("20001", "tcp")
	if !bytes.Equal(foreignBefore, must("", "exec", names[0], "nft", "-j", "list", "table", "ip", "reladock_foreign")) {
		t.Fatal("foreign table changed")
	}
	if _, e = a.Apply(ctx, cfg); !errors.Is(e, ErrStale) {
		t.Fatalf("stale replay: %v", e)
	}
	// Restart from disk, apply a newer snapshot without the failed allocation,
	// and ensure neither that operation nor restart removes its exact guard.
	a, e = NewAdapter(opts, r, store, store.Locker(), ReadbackVerifier(r))
	if e != nil {
		t.Fatal(e)
	}
	cfg.Generation = 3
	cfg.Rules = cfg.Rules[:1]
	if _, e = a.Apply(ctx, cfg); e != nil {
		t.Fatal("post-rollback restart:", e)
	}
	if e = candidateFlow.expect("BLOCKED"); e != nil {
		t.Fatal("restart removed existing-flow fence:", e)
	}
	assertNewBlocked(relay, "20002")
	echo("20000", "tcp")
	echo("20000", "udp")
	echo("20001", "tcp")

	// An initial apply has no old business config. It must leave a guard-only
	// table, not delete the table while the candidate's conntrack still exists.
	initialIP := address(names[3])
	initialRunner := &containerRunner{docker: docker, name: names[3]}
	initialStore, e := NewFileStore(filepath.Join(t.TempDir(), "initial-state"))
	if e != nil {
		t.Fatal(e)
	}
	initialOptions := Options{NodeID: cfg.NodeID, SNATAddress: initialIP}
	var initialFlow *heldTCPFlow
	defer func() {
		if initialFlow != nil {
			initialFlow.close()
		}
	}()
	initialWasEchoing := false
	initialVerify := func(c context.Context, p Plan) error {
		if e := ReadbackVerifier(initialRunner)(c, p); e != nil {
			return e
		}
		if len(p.Counters) > 0 {
			var e error
			initialFlow, e = startHeldTCPFlow(ctx, dockerPath, names[2], initialIP, "20004")
			if e != nil {
				return e
			}
			if e = initialFlow.expect("ECHO"); e != nil {
				return e
			}
			initialWasEchoing = true
			return errors.New("initial candidate verification failed")
		}
		return nil
	}
	initialAdapter, e := NewAdapter(initialOptions, initialRunner, initialStore, initialStore.Locker(), initialVerify)
	if e != nil {
		t.Fatal(e)
	}
	wal, e := metering.Open(filepath.Join(t.TempDir(), "usage-wal"), metering.Options{NodeID: cfg.NodeID})
	if e != nil {
		t.Fatal(e)
	}
	defer wal.Close()
	initialMeter := MeterHooks{WAL: wal, Runner: initialRunner}
	initialAdapter.SetTransitionHooks(initialMeter)
	initialConfig := fixture()
	initialConfig.Rules[0].ID = "00000000-0000-4000-8000-000000000001"
	initialConfig.Rules[0].SubscriptionID = "00000000-0000-4000-8000-000000000002"
	initialConfig.ListenAddress = initialIP
	initialConfig.Rules[0].TargetHost = target
	initialConfig.Rules[0].TargetPort = 9000
	initialConfig.Rules[0].ListenPort = 20004
	initialConfig.Rules[0].Protocol = "tcp"
	if _, e = initialAdapter.Apply(ctx, initialConfig); e == nil {
		t.Fatal("initial candidate failure lost")
	}
	if initialFlow == nil || !initialWasEchoing {
		t.Fatal("initial candidate flow was not established")
	}
	if e = initialFlow.expect("BLOCKED"); e != nil {
		t.Fatal("initial rollback leaked existing candidate flow:", e)
	}
	assertNewBlocked(initialIP, "20004")
	initialState, e := initialStore.Load(ctx)
	if e != nil || initialState.Pending || initialState.Good == nil || len(initialState.Good.Counters) != 0 {
		t.Fatalf("initial guard-only state: %+v %v", initialState, e)
	}
	if e = ReadbackVerifier(initialRunner)(ctx, *initialState.Good); e != nil {
		t.Fatal("initial guard-only readback:", e)
	}
	initialAdapter, e = NewAdapter(initialOptions, initialRunner, initialStore, initialStore.Locker(), ReadbackVerifier(initialRunner))
	if e != nil {
		t.Fatal(e)
	}
	initialAdapter.SetTransitionHooks(initialMeter)
	initialConfig.Generation = 2
	initialConfig.Rules[0].ListenPort = 20006
	if _, e = initialAdapter.Apply(ctx, initialConfig); e != nil {
		t.Fatal("restart after initial failure:", e)
	}
	must("", "exec", names[2], "python3", "-c", echoClient, initialIP, "20006", "tcp", "metered-real-traffic")
	if e = initialAdapter.Observe(ctx, initialMeter.Sample); e != nil {
		t.Fatal("real counter sample:", e)
	}
	initialConfig.Generation = 3
	initialConfig.Rules[0].Status = "paused"
	if _, e = initialAdapter.Apply(ctx, initialConfig); e != nil {
		t.Fatal("freeze/final-sample/replace:", e)
	}
	var billed int64
	if e = wal.Flush(ctx, func(_ context.Context, p []byte) (metering.Receipt, error) {
		var u metering.Report
		if err := json.Unmarshal(p, &u); err != nil {
			return metering.Receipt{}, err
		}
		if u.RuleID != initialConfig.Rules[0].ID || u.SubscriptionID != initialConfig.Rules[0].SubscriptionID {
			return metering.Receipt{}, errors.New("counter attributed to another rule or period")
		}
		billed += u.UploadBytes + u.DownloadBytes
		return metering.Receipt{RuleID: u.RuleID, SubscriptionID: u.SubscriptionID, Epoch: u.Epoch, Sequence: u.Sequence}, nil
	}); e != nil {
		t.Fatal(e)
	}
	if billed <= 0 {
		t.Fatal("real forward packets never reached durable WAL")
	}
	// The kernel owns the expiry, not an Agent timer. Keep a connection alive
	// while no Agent loop runs, then verify that its subsequent packets stop.
	initialConfig.Generation = 4
	initialConfig.Rules[0].Status = "pending-agent"
	expires := time.Now().Add(ExpirySafetyMargin + 4*time.Second)
	initialConfig.Rules[0].ExpiresAt = &expires
	if _, e = initialAdapter.Apply(ctx, initialConfig); e != nil {
		t.Fatal("kernel entitlement timeout:", e)
	}
	expiringFlow, e := startHeldTCPFlow(ctx, dockerPath, names[2], initialIP, "20006")
	if e != nil {
		t.Fatal(e)
	}
	defer expiringFlow.close()
	if e = expiringFlow.expect("ECHO"); e != nil {
		t.Fatal("pre-expiry flow:", e)
	}
	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-time.After(5 * time.Second):
	}
	if e = expiringFlow.expect("BLOCKED"); e != nil {
		t.Fatal("expired entitlement kept forwarding with no Agent:", e)
	}
	// Compose the actual same-egress tc tree, nft marks, durable metering and
	// per-rule offline quota. All privileged work remains inside this container.
	composedIP := address(names[4])
	must("", "exec", names[4], "ip", "link", "set", "dev", "eth0", "alias", "reladock-shaping-v1:hk-01")
	var links []struct {
		IfIndex int `json:"ifindex"`
	}
	if e = json.Unmarshal(must("", "exec", names[4], "ip", "-j", "link", "show", "dev", "eth0"), &links); e != nil || len(links) != 1 {
		t.Fatal("owned egress interface:", e)
	}
	ns := strings.TrimSpace(string(must("", "exec", names[4], "readlink", "/proc/self/ns/net")))
	shapeOptions := shaping.Options{NodeID: "hk-01", Lease: shaping.Lease{Device: "eth0", IfIndex: links[0].IfIndex, NamespaceID: ns, OwnerID: "hk-01", Dedicated: true, MarkNamespace: 82, LinkMbps: 1000}, UploadEgress: "eth0", DownloadEgress: "eth0"}
	shapeBackend := shaping.NewSystemBackend(containerShapeRunner{docker: docker, name: names[4]}, func(context.Context) (string, error) { return ns, nil })
	shapeStore, e := shaping.NewFileStore(filepath.Join(t.TempDir(), "shape"))
	if e != nil {
		t.Fatal(e)
	}
	shapeAdapter, e := shaping.NewAdapter(shapeOptions, shapeBackend, shapeStore, shapeStore.Locker())
	if e != nil {
		t.Fatal(e)
	}
	composedRunner := &containerRunner{docker: docker, name: names[4]}
	composedStore, e := NewFileStore(filepath.Join(t.TempDir(), "composed"))
	if e != nil {
		t.Fatal(e)
	}
	composedWAL, e := metering.Open(filepath.Join(t.TempDir(), "composed-wal"), metering.Options{NodeID: "hk-01"})
	if e != nil {
		t.Fatal(e)
	}
	defer composedWAL.Close()
	composedAdapter, e := NewAdapter(Options{NodeID: "hk-01", SNATAddress: composedIP, Shaping: &shapeOptions}, composedRunner, composedStore, composedStore.Locker(), ComposedVerifier(composedRunner, shapeAdapter))
	if e != nil {
		t.Fatal(e)
	}
	composedMeter := MeterHooks{WAL: composedWAL, Runner: composedRunner}
	composedAdapter.SetTransitionHooks(ShapedMeterHooks{MeterHooks: composedMeter, Shaper: shapeAdapter})
	composedCfg := initialConfig
	composedCfg.Generation = 1
	composedCfg.ListenAddress = composedIP
	composedCfg.Rules = append([]platform.ConfigRule(nil), initialConfig.Rules...)
	composedCfg.Rules[0].ExpiresAt = nil
	composedCfg.Rules[0].QuotaBytes = 12000
	composedCfg.Rules[0].BandwidthMbps = 2
	if _, e = composedAdapter.Apply(ctx, composedCfg); e != nil {
		t.Fatal("real composed apply:", e)
	}
	must("", "exec", names[2], "python3", "-c", echoClient, composedIP, "20006", "tcp", "composed-quota")
	if e = composedAdapter.Observe(ctx, composedMeter.Sample); e != nil {
		t.Fatal(e)
	}
	beforeUp, beforeDown, e := composedWAL.CumulativeFor(ctx, composedCfg.Rules[0].ID, composedCfg.Rules[0].SubscriptionID)
	if e != nil || beforeUp+beforeDown == 0 {
		t.Fatal("unmetered composed path", e)
	}
	composedCfg.Generation = 2
	p, e = composedAdapter.Apply(ctx, composedCfg)
	if e != nil {
		t.Fatal("composed replace/quota:", e)
	}
	if p.QuotaUsed[composedCfg.Rules[0].ID] < beforeUp+beforeDown {
		t.Fatal("quota reset across replacement")
	}
	must("", "exec", names[4], "tc", "qdisc", "del", "dev", "eth0", "root")
	if e = composedAdapter.Observe(ctx, composedMeter.Sample); e == nil {
		t.Fatal("missing limiter was not detected")
	}
	assertNewBlocked(composedIP, "20006")
	composedCfg.Generation = 3
	if _, e = composedAdapter.Apply(ctx, composedCfg); e != nil {
		t.Fatal("owned limiter reassertion while frozen:", e)
	}
	// No control connection or Agent loop replenishes this quota. Repeated
	// uploads must exhaust the one shared rule quota and block a new TCP flow.
	must("", "exec", names[2], "python3", "-c", exhaustRuleQuota, composedIP, "20006")
	assertNewBlocked(composedIP, "20006")
	if e = composedAdapter.Observe(ctx, composedMeter.Sample); e != nil {
		t.Fatal(e)
	}
	replacementWAL, e := metering.Open(filepath.Join(t.TempDir(), "wrong-empty-wal"), metering.Options{NodeID: "hk-01"})
	if e != nil {
		t.Fatal(e)
	}
	defer replacementWAL.Close()
	replacementMeter := MeterHooks{WAL: replacementWAL, Runner: composedRunner}
	composedAdapter.SetTransitionHooks(ShapedMeterHooks{MeterHooks: replacementMeter, Shaper: shapeAdapter})
	if e = composedAdapter.Observe(ctx, replacementMeter.Sample); !errors.Is(e, ErrMeterIdentity) {
		t.Fatalf("different WAL accepted physical counters: %v", e)
	}
	if _, ok, e := replacementWAL.Next(); e != nil || ok {
		t.Fatal("replacement WAL rebilled existing traffic", e)
	}
	if e = initialFlow.expect("BLOCKED"); e != nil {
		t.Fatal("restart lost initial candidate fence:", e)
	}
	assertNewBlocked(initialIP, "20004")
	t.Log("verified real container nft/counters/foreign preservation and rollback with live candidate-new-port TCP flows fenced across rollback and restart (existing Good and initial apply); no production readiness claim")
}

type containerShapeRunner struct {
	docker func(context.Context, string, ...string) ([]byte, error)
	name   string
}

func (r containerShapeRunner) Run(ctx context.Context, tool shaping.Tool, args []string) ([]byte, error) {
	return r.docker(ctx, "", append([]string{"exec", r.name, string(tool)}, args...)...)
}

const exhaustRuleQuota = `import socket,sys
s=socket.create_connection((sys.argv[1],int(sys.argv[2])),timeout=2)
s.settimeout(.5)
try:
 for i in range(256):
  s.sendall(b'x'*1024)
  if not s.recv(1024): break
except (socket.timeout,ConnectionResetError,BrokenPipeError): pass
s.close()
`

// heldTCPFlow owns one real client socket in the client container. Probe lines
// go to that same Python process; no reconnect can mask a conntrack leak.
type heldTCPFlow struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *bufio.Reader
	stderr bytes.Buffer
}

func startHeldTCPFlow(ctx context.Context, dockerPath, container, ip, port string) (*heldTCPFlow, error) {
	f := &heldTCPFlow{cmd: exec.CommandContext(ctx, dockerPath, "exec", "-i", container, "python3", "-u", "-c", heldTCPClient, ip, port)}
	var e error
	f.in, e = f.cmd.StdinPipe()
	if e != nil {
		return nil, e
	}
	out, e := f.cmd.StdoutPipe()
	if e != nil {
		f.in.Close()
		return nil, e
	}
	f.out = bufio.NewReader(out)
	f.cmd.Stderr = &f.stderr
	if e = f.cmd.Start(); e != nil {
		f.in.Close()
		return nil, e
	}
	return f, nil
}
func (f *heldTCPFlow) expect(want string) error {
	if _, e := fmt.Fprintln(f.in, "probe"); e != nil {
		return e
	}
	line, e := f.out.ReadString('\n')
	if e != nil {
		return fmt.Errorf("live socket probe: %w", e)
	}
	if strings.TrimSpace(line) != want {
		return fmt.Errorf("live socket got %q; want %s", line, want)
	}
	return nil
}
func (f *heldTCPFlow) close() { _ = f.in.Close(); _ = f.cmd.Wait() }

const heldTCPClient = `import socket,sys
s=socket.socket(); s.settimeout(1); s.connect((sys.argv[1],int(sys.argv[2])))
for line in sys.stdin:
 try:
  s.sendall(b'candidate-connection-probe')
  got=b''
  while len(got)<len(b'candidate-connection-probe'):
   part=s.recv(4096)
   if not part: break
   got+=part
  print('ECHO' if got==b'candidate-connection-probe' else ('BLOCKED' if not got else 'INVALID'),flush=True)
 except (socket.timeout,ConnectionResetError,BrokenPipeError): print('BLOCKED',flush=True)
s.close()
`
const newTCPMustBlock = `import socket,sys
s=socket.socket(); s.settimeout(1)
try:
 s.connect((sys.argv[1],int(sys.argv[2]))); s.sendall(b'new-connection-probe')
 got=s.recv(4096)
 if got: raise RuntimeError('failed candidate port still echoes')
 print('BLOCKED')
except (socket.timeout,ConnectionRefusedError,ConnectionResetError,BrokenPipeError): print('BLOCKED')
finally: s.close()
`

type containerRunner struct {
	docker          func(context.Context, string, ...string) ([]byte, error)
	name            string
	checks, applies int
}

func (r *containerRunner) Run(ctx context.Context, args []string, input string) ([]byte, error) {
	if reflect.DeepEqual(args, []string{"-c", "-f", "-"}) {
		r.checks++
	}
	if reflect.DeepEqual(args, []string{"-f", "-"}) {
		r.applies++
	}
	return r.docker(ctx, input, append([]string{"exec", "-i", r.name, "nft"}, args...)...)
}

const echoServer = `import socket, threading
def tcp():
 s=socket.socket(); s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1); s.bind(('0.0.0.0',9000)); s.listen()
 while True:
  c,_=s.accept()
  def serve(c):
   with c:
    while True:
     data=c.recv(65536)
     if not data: break
     c.sendall(data)
  threading.Thread(target=serve,args=(c,),daemon=True).start()
threading.Thread(target=tcp,daemon=True).start()
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); s.bind(('0.0.0.0',9000))
while True:
 data,addr=s.recvfrom(65536); s.sendto(data,addr)
`
const echoClient = `import socket,sys
host,port,proto,payload=sys.argv[1:]; data=(payload*32).encode()
s=socket.socket(socket.AF_INET,socket.SOCK_STREAM if proto=='tcp' else socket.SOCK_DGRAM); s.settimeout(2); s.connect((host,int(port)))
for _ in range(8):
 s.sendall(data)
 out=b''
 while len(out)<len(data): out+=s.recv(65536)
 assert out==data,(out,data)
s.close()
`
