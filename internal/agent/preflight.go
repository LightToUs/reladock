package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

type HostReport struct {
	OS              string            `json:"os"`
	Arch            string            `json:"arch"`
	Mode            string            `json:"mode"`
	ForwardingReady bool              `json:"forwardingReady"`
	Checks          map[string]string `json:"checks"`
	ObservedAt      time.Time         `json:"observedAt"`
}
type preflightOutput struct{ bytes.Buffer }

func (b *preflightOutput) Write(p []byte) (int, error) {
	n := len(p)
	if b.Len() < 480 {
		keep := 480 - b.Len()
		if keep > n {
			keep = n
		}
		b.Buffer.Write(p[:keep])
	}
	return n, nil
}
func InspectHost(ctx context.Context) (HostReport, error) {
	report := HostReport{OS: runtime.GOOS, Arch: runtime.GOARCH, Mode: "validation-only", Checks: map[string]string{}, ObservedAt: time.Now().UTC()}
	report.Checks["readiness"] = "unknown: read-only inspection does not authorize forwarding; cloud firewall and routing require operator review"
	if runtime.GOOS != "linux" {
		report.Checks["platform"] = "unsupported: Linux required"
		return report, nil
	}
	if b, e := os.ReadFile("/proc/sys/net/ipv4/ip_forward"); e == nil {
		report.Checks["ipForward"] = strings.TrimSpace(string(b))
	} else {
		report.Checks["ipForward"] = "unknown"
	}
	if b, e := os.Readlink("/proc/self/ns/net"); e == nil {
		report.Checks["namespace"] = b
	}
	commands := []struct {
		name, path string
		args       []string
	}{{"addresses", "/usr/sbin/ip", []string{"-j", "-4", "address", "show"}}, {"routes", "/usr/sbin/ip", []string{"-j", "-4", "route", "show"}}, {"links", "/usr/sbin/ip", []string{"-j", "link", "show"}}, {"qdisc", "/usr/sbin/tc", []string{"-j", "qdisc", "show"}}, {"nftables", "/usr/sbin/nft", []string{"-j", "list", "ruleset"}}}
	for _, c := range commands {
		if ctx.Err() != nil {
			return report, ctx.Err()
		}
		if _, e := os.Stat(c.path); e != nil {
			report.Checks[c.name] = "unknown: dependency unavailable at " + c.path
			continue
		}
		sub, cancel := context.WithTimeout(ctx, 2*time.Second)
		cmd := exec.CommandContext(sub, c.path, c.args...)
		var out preflightOutput
		cmd.Stdout = &out
		cmd.Stderr = &out
		err := cmd.Run()
		cancel()
		if err != nil {
			report.Checks[c.name] = "unknown: insufficient permission or inspection failed"
		} else {
			report.Checks[c.name] = out.String()
		}
	}
	report.Checks["offload"] = "unknown: inspect GRO/GSO/TSO/LRO on the explicitly selected dedicated egress before apply"
	report.Checks["dedicatedInterface"] = "unknown: explicit shaping lease required; no interface is automatically adopted"
	return report, nil
}
func (c *Client) ReportPreflight(ctx context.Context) error {
	if e := ValidateEndpoint(c.BaseURL, c.NodeID); e != nil {
		return e
	}
	if c.HTTP == nil {
		return errors.New("mTLS client required")
	}
	report, e := InspectHost(ctx)
	if e != nil {
		return e
	}
	if c.Applier != nil {
		report.Mode = "apply-requested; readiness not established by inspection"
	}
	raw, e := json.Marshal(report)
	if e != nil {
		return e
	}
	req, e := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/agent/preflight", bytes.NewReader(raw))
	if e != nil {
		return e
	}
	req.Header.Set("Content-Type", "application/json")
	res, e := c.HTTP.Do(req)
	if e != nil {
		return errors.New("preflight report connection failed")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return errors.New("preflight report rejected")
	}
	return nil
}
