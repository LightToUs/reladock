package agent

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os/exec"
	"strings"

	"reladock/internal/dataplane"
	"reladock/internal/shaping"
)

func checkPreparationIdentity(ctx context.Context, o RuntimeOptions, nodeID string, lease shaping.Lease, backend shaping.Backend) error {
	if lease.Device != o.ShapingInterface || lease.OwnerID != nodeID {
		return errors.New("shaping lease identity/device mismatch")
	}
	if err := shaping.CheckPreparationBaseline(ctx, shaping.Options{NodeID: nodeID, Lease: lease, UploadEgress: lease.Device, DownloadEgress: lease.Device}, backend); err != nil {
		return err
	}
	iface, err := net.InterfaceByName(lease.Device)
	if err != nil || iface.Index != lease.IfIndex {
		return errors.New("reviewed interface identity changed")
	}
	addresses, err := iface.Addrs()
	if err != nil {
		return errors.New("reviewed interface addresses unavailable")
	}
	found := false
	for _, value := range addresses {
		raw := value.String()
		if host, _, splitErr := net.ParseCIDR(raw); splitErr == nil && host.String() == o.SNATAddress {
			found = true
		}
	}
	if !found {
		return errors.New("reviewed SNAT/listen address is no longer assigned to the interface")
	}
	runner, err := dataplane.NewExecRunner(o.NFTBinary)
	if err != nil {
		return err
	}
	if err = dataplane.CheckMeteringEnvironment(ctx, runner); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, o.IPBinary, "-4", "rule", "show")
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err = cmd.Run(); err != nil || output.Len() > 64*1024 {
		return errors.New("policy-rule inspection failed")
	}
	if strings.Contains(output.String(), "fwmark") {
		return errors.New("existing fwmark policy requires manual review")
	}
	return nil
}

func runPreparationCommand(ctx context.Context, path string, args ...string) error {
	cmd := exec.CommandContext(ctx, path, args...)
	var output cappedOutput
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Run(); err != nil {
		return errors.New("network prerequisite command failed")
	}
	return nil
}

// PrepareNetwork revalidates the reviewed lease immediately before every
// privileged mutation. It never opens Agent journals or contacts control.
func PrepareNetwork(ctx context.Context, o RuntimeOptions, nodeID string) error {
	if err := ValidateRuntimeOptions(o); err != nil {
		return err
	}
	lease, err := readShapingLease(o.ShapingLeaseFile)
	if err != nil {
		return err
	}
	backend, err := shaping.NewLinuxBackend(o.TCBinary, o.IPBinary)
	if err != nil {
		return err
	}
	if err = checkPreparationIdentity(ctx, o, nodeID, lease, backend); err != nil {
		return err
	}
	if err = runPreparationCommand(ctx, "/usr/sbin/sysctl", "-w", "net.ipv4.ip_forward=1"); err != nil {
		return err
	}
	if err = checkPreparationIdentity(ctx, o, nodeID, lease, backend); err != nil {
		return err
	}
	if err = runPreparationCommand(ctx, o.EthtoolBinary, "-K", lease.Device, "gro", "off", "gso", "off", "tso", "off", "lro", "off"); err != nil {
		return err
	}
	if err = checkPreparationIdentity(ctx, o, nodeID, lease, backend); err != nil {
		return err
	}
	if err = runPreparationCommand(ctx, o.IPBinary, "link", "set", "dev", lease.Device, "alias", lease.Alias()); err != nil {
		return err
	}
	return CheckApplyReadiness(ctx, o, nodeID)
}
