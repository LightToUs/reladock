package dataplane

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"reladock/internal/platform"
	"reladock/internal/shaping"
)

func exactObject(raw json.RawMessage, key string) (json.RawMessage, bool) {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) != 1 {
		return nil, false
	}
	value, ok := object[key]
	return value, ok
}

func exactFields(raw json.RawMessage, expected map[string]any, optional map[string]any) bool {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) < len(expected) || len(object) > len(expected)+len(optional) {
		return false
	}
	for key, want := range expected {
		value, ok := object[key]
		if !ok || !jsonValueEqual(value, want) {
			return false
		}
		delete(object, key)
	}
	for key, rawValue := range object {
		want, ok := optional[key]
		if !ok || !jsonValueEqual(rawValue, want) {
			return false
		}
	}
	return true
}

func jsonValueEqual(raw json.RawMessage, want any) bool {
	var got any
	if json.Unmarshal(raw, &got) != nil {
		return false
	}
	wantJSON, _ := json.Marshal(want)
	var normalized any
	_ = json.Unmarshal(wantJSON, &normalized)
	return reflect.DeepEqual(got, normalized)
}

func exactMatch(raw json.RawMessage, op, leftKind string, left map[string]any, optionalLeft map[string]any, right any) bool {
	match, ok := exactObject(raw, "match")
	if !ok {
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(match, &fields) != nil || len(fields) != 3 || !jsonValueEqual(fields["op"], op) || !jsonValueEqual(fields["right"], right) {
		return false
	}
	leftRaw, ok := exactObject(fields["left"], leftKind)
	return ok && exactFields(leftRaw, left, optionalLeft)
}

func exactVerdict(raw json.RawMessage, kind string) bool {
	value, ok := exactObject(raw, kind)
	return ok && string(value) == "null"
}

func exactJump(raw json.RawMessage) bool {
	value, ok := exactObject(raw, "jump")
	return ok && exactFields(value, map[string]any{"target": "relay"}, nil)
}

func validGuardExpressions(expr []json.RawMessage, guard AllocationGuard, quarantine bool) bool {
	if quarantine {
		return len(expr) == 4 &&
			exactMatch(expr[0], "==", "payload", map[string]any{"protocol": "ip", "field": "daddr"}, nil, guard.Address) &&
			exactMatch(expr[1], "==", "meta", map[string]any{"key": "l4proto"}, nil, guard.Protocol) &&
			exactMatch(expr[2], "==", "payload", map[string]any{"protocol": guard.Protocol, "field": "dport"}, nil, guard.Port) &&
			exactVerdict(expr[3], "drop")
	}
	return len(expr) == 5 &&
		exactMatch(expr[0], "in", "ct", map[string]any{"key": "status"}, nil, "dnat") &&
		exactMatch(expr[1], "==", "meta", map[string]any{"key": "l4proto"}, nil, guard.Protocol) &&
		exactMatch(expr[2], "==", "ct", map[string]any{"key": "daddr", "dir": "original"}, map[string]any{"family": "ip"}, guard.Address) &&
		exactMatch(expr[3], "==", "ct", map[string]any{"key": "proto-dst", "dir": "original"}, nil, guard.Port) &&
		exactJump(expr[4])
}

func planToken(p Plan) string {
	if p.Config.SchemaVersion == 1 {
		legacyConfig := struct {
			SchemaVersion int                   `json:"schemaVersion"`
			NodeID        string                `json:"nodeId"`
			Generation    int64                 `json:"generation"`
			ListenAddress string                `json:"listenAddress"`
			Rules         []platform.ConfigRule `json:"rules"`
		}{p.Config.SchemaVersion, p.Config.NodeID, p.Config.Generation, p.Config.ListenAddress, p.Config.Rules}
		b, _ := json.Marshal(struct {
			Config      any
			SNAT        string
			Allocations []Allocation
			Incarnation string
			Shaping     *shaping.Options
			BuiltAt     time.Time
			QuotaUsed   map[string]int64
		}{legacyConfig, p.SNATAddress, p.Allocations, p.Incarnation, p.Shaping, p.BuiltAt, p.QuotaUsed})
		return fmt.Sprintf("reladock-plan-%x", sha256.Sum256(b))
	}
	b, _ := json.Marshal(struct {
		Config        platform.NodeConfig
		SNAT          string
		Allocations   []Allocation
		Quarantines   []AllocationGuard
		ForwardGuards []AllocationGuard
		Incarnation   string
		Shaping       *shaping.Options
		BuiltAt       time.Time
		QuotaUsed     map[string]int64
	}{p.Config, p.SNATAddress, p.Allocations, p.Quarantines, p.ForwardGuards, p.Incarnation, p.Shaping, p.BuiltAt, p.QuotaUsed})
	return fmt.Sprintf("reladock-plan-%x", sha256.Sum256(b))
}

// ReadbackVerifier checks the local table's ownership, generation token, chain
// hooks, counter identities and rule counts. It is deliberately not a path
// health check or a semantic proof of kernel forwarding/offload behavior.
func ReadbackVerifier(r Runner) Verifier {
	return func(ctx context.Context, p Plan) error {
		out, e := r.Run(ctx, []string{"-j", "list", "table", "ip", Table}, "")
		if e != nil {
			return e
		}
		var doc struct {
			NFTables []struct {
				Table *struct{ Family, Name, Comment string } `json:"table"`
				Chain *struct {
					Family, Table, Name, Type, Hook, Policy, Comment string
					Prio                                             int
				} `json:"chain"`
				Counter *struct{ Family, Table, Name string } `json:"counter"`
				Rule    *struct {
					Family, Table, Chain, Comment string
					Expr                          []json.RawMessage `json:"expr"`
				} `json:"rule"`
			} `json:"nftables"`
		}
		if e = json.Unmarshal(out, &doc); e != nil {
			return e
		}
		bad := errors.New("nft readback does not match compiled table structure")
		chains := map[string]bool{}
		counters := map[string]bool{}
		rules := map[string]int{}
		quarantineGuards := map[string]AllocationGuard{}
		forwardGuards := map[string]AllocationGuard{}
		for _, guard := range p.Quarantines {
			quarantineGuards["reladock-quarantine-"+guard.ID] = guard
		}
		for _, guard := range p.ForwardGuards {
			forwardGuards["reladock-forward-"+guard.ID] = guard
		}
		tables := 0
		tableComment, relayComment := "", ""
		for _, v := range doc.NFTables {
			if v.Table != nil {
				if v.Table.Family != "ip" || v.Table.Name != Table {
					return bad
				}
				tableComment = v.Table.Comment
				tables++
			}
			if c := v.Chain; c != nil {
				if c.Family != "ip" || c.Table != Table || chains[c.Name] {
					return bad
				}
				chains[c.Name] = true
				switch c.Name {
				case "quarantine":
					if p.Config.SchemaVersion != 2 || c.Type != "filter" || c.Hook != "prerouting" || c.Prio != -300 || c.Policy != "accept" {
						return bad
					}
				case "prerouting":
					if c.Type != "nat" || c.Hook != "prerouting" || c.Prio != -100 || c.Policy != "accept" {
						return bad
					}
				case "postrouting":
					if c.Type != "nat" || c.Hook != "postrouting" || c.Prio != 100 || c.Policy != "accept" {
						return bad
					}
				case "forward":
					if c.Type != "filter" || c.Hook != "forward" || c.Prio != 0 || c.Policy != "accept" {
						return bad
					}
				case "relay":
					if c.Type != "" || c.Hook != "" {
						return bad
					}
					relayComment = c.Comment
				default:
					return bad
				}
			}
			if c := v.Counter; c != nil {
				if c.Family != "ip" || c.Table != Table || counters[c.Name] {
					return bad
				}
				counters[c.Name] = true
			}
			if v.Rule != nil {
				if v.Rule.Family != "ip" || v.Rule.Table != Table {
					return bad
				}
				if p.Config.SchemaVersion == 2 && (v.Rule.Chain == "quarantine" || v.Rule.Chain == "forward") {
					expected := forwardGuards
					quarantine := false
					if v.Rule.Chain == "quarantine" {
						expected = quarantineGuards
						quarantine = true
					}
					guard, ok := expected[v.Rule.Comment]
					if !ok || !validGuardExpressions(v.Rule.Expr, guard, quarantine) {
						return bad
					}
					delete(expected, v.Rule.Comment)
				}
				rules[v.Rule.Chain]++
			}
		}
		if tableComment == "" || relayComment == "" {
			comments, textErr := readNFTTextComments(ctx, r)
			if textErr != nil {
				return textErr
			}
			if tableComment == "" {
				tableComment = comments.Owner
			}
			if relayComment == "" {
				relayComment = comments.Relay
			}
		}
		expectedChains := 4
		if p.Config.SchemaVersion == 2 {
			expectedChains = 5
		}
		if tableComment != owner || relayComment != planToken(p) || tables != 1 || len(chains) != expectedChains || len(counters) != len(p.Counters) {
			return bad
		}
		if p.Config.SchemaVersion == 2 && (len(quarantineGuards) != 0 || len(forwardGuards) != 0) {
			return bad
		}
		for _, c := range p.Counters {
			if !counters[c.Name] {
				return bad
			}
		}
		n := len(p.Counters) / 2
		expectedRules := map[string]int{"prerouting": n, "postrouting": n, "forward": len(p.Allocations), "relay": 2*n + 1}
		if p.Config.SchemaVersion == 2 {
			expectedRules["forward"] = len(p.ForwardGuards)
			expectedRules["quarantine"] = len(p.Quarantines)
		}
		for name, count := range expectedRules {
			if rules[name] != count {
				return bad
			}
			delete(rules, name)
		}
		if len(rules) != 0 {
			return bad
		}
		return nil
	}
}
