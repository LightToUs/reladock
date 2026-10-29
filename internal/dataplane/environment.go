package dataplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
)

// CheckMeteringEnvironment reads but never modifies external firewall tables.
// A flowtable fast path can bypass full forward counters; refuse rather than
// silently billing from incomplete statistics or deleting another owner's table.
func CheckMeteringEnvironment(ctx context.Context, r Runner) error {
	raw, e := r.Run(ctx, []string{"-j", "list", "ruleset"}, "")
	if e != nil {
		return e
	}
	var doc struct {
		NFTables []json.RawMessage `json:"nftables"`
	}
	if json.Unmarshal(raw, &doc) != nil || doc.NFTables == nil {
		return errors.New("invalid firewall inventory")
	}
	for _, item := range doc.NFTables {
		var obj map[string]json.RawMessage
		if json.Unmarshal(item, &obj) != nil {
			return errors.New("invalid firewall object")
		}
		if _, exists := obj["flowtable"]; exists {
			return errors.New("flowtable offload bypasses metering; explicit network reconciliation required")
		}
		if rule := obj["rule"]; bytes.Contains(rule, []byte(`"flow"`)) || bytes.Contains(rule, []byte(`"offload"`)) {
			return errors.New("flow offload statement bypasses metering")
		}
	}
	return nil
}
