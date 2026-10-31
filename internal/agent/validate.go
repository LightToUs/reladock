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
	"strconv"

	"reladock/internal/platform"
)

var destination = regexp.MustCompile(`^(?i:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)(?:\.(?i:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?))+$`)
var allocationUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var configIdentifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,79}$`)

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
	listen := net.ParseIP(cfg.ListenAddress)
	if (cfg.SchemaVersion != 1 && cfg.SchemaVersion != 2) || cfg.NodeID != nodeID || cfg.Generation != job.Generation || cfg.Generation < 1 || listen == nil {
		return invalid
	}
	ids := map[string]bool{}
	ports := map[string]map[int]bool{"tcp": {}, "udp": {}}
	for _, r := range cfg.Rules {
		if !configIdentifier.MatchString(r.ID) || !configIdentifier.MatchString(r.TenantID) || ids[r.ID] || r.NodeID != nodeID || r.Version < 1 || (r.Status != "pending-agent" && r.Status != "paused") || r.TargetPort < 1 || r.TargetPort > 65535 || r.ListenPort < 20000 || r.ListenPort > 60000 || r.BandwidthMbps < 1 || r.BandwidthMbps > 1000 || len(r.TargetHost) > 253 || (net.ParseIP(r.TargetHost) == nil && !destination.MatchString(r.TargetHost)) {
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
	if cfg.SchemaVersion == 1 {
		if len(cfg.Allocations) != 0 || len(cfg.Retirements) != 0 {
			return invalid
		}
		return nil
	}
	expected := map[string]string{}
	for _, r := range cfg.Rules {
		protocols := []string{r.Protocol}
		if r.Protocol == "both" {
			protocols = []string{"tcp", "udp"}
		}
		for _, protocol := range protocols {
			expected[protocol+":"+strconv.Itoa(r.ListenPort)] = r.ID
		}
	}
	allocationIDs := map[string]bool{}
	allocationTuples := map[string]bool{}
	predecessorIDs := map[string]bool{}
	for _, allocation := range cfg.Allocations {
		key := allocation.Protocol + ":" + strconv.Itoa(allocation.Port)
		if !allocationUUID.MatchString(allocation.ID) || allocationIDs[allocation.ID] || !configIdentifier.MatchString(allocation.RuleID) || allocation.Address != cfg.ListenAddress || (allocation.Protocol != "tcp" && allocation.Protocol != "udp") || allocation.Port < 20000 || allocation.Port > 60000 || allocationTuples[key] || expected[key] != allocation.RuleID || allocation.PredecessorRetirementID != "" && (!allocationUUID.MatchString(allocation.PredecessorRetirementID) || predecessorIDs[allocation.PredecessorRetirementID]) {
			return invalid
		}
		if allocation.PredecessorRetirementID != "" {
			predecessorIDs[allocation.PredecessorRetirementID] = true
		}
		allocationIDs[allocation.ID] = true
		allocationTuples[key] = true
	}
	if len(allocationTuples) != len(expected) {
		return invalid
	}
	retirementIDs := map[string]bool{}
	retirementTuples := map[string]bool{}
	states := map[string]bool{"waiting-removal": true, "queued": true, "claimed": true, "purging": true, "failed": true}
	for _, retirement := range cfg.Retirements {
		key := retirement.Protocol + ":" + strconv.Itoa(retirement.Port)
		if !allocationUUID.MatchString(retirement.ID) || retirementIDs[retirement.ID] || !allocationUUID.MatchString(retirement.AllocationID) || allocationIDs[retirement.AllocationID] || !configIdentifier.MatchString(retirement.RuleID) || retirement.Address != cfg.ListenAddress || (retirement.Protocol != "tcp" && retirement.Protocol != "udp") || retirement.Port < 20000 || retirement.Port > 60000 || allocationTuples[key] || retirementTuples[key] || retirement.QuarantineGeneration < 1 || retirement.QuarantineGeneration > cfg.Generation || !states[retirement.State] {
			return invalid
		}
		retirementIDs[retirement.ID] = true
		allocationIDs[retirement.AllocationID] = true
		retirementTuples[key] = true
	}
	return nil
}
