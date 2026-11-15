// Package sourceflow defines the bounded, canonical metadata contract shared
// by the Linux dataplane, Agent journal, and control-plane applied ACK.
package sourceflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/netip"
	"reflect"
	"regexp"
	"sort"
	"strconv"
)

const ManifestSchemaVersion = 1

var (
	nodeIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	uuidPattern   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

func ValidateKernelBootID(value string) error {
	if !uuidPattern.MatchString(value) {
		return errors.New("invalid kernel boot identity")
	}
	return nil
}

type AppliedRule struct {
	AllocationID         string `json:"allocationId"`
	TenantID             string `json:"tenantId"`
	RuleID               string `json:"ruleId"`
	RuleVersion          int64  `json:"ruleVersion"`
	SourceLoggingEnabled bool   `json:"sourceLoggingEnabled"`
	Protocol             string `json:"protocol"`
	RelayIP              string `json:"relayIp"`
	RelayPort            int    `json:"relayPort"`
	TargetIP             string `json:"targetIp"`
	TargetPort           int    `json:"targetPort"`
}

type Manifest struct {
	SchemaVersion int           `json:"schemaVersion"`
	NodeID        string        `json:"nodeId"`
	Generation    int64         `json:"generation"`
	Rules         []AppliedRule `json:"rules"`
}

func validIPv4(value string) bool {
	ip, err := netip.ParseAddr(value)
	return err == nil && ip.Is4() && ip.IsGlobalUnicast() && !ip.IsLoopback() && !ip.IsUnspecified()
}

func validateRule(rule AppliedRule) error {
	if !uuidPattern.MatchString(rule.AllocationID) || !uuidPattern.MatchString(rule.TenantID) || !uuidPattern.MatchString(rule.RuleID) || rule.RuleVersion < 1 || !rule.SourceLoggingEnabled || (rule.Protocol != "tcp" && rule.Protocol != "udp") || !validIPv4(rule.RelayIP) || !validIPv4(rule.TargetIP) || rule.RelayPort < 1 || rule.RelayPort > 65535 || rule.TargetPort < 1 || rule.TargetPort > 65535 {
		return errors.New("invalid source-flow manifest rule")
	}
	return nil
}

func NewManifest(nodeID string, generation int64, rules []AppliedRule) (Manifest, string, error) {
	manifest := Manifest{SchemaVersion: ManifestSchemaVersion, NodeID: nodeID, Generation: generation, Rules: append([]AppliedRule(nil), rules...)}
	if !nodeIDPattern.MatchString(nodeID) || generation < 1 || len(rules) < 1 || len(rules) > 10000 {
		return Manifest{}, "", errors.New("invalid source-flow manifest identity")
	}
	sort.Slice(manifest.Rules, func(i, j int) bool { return manifest.Rules[i].AllocationID < manifest.Rules[j].AllocationID })
	allocations := make(map[string]struct{}, len(manifest.Rules))
	tuples := make(map[string]struct{}, len(manifest.Rules))
	for _, rule := range manifest.Rules {
		if err := validateRule(rule); err != nil {
			return Manifest{}, "", err
		}
		tuple := rule.Protocol + "/" + rule.RelayIP + "/" + strconv.Itoa(rule.RelayPort)
		if _, exists := allocations[rule.AllocationID]; exists {
			return Manifest{}, "", errors.New("duplicate source-flow allocation")
		}
		if _, exists := tuples[tuple]; exists {
			return Manifest{}, "", errors.New("duplicate source-flow relay tuple")
		}
		allocations[rule.AllocationID] = struct{}{}
		tuples[tuple] = struct{}{}
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return Manifest{}, "", err
	}
	sum := sha256.Sum256(raw)
	return manifest, hex.EncodeToString(sum[:]), nil
}

func ValidateManifest(manifest Manifest, digest string) error {
	if manifest.SchemaVersion != ManifestSchemaVersion || !digestPattern.MatchString(digest) {
		return errors.New("invalid source-flow manifest schema or digest")
	}
	canonical, expected, err := NewManifest(manifest.NodeID, manifest.Generation, manifest.Rules)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(canonical, manifest) || expected != digest {
		return errors.New("source-flow manifest is not canonical")
	}
	return nil
}
