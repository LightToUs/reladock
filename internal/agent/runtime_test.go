package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"reladock/internal/dataplane"
	"reladock/internal/platform"
)

func TestApplyOptionsFailBeforeUnconnectedDependencies(t *testing.T) {
	good := RuntimeOptions{Apply: true, StateDir: "/var/lib/reladock/agent", SNATAddress: "192.0.2.2", ShapingInterface: "eth0", WALDir: "/var/lib/reladock/usage", NFTBinary: "/usr/sbin/nft", TCBinary: "/usr/sbin/tc", IPBinary: "/usr/sbin/ip", ShapingLeaseFile: "/etc/reladock/shaping.json", EthtoolBinary: "/usr/sbin/ethtool"}
	for _, name := range []string{"linux-required", "state-dir", "snat", "interface", "wal", "nft", "lease", "tc", "ip"} {
		t.Run(name, func(t *testing.T) {
			o := good
			osname := "linux"
			switch name {
			case "linux-required":
				osname = "darwin"
			case "state-dir":
				o.StateDir = ""
			case "snat":
				o.SNATAddress = "0.0.0.0"
			case "interface":
				o.ShapingInterface = ""
			case "wal":
				o.WALDir = "relative"
			case "nft":
				o.NFTBinary = "nft"
			case "lease":
				o.ShapingLeaseFile = ""
			case "tc":
				o.TCBinary = "tc"
			case "ip":
				o.IPBinary = "ip"
			}
			if e := validateRuntimeOptions(o, osname); e == nil {
				t.Fatalf("accepted unsafe %s", name)
			}
		})
	}
	if e := validateRuntimeOptions(good, "linux"); e != nil {
		t.Fatal(e)
	}
	if e := validateRuntimeOptions(RuntimeOptions{}, "darwin"); e != nil {
		t.Fatalf("default validation mode gated: %v", e)
	}
}

type verifiedAdapterFunc func(context.Context, platform.NodeConfig) (dataplane.Plan, error)

func (f verifiedAdapterFunc) ApplyVerified(ctx context.Context, cfg platform.NodeConfig) (dataplane.Plan, error) {
	return f(ctx, cfg)
}
func TestVerifiedApplierUsesExactVerification(t *testing.T) {
	cfg := platform.NodeConfig{NodeID: "hk-01", Generation: 5}
	sentinel := errors.New("verification failed")
	bridge := VerifiedApplier{Adapter: verifiedAdapterFunc(func(_ context.Context, got platform.NodeConfig) (dataplane.Plan, error) {
		if got.NodeID != cfg.NodeID || got.Generation != cfg.Generation {
			t.Fatal("changed verified config")
		}
		return dataplane.Plan{}, sentinel
	})}
	if e := bridge.Apply(context.Background(), cfg); !errors.Is(e, sentinel) {
		t.Fatalf("lost verification error %v", e)
	}
	if e := (VerifiedApplier{}).Apply(context.Background(), cfg); e == nil {
		t.Fatal("nil adapter accepted")
	}
}

type stableAdapter struct {
	apply  verifiedAdapterFunc
	stable func(context.Context) error
}

func (a stableAdapter) ApplyVerified(ctx context.Context, cfg platform.NodeConfig) (dataplane.Plan, error) {
	return a.apply(ctx, cfg)
}
func (a stableAdapter) Stable(ctx context.Context) error { return a.stable(ctx) }

func TestVerifiedApplierRequiresIndependentBoundedStableProof(t *testing.T) {
	for _, stableErr := range []error{nil, dataplane.ErrRecoveryRequired, errors.New("kernel readback failed")} {
		t.Run(fmt.Sprint(stableErr), func(t *testing.T) {
			cause := errors.New("apply failed")
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			bridge := VerifiedApplier{Adapter: stableAdapter{apply: func(context.Context, platform.NodeConfig) (dataplane.Plan, error) { return dataplane.Plan{}, cause }, stable: func(probe context.Context) error {
				if e := probe.Err(); e != nil {
					t.Fatalf("stable inherited canceled context: %v", e)
				}
				deadline, ok := probe.Deadline()
				if !ok || time.Until(deadline) > 30*time.Second {
					t.Fatal("stable check is unbounded")
				}
				return stableErr
			}}}
			e := bridge.Apply(ctx, platform.NodeConfig{})
			var safe *SafeApplyFailure
			if errors.As(e, &safe) != (stableErr == nil) || !errors.Is(e, cause) {
				t.Fatalf("stable=%v returned=%v", stableErr, e)
			}
		})
	}
}
