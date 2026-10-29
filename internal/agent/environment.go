package agent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"reladock/internal/dataplane"
	"strings"
	"time"
)

func validateOffloadFeatures(raw []byte) error {
	expected := map[string]bool{"generic-receive-offload": false, "generic-segmentation-offload": false, "tcp-segmentation-offload": false, "large-receive-offload": false}
	for _, line := range strings.Split(string(raw), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		if seen, wanted := expected[key]; wanted {
			if seen || !(strings.TrimSpace(value) == "off" || strings.TrimSpace(value) == "off [fixed]") {
				return errors.New("metering requires disabled GRO/GSO/TSO/LRO; operator network review required")
			}
			expected[key] = true
		}
	}
	for _, seen := range expected {
		if !seen {
			return errors.New("incomplete offload inspection")
		}
	}
	return nil
}

type cappedOutput struct{ bytes.Buffer }

func (b *cappedOutput) Write(p []byte) (int, error) {
	if len(p) > 64*1024-b.Len() {
		return 0, errors.New("inspection output too large")
	}
	return b.Buffer.Write(p)
}

func checkApplyEnvironment(ctx context.Context, o RuntimeOptions, r dataplane.Runner) error {
	ipForward, e := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
	if e != nil || strings.TrimSpace(string(ipForward)) != "1" {
		return errors.New("IPv4 forwarding must be explicitly enabled by the node operator")
	}
	if e = dataplane.CheckMeteringEnvironment(ctx, r); e != nil {
		return e
	}
	cmd := exec.CommandContext(ctx, o.EthtoolBinary, "-k", o.ShapingInterface)
	out, diagnostic := &cappedOutput{}, &cappedOutput{}
	cmd.Stdout = out
	cmd.Stderr = diagnostic
	if e = cmd.Run(); e != nil {
		return errors.New("offload inspection failed; ethtool and explicit operator setup required")
	}
	return validateOffloadFeatures(out.Bytes())
}

type runtimeStopper interface{ StopOwned(context.Context) error }

func stopAfterEnvironmentFailure(ctx context.Context, s runtimeStopper) error {
	stop, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return s.StopOwned(stop)
}
