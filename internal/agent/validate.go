package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"regexp"

	"reladock/internal/platform"
)

var destination = regexp.MustCompile(`^(?i:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)(?:\.(?i:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?))+$`)

func ValidateConfig(nodeID string, job platform.ConfigJob) error {
	invalid := errors.New("invalid desired configuration")
	hash := sha256.Sum256(job.Payload)
	if job.Checksum != hex.EncodeToString(hash[:]) || len(job.Payload) > 2*1024*1024 || job.NodeID != nodeID {
		return invalid
	}
	var cfg platform.NodeConfig
	dec := json.NewDecoder(bytes.NewReader(job.Payload))
	dec.DisallowUnknownFields()
	if dec.Decode(&cfg) != nil || dec.Decode(new(any)) != io.EOF {
		return invalid
	}
	if cfg.SchemaVersion != 1 || cfg.NodeID != nodeID || cfg.Generation != job.Generation || cfg.Generation < 1 || net.ParseIP(cfg.ListenAddress) == nil {
		return invalid
	}
	ids := map[string]bool{}
	ports := map[string]map[int]bool{"tcp": {}, "udp": {}}
	for _, r := range cfg.Rules {
		if r.ID == "" || r.TenantID == "" || ids[r.ID] || r.NodeID != nodeID || r.Version < 1 || (r.Status != "pending-agent" && r.Status != "paused") || r.TargetPort < 1 || r.TargetPort > 65535 || r.ListenPort < 20000 || r.ListenPort > 60000 || r.BandwidthMbps < 1 || r.BandwidthMbps > 1000 || len(r.TargetHost) > 253 || (net.ParseIP(r.TargetHost) == nil && !destination.MatchString(r.TargetHost)) {
			return invalid
		}
		ids[r.ID] = true
		protocols := []string{r.Protocol}
		if r.Protocol == "both" {
			protocols = []string{"tcp", "udp"}
		}
		for _, protocol := range protocols {
			if protocol != "tcp" && protocol != "udp" {
				return invalid
			}
			if ports[protocol][r.ListenPort] {
				return invalid
			}
			ports[protocol][r.ListenPort] = true
		}
	}
	return nil
}
