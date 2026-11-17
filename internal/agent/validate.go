package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
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
	if job.Checksum != hex.EncodeToString(hash[:]) || len(job.Payload) > platform.MaxConfigPayloadBytes || job.NodeID != nodeID {
		return invalid
	}
	var cfg platform.NodeConfig
	dec := json.NewDecoder(bytes.NewReader(job.Payload))
	dec.DisallowUnknownFields()
	if dec.Decode(&cfg) != nil || dec.Decode(new(any)) != io.EOF {
		return invalid
	}
	listen := net.ParseIP(cfg.ListenAddress)
	if cfg.SchemaVersion < 1 || cfg.SchemaVersion > 5 || cfg.NodeID != nodeID || cfg.Generation != job.Generation || cfg.Generation < 1 || listen == nil {
		return invalid
	}
	ids := map[string]bool{}
	ports := map[string]map[int]bool{"tcp": {}, "udp": {}}
	loggingRules := 0
	blockedCIDRs := 0
	for _, r := range cfg.Rules {
		end := r.ListenPort
		portsValid := r.ListenPort >= 20000 && r.ListenPort <= 60000 && r.ListenPortEnd == 0
		if cfg.SchemaVersion >= 4 {
			end = r.ListenPortEnd
			portsValid = r.ListenPort >= 1 && end >= r.ListenPort && end <= 65535 && end-r.ListenPort+1 <= 4096
		}
		if !configIdentifier.MatchString(r.ID) || !configIdentifier.MatchString(r.TenantID) || ids[r.ID] || r.NodeID != nodeID || r.Version < 1 || (r.Status != "pending-agent" && r.Status != "paused") || r.TargetPort < 1 || r.TargetPort > 65535 || !portsValid || r.BandwidthMbps < 1 || r.BandwidthMbps > 1000 || len(r.TargetHost) > 253 || (net.ParseIP(r.TargetHost) == nil && !destination.MatchString(r.TargetHost)) {
			return invalid
		}
		ids[r.ID] = true
		if r.SourceLoggingEnabled {
			loggingRules++
			if cfg.SchemaVersion < 3 || r.Status != "pending-agent" {
				return invalid
			}
		}
		if len(r.BlockedSourceCIDRs) > 256 || len(r.BlockedSourceCIDRs) > 0 && cfg.SchemaVersion != 5 {
			return invalid
		}
		previous := ""
		for _, raw := range r.BlockedSourceCIDRs {
			prefix, parseErr := netip.ParsePrefix(raw)
			if parseErr != nil || !prefix.Addr().Is4() || prefix.Bits() == 0 || prefix != prefix.Masked() || prefix.String() != raw || prefix.Addr().IsUnspecified() || prefix.Addr().IsLoopback() || prefix.Addr().IsMulticast() || previous != "" && raw <= previous {
				return invalid
			}
			previous = raw
			blockedCIDRs++
		}
		protocols := []string{r.Protocol}
		if r.Protocol == "both" {
			protocols = []string{"tcp", "udp"}
		}
		for _, protocol := range protocols {
			if protocol != "tcp" && protocol != "udp" {
				return invalid
			}
			for port := r.ListenPort; port <= end; port++ {
				if ports[protocol][port] {
					return invalid
				}
				ports[protocol][port] = true
			}
		}
	}
	if cfg.SchemaVersion == 3 && loggingRules == 0 {
		return invalid
	}
	if cfg.SchemaVersion == 5 && blockedCIDRs == 0 {
		return invalid
	}
	if cfg.SchemaVersion == 1 {
		if len(cfg.Allocations) != 0 || len(cfg.Retirements) != 0 {
			return invalid
		}
		return nil
	}
	expected := map[string]string{}
	for _, r := range cfg.Rules {
		end := r.ListenPort
		if cfg.SchemaVersion >= 4 {
			end = r.ListenPortEnd
		}
		protocols := []string{r.Protocol}
		if r.Protocol == "both" {
			protocols = []string{"tcp", "udp"}
		}
		for _, protocol := range protocols {
			for port := r.ListenPort; port <= end; port++ {
				expected[protocol+":"+strconv.Itoa(port)] = r.ID
			}
		}
	}
	allocationIDs := map[string]bool{}
	allocationTuples := map[string]bool{}
	predecessorIDs := map[string]bool{}
	for _, allocation := range cfg.Allocations {
		key := allocation.Protocol + ":" + strconv.Itoa(allocation.Port)
		portValid := allocation.Port >= 20000 && allocation.Port <= 60000
		if cfg.SchemaVersion >= 4 {
			portValid = allocation.Port >= 1 && allocation.Port <= 65535
		}
		if !allocationUUID.MatchString(allocation.ID) || allocationIDs[allocation.ID] || !configIdentifier.MatchString(allocation.RuleID) || allocation.Address != cfg.ListenAddress || (allocation.Protocol != "tcp" && allocation.Protocol != "udp") || !portValid || allocationTuples[key] || expected[key] != allocation.RuleID || allocation.PredecessorRetirementID != "" && (!allocationUUID.MatchString(allocation.PredecessorRetirementID) || predecessorIDs[allocation.PredecessorRetirementID]) {
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
		portValid := retirement.Port >= 20000 && retirement.Port <= 60000
		if cfg.SchemaVersion >= 4 {
			portValid = retirement.Port >= 1 && retirement.Port <= 65535
		}
		if !allocationUUID.MatchString(retirement.ID) || retirementIDs[retirement.ID] || !allocationUUID.MatchString(retirement.AllocationID) || allocationIDs[retirement.AllocationID] || !configIdentifier.MatchString(retirement.RuleID) || retirement.Address != cfg.ListenAddress || (retirement.Protocol != "tcp" && retirement.Protocol != "udp") || !portValid || allocationTuples[key] || retirementTuples[key] || retirement.QuarantineGeneration < 1 || retirement.QuarantineGeneration > cfg.Generation || !states[retirement.State] {
			return invalid
		}
		retirementIDs[retirement.ID] = true
		allocationIDs[retirement.AllocationID] = true
		retirementTuples[key] = true
	}
	return nil
}
