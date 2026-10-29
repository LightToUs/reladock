package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"reladock/internal/agent"
	"reladock/internal/nodeauth"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "enroll" {
		flags := flag.NewFlagSet("enroll", flag.ExitOnError)
		origin := flags.String("bootstrap-origin", "", "trusted control HTTPS origin")
		pki := flags.String("pki", "/etc/reladock/node-pki", "private PKI directory")
		env := flags.String("env-file", "", "new private environment file")
		flags.Parse(os.Args[2:])
		if flags.NArg() != 0 {
			log.Fatal("unexpected arguments")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := agent.Bootstrap(ctx, agent.BootstrapOptions{Origin: *origin, PKIDir: *pki, EnvFile: *env, Token: os.Stdin}); err != nil {
			log.Fatal(err)
		}
		log.Print("Node enrollment saved; forwarding remains disabled")
		return
	}
	endpoint := flag.String("control", "https://127.0.0.1:3002", "control HTTPS origin")
	nodeID := flag.String("node", "hk-01", "enrolled node ID")
	dir := flag.String("pki", ".local/pki", "CA and node certificate directory")
	once := flag.Bool("once", false, "validate at most one configuration and exit")
	apply := flag.Bool("apply", false, "explicit Linux application opt-in (requires connected tc and collector/WAL)")
	stateDir := flag.String("state-dir", "", "private absolute persistent Agent state directory")
	snat := flag.String("snat", "", "explicit IPv4 SNAT address for apply mode")
	shapingInterface := flag.String("shaping-interface", "", "explicit dedicated shaping interface for apply mode")
	walDir := flag.String("metering-wal-dir", "", "explicit absolute collector WAL directory for apply mode")
	nftBinary := flag.String("nft-binary", "/usr/sbin/nft", "trusted absolute nft binary path")
	tcBinary := flag.String("tc-binary", "/usr/sbin/tc", "trusted absolute tc binary path")
	ipBinary := flag.String("ip-binary", "/usr/sbin/ip", "trusted absolute ip binary path")
	shapingLease := flag.String("shaping-lease", "", "private explicit shaping interface lease JSON")
	ethtoolBinary := flag.String("ethtool-binary", "/usr/sbin/ethtool", "trusted absolute ethtool binary for read-only offload inspection")
	flag.Parse()
	if flag.NArg() != 0 {
		log.Fatal("unexpected positional arguments")
	}
	if err := agent.ValidateEndpoint(*endpoint, *nodeID); err != nil {
		log.Fatal(err)
	}
	state, applier, err := agent.PrepareApply(agent.RuntimeOptions{Apply: *apply, StateDir: *stateDir, SNATAddress: *snat, ShapingInterface: *shapingInterface, WALDir: *walDir, NFTBinary: *nftBinary, TCBinary: *tcBinary, IPBinary: *ipBinary, ShapingLeaseFile: *shapingLease, EthtoolBinary: *ethtoolBinary}, *nodeID)
	if err != nil {
		log.Fatal(err)
	}
	engine, _ := applier.(*agent.NodeEngine)
	if engine != nil {
		defer engine.Close()
	}
	config, err := nodeauth.ClientTLS(filepath.Join(*dir, "ca.crt"), filepath.Join(*dir, "agent.crt"), filepath.Join(*dir, "agent.key"))
	if err != nil {
		log.Fatal("client TLS configuration unavailable")
	}
	transport := &http.Transport{TLSClientConfig: config}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 20 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	runner := agent.Client{HTTP: client, BaseURL: *endpoint, NodeID: *nodeID, State: state, Applier: applier}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *apply {
		log.Print("Agent mode: explicit application; application ACK does not establish forwarding health")
	} else {
		log.Print("Agent mode: validation-only; no nftables/tc execution and no applied ACK")
	}
	var lastPreflight time.Time
	for {
		// Contact and inspection are distinct from configuration/application health.
		if !*once && time.Since(lastPreflight) >= time.Minute {
			pctx, done := context.WithTimeout(ctx, 20*time.Second)
			if err := runner.ReportPreflight(pctx); err != nil {
				log.Print("Read-only environment report needs attention")
			}
			done()
			lastPreflight = time.Now()
		}
		if engine != nil {
			tickCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			if err := engine.Tick(tickCtx, &runner); err != nil {
				log.Printf("metering cycle needs attention: %v", err)
			}
			cancel()
		}
		done, err := runner.Once(ctx)
		if err != nil {
			if *once {
				log.Fatal(err)
			}
			log.Printf("configuration cycle failed: %v", err)
		} else if done {
			if *apply {
				log.Print("Configuration application report acknowledged; forwarding health remains unknown")
			} else {
				log.Print("Desired configuration validated and acknowledged; traffic not enabled")
			}
		}
		if *once {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}
