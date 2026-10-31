package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"reladock/internal/agent"
	"reladock/internal/buildinfo"
	"reladock/internal/platform"
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
	state, applier, err := agent.PrepareApply(agent.RuntimeOptions{Apply: *apply, StateDir: *stateDir, SNATAddress: *snat, ShapingInterface: *shapingInterface, WALDir: *walDir, NFTBinary: *nftBinary, TCBinary: *tcBinary, IPBinary: *ipBinary, ShapingLeaseFile: *shapingLease, EthtoolBinary: *ethtoolBinary, ControlLockPath: "/var/lib/reladock-locks/control.lock"}, *nodeID)
	if err != nil {
		log.Fatal(err)
	}
	engine, _ := applier.(*agent.NodeEngine)
	if engine != nil {
		defer engine.Close()
	}
	credentials, err := agent.OpenCredentialManager(*dir, *nodeID)
	if err != nil {
		log.Fatal("private node credentials unavailable")
	}
	config, err := credentials.TLSConfig()
	if err != nil {
		log.Fatal("client TLS configuration unavailable")
	}
	transport := &http.Transport{TLSClientConfig: config}
	credentials.RegisterTransport(transport)
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 20 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	runner := agent.Client{HTTP: client, BaseURL: *endpoint, NodeID: *nodeID, State: state, Applier: applier}
	kernelBootID := ""
	var retirementState *agent.RetirementState
	if engine != nil {
		var stateErr error
		retirementState, stateErr = agent.NewRetirementState(*stateDir)
		if stateErr != nil {
			log.Fatal(stateErr)
		}
		bootID, bootErr := agent.ReadKernelBootID("/proc/sys/kernel/random/boot_id")
		if bootErr != nil {
			log.Fatal("kernel boot identity unavailable")
		}
		kernelBootID = bootID
		runner.Retirement = &agent.RetirementWorker{State: retirementState, Adapter: engine.Adapter, BootID: bootID}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *apply {
		log.Print("Agent mode: explicit application; application ACK does not establish forwarding health")
	} else {
		log.Print("Agent mode: validation-only; no nftables/tc execution and no applied ACK")
	}
	if *once {
		if engine != nil {
			tickCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			if err := engine.Tick(tickCtx, &runner); err != nil {
				log.Printf("metering cycle needs attention: %v", err)
			}
			cancel()
		}
		done, err := runner.Once(ctx)
		if err != nil {
			log.Fatal(err)
		} else if done {
			if *apply {
				log.Print("Configuration application report acknowledged; forwarding health remains unknown")
			} else {
				log.Print("Desired configuration validated and acknowledged; traffic not enabled")
			}
		}
		return
	}
	startedAt := time.Now().UTC().Truncate(time.Microsecond)
	mode := "validation"
	if *apply {
		mode = "apply"
	}
	upgradeHealth := agent.NewUpgradeHealth("/run/reladock-updater/health-request.json", "/run/reladock-agent", buildinfo.Current())
	resourceCollector := agent.NewResourceCollector()
	hooks := agent.RuntimeHooks{Preflight: func(parent context.Context) error {
		// Contact and inspection are distinct from configuration/application health.
		activityCtx, cancel := context.WithTimeout(parent, 20*time.Second)
		defer cancel()
		return runner.ReportPreflight(activityCtx)
	}, Configuration: func(parent context.Context) error {
		done, err := runner.Once(parent)
		if err == nil {
			err = upgradeHealth.ConfigurationSucceeded()
		}
		if err == nil && done {
			if *apply {
				log.Print("Configuration application report acknowledged; forwarding health remains unknown")
			} else {
				log.Print("Desired configuration validated and acknowledged; traffic not enabled")
			}
		}
		if err == nil && !done && engine != nil {
			if sampleErr := engine.Sample(parent); sampleErr == nil {
				_ = engine.FlushUsage(parent, &runner)
				draining, statusErr := runner.DrainRequested(parent)
				if statusErr != nil {
					err = statusErr
				} else if draining {
					current := credentials.Current()
					if attestation, evidenceErr := engine.BuildDrainAttestation(parent, retirementState, current.Fingerprint, kernelBootID, startedAt, agent.UpdaterStable("/var/lib/reladock-updater/state.json")); evidenceErr == nil {
						err = runner.ReportDrainAttestation(parent, attestation)
					}
				}
			}
		}
		return err
	}, Heartbeat: func(parent context.Context) error {
		activityCtx, cancel := context.WithTimeout(parent, 20*time.Second)
		defer cancel()
		current := credentials.Current()
		journalSequence := int64(0)
		if retirementState != nil {
			var sequenceErr error
			journalSequence, sequenceErr = retirementState.CurrentSequence()
			if sequenceErr != nil {
				return sequenceErr
			}
		}
		rotationState, rotationError := credentials.RotationStatus()
		if err := runner.ReportRuntimeHeartbeat(activityCtx, platform.AgentHeartbeat{Version: buildinfo.Current(), Mode: mode, StartedAt: startedAt, CertificateNotAfter: current.NotAfter, RotationState: rotationState, RotationErrorCode: rotationError, KernelBootID: kernelBootID, JournalSequence: journalSequence}, resourceCollector, engine); err != nil {
			return err
		}
		return upgradeHealth.HeartbeatSucceeded()
	}, Renewal: func(parent context.Context) error {
		activityCtx, cancel := context.WithTimeout(parent, 20*time.Second)
		defer cancel()
		return credentials.ReconcileRotation(activityCtx, &runner, time.Now().UTC())
	}}
	if engine != nil {
		hooks.MeterSample = func(parent context.Context) error {
			activityCtx, cancel := context.WithTimeout(parent, 10*time.Second)
			defer cancel()
			return engine.Sample(activityCtx)
		}
		hooks.UsageUpload = func(parent context.Context) error {
			activityCtx, cancel := context.WithTimeout(parent, 20*time.Second)
			defer cancel()
			return engine.FlushUsage(activityCtx, &runner)
		}
		hooks.Probes = func(parent context.Context) error {
			activityCtx, cancel := context.WithTimeout(parent, 20*time.Second)
			defer cancel()
			return engine.ProbeCycle(activityCtx, &runner)
		}
	}
	activities, err := agent.RuntimeActivities(hooks)
	if err != nil {
		log.Fatal(err)
	}
	if err := agent.RunSupervisor(ctx, activities, func(name string, err error) {
		if name == "preflight" {
			log.Print("Read-only environment report needs attention")
			return
		}
		log.Printf("%s cycle needs attention: %v", name, err)
	}); err != nil {
		log.Fatal(err)
	}
}
