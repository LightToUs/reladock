package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"time"

	"reladock/internal/dataplane"
	"reladock/internal/metering"
	"reladock/internal/platform"
	"reladock/internal/shaping"
)

type RuntimeOptions struct {
	Apply            bool
	StateDir         string
	SNATAddress      string
	ShapingInterface string
	WALDir           string
	NFTBinary        string
	TCBinary         string
	IPBinary         string
	ShapingLeaseFile string
	EthtoolBinary    string
}

func ValidateRuntimeOptions(o RuntimeOptions) error { return validateRuntimeOptions(o, runtime.GOOS) }
func validateRuntimeOptions(o RuntimeOptions, goos string) error {
	if !o.Apply {
		return nil
	}
	if goos != "linux" {
		return errors.New("--apply requires Linux; validation-only is available on this OS")
	}
	if !privatePath(o.StateDir) {
		return errors.New("--apply requires an explicit absolute --state-dir")
	}
	ip := net.ParseIP(o.SNATAddress)
	if ip == nil || ip.To4() == nil || !ip.IsGlobalUnicast() || ip.IsLoopback() {
		return errors.New("--apply requires an explicit unicast IPv4 --snat address")
	}
	if !regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,15}$`).MatchString(o.ShapingInterface) {
		return errors.New("--apply requires an explicit --shaping-interface")
	}
	if !privatePath(o.WALDir) {
		return errors.New("--apply requires an explicit absolute --metering-wal-dir")
	}
	if !privatePath(o.NFTBinary) {
		return errors.New("--apply requires an absolute --nft-binary")
	}
	if !privatePath(o.TCBinary) || !privatePath(o.IPBinary) || !privatePath(o.ShapingLeaseFile) || !privatePath(o.EthtoolBinary) {
		return errors.New("explicit absolute tc/ip binaries and shaping lease file required")
	}
	return nil
}
func privatePath(s string) bool {
	return filepath.IsAbs(s) && filepath.Clean(s) == s && s != string(filepath.Separator)
}

type VerifiedAdapter interface {
	ApplyVerified(context.Context, platform.NodeConfig) (dataplane.Plan, error)
}

// VerifiedApplier bridges the adapter's exact, durable, locally verified retry
// to Client. A successful result is an application ACK, never path health.
type VerifiedApplier struct{ Adapter VerifiedAdapter }

func (v VerifiedApplier) Apply(ctx context.Context, cfg platform.NodeConfig) error {
	if v.Adapter == nil {
		return errors.New("verified adapter is required")
	}
	_, err := v.Adapter.ApplyVerified(ctx, cfg)
	if err != nil {
		if stable, ok := v.Adapter.(interface{ Stable(context.Context) error }); ok {
			probe, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			if stableErr := stable.Stable(probe); stableErr == nil && probe.Err() == nil {
				return &SafeApplyFailure{Err: err}
			}
		}
	}
	return err
}

// PrepareApply composes private journals, verified shaping and reliable meter
// hooks. Explicit lease ownership is checked read-only before any network write.
func PrepareApply(o RuntimeOptions, nodeID string) (*ApplyState, Applier, error) {
	if err := ValidateRuntimeOptions(o); err != nil {
		return nil, nil, err
	}
	if !o.Apply {
		return nil, nil, nil
	}
	lease, err := readShapingLease(o.ShapingLeaseFile)
	if err != nil {
		return nil, nil, err
	}
	if lease.Device != o.ShapingInterface {
		return nil, nil, errors.New("shaping lease/device mismatch")
	}
	shapeOptions := shaping.Options{NodeID: nodeID, Lease: lease, UploadEgress: lease.Device, DownloadEgress: lease.Device}
	backend, err := shaping.NewLinuxBackend(o.TCBinary, o.IPBinary)
	if err != nil {
		return nil, nil, err
	}
	state, err := NewApplyState(o.StateDir)
	if err != nil {
		return nil, nil, err
	}
	store, err := dataplane.NewFileStore(filepath.Join(o.StateDir, "dataplane"))
	if err != nil {
		return nil, nil, err
	}
	runner, err := dataplane.NewExecRunner(o.NFTBinary)
	if err != nil {
		return nil, nil, err
	}
	shapeStore, err := shaping.NewFileStore(filepath.Join(o.StateDir, "shaping"))
	if err != nil {
		return nil, nil, err
	}
	shaper, err := shaping.NewAdapter(shapeOptions, backend, shapeStore, shapeStore.Locker())
	if err != nil {
		return nil, nil, err
	}
	wal, err := metering.Open(o.WALDir, metering.Options{NodeID: nodeID})
	if err != nil {
		return nil, nil, err
	}
	adapter, err := dataplane.NewAdapter(dataplane.Options{NodeID: nodeID, SNATAddress: o.SNATAddress, Resolver: net.DefaultResolver, Shaping: &shapeOptions}, runner, store, store.Locker(), dataplane.ComposedVerifier(runner, shaper))
	if err != nil {
		wal.Close()
		return nil, nil, err
	}
	meter := dataplane.MeterHooks{WAL: wal, Runner: runner}
	adapter.SetTransitionHooks(dataplane.ShapedMeterHooks{MeterHooks: meter, Shaper: shaper})
	preflight, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err = adapter.CheckMeterBinding(preflight); err != nil {
		wal.Close()
		return nil, nil, err
	}
	if err = checkApplyEnvironment(preflight, o, runner); err != nil {
		stopErr := stopAfterEnvironmentFailure(preflight, adapter)
		wal.Close()
		return nil, nil, errors.Join(err, stopErr)
	}
	return state, &NodeEngine{Adapter: adapter, WAL: wal, Meter: meter}, nil
}

func readShapingLease(path string) (shaping.Lease, error) {
	var lease shaping.Lease
	info, e := os.Lstat(path)
	if e != nil {
		return lease, e
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 8192 {
		return lease, errors.New("private regular shaping lease file required")
	}
	data, e := os.ReadFile(path)
	if e != nil {
		return lease, e
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if e = d.Decode(&lease); e != nil {
		return lease, e
	}
	if d.Decode(new(any)) != io.EOF {
		return lease, errors.New("trailing lease data")
	}
	return lease, nil
}
