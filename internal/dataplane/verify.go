package dataplane

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"reladock/internal/platform"
	"reladock/internal/shaping"
)

func planToken(p Plan) string {
	b, _ := json.Marshal(struct {
		Config      platform.NodeConfig
		SNAT        string
		Allocations []Allocation
		Incarnation string
		Shaping     *shaping.Options
		BuiltAt     time.Time
		QuotaUsed   map[string]int64
	}{p.Config, p.SNATAddress, p.Allocations, p.Incarnation, p.Shaping, p.BuiltAt, p.QuotaUsed})
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
				Counter *struct{ Family, Table, Name string }  `json:"counter"`
				Rule    *struct{ Family, Table, Chain string } `json:"rule"`
			} `json:"nftables"`
		}
		if e = json.Unmarshal(out, &doc); e != nil {
			return e
		}
		bad := errors.New("nft readback does not match compiled table structure")
		chains := map[string]bool{}
		counters := map[string]bool{}
		rules := map[string]int{}
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
		if tableComment != owner || relayComment != planToken(p) || tables != 1 || len(chains) != 4 || len(counters) != len(p.Counters) {
			return bad
		}
		for _, c := range p.Counters {
			if !counters[c.Name] {
				return bad
			}
		}
		n := len(p.Counters) / 2
		for name, count := range map[string]int{"prerouting": n, "postrouting": n, "forward": len(p.Allocations), "relay": 2*n + 1} {
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
