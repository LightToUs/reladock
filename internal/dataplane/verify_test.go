package dataplane

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type runnerFunc func(context.Context, []string, string) ([]byte, error)

func (f runnerFunc) Run(c context.Context, a []string, s string) ([]byte, error) { return f(c, a, s) }

func readbackFixture(p Plan) []map[string]any {
	items := []map[string]any{{"table": map[string]any{"family": "ip", "name": "reladock", "comment": owner}}}
	for _, v := range []struct {
		name, kind, hook string
		prio             int
	}{{"prerouting", "nat", "prerouting", -100}, {"postrouting", "nat", "postrouting", 100}, {"forward", "filter", "forward", 0}, {"relay", "", "", 0}} {
		chain := map[string]any{"family": "ip", "table": "reladock", "name": v.name}
		if v.kind != "" {
			chain["type"] = v.kind
			chain["hook"] = v.hook
			chain["prio"] = v.prio
			chain["policy"] = "accept"
		} else {
			chain["comment"] = planToken(p)
		}
		items = append(items, map[string]any{"chain": chain})
	}
	for _, v := range p.Counters {
		items = append(items, map[string]any{"counter": map[string]any{"family": "ip", "table": "reladock", "name": v.Name, "packets": 17, "bytes": 1234}})
	}
	n := len(p.Counters) / 2
	for chain, count := range map[string]int{"prerouting": n, "postrouting": n, "forward": len(p.Allocations), "relay": 2*n + 1} {
		for i := 0; i < count; i++ {
			items = append(items, map[string]any{"rule": map[string]any{"family": "ip", "table": "reladock", "chain": chain}})
		}
	}
	return items
}

func TestReadbackRejectsMissingCountersWrongGenerationAndCommandFailure(t *testing.T) {
	p, e := Compile(context.Background(), fixture(), options())
	if e != nil {
		t.Fatal(e)
	}
	for _, mode := range []string{"valid", "missing counter", "wrong generation", "command failure", "invalid JSON"} {
		t.Run(mode, func(t *testing.T) {
			items := readbackFixture(p)
			if mode == "missing counter" {
				items = append(items[:5], items[6:]...)
			}
			if mode == "wrong generation" {
				items[4]["chain"].(map[string]any)["comment"] = "old"
			}
			b, _ := json.Marshal(map[string]any{"nftables": items})
			r := runnerFunc(func(_ context.Context, args []string, in string) ([]byte, error) {
				if len(args) != 5 || args[2] != "table" || args[4] != "reladock" || in != "" {
					t.Fatal(args, in)
				}
				if mode == "command failure" {
					return nil, errors.New("readback failed")
				}
				if mode == "invalid JSON" {
					return []byte("{}"), nil
				}
				return b, nil
			})
			e := ReadbackVerifier(r)(context.Background(), p)
			if (e == nil) != (mode == "valid") {
				t.Fatalf("%s: %v", mode, e)
			}
		})
	}
}

func TestReadbackUsesStrictTextFallbackWhenJSONOmitsComments(t *testing.T) {
	p, e := Compile(context.Background(), fixture(), options())
	if e != nil {
		t.Fatal(e)
	}
	items := readbackFixture(p)
	delete(items[0]["table"].(map[string]any), "comment")
	delete(items[4]["chain"].(map[string]any), "comment")
	raw, _ := json.Marshal(map[string]any{"nftables": items})
	for _, valid := range []bool{true, false} {
		text := "table ip reladock { # handle 24\n\tcomment \"reladock-dataplane-v1\"\n\tchain relay { # handle 4\n\t\tcomment \"" + planToken(p) + "\"\n\t}\n}\n"
		if !valid {
			text = "table ip reladock {\n\tcomment \"foreign\"\n}\n"
		}
		r := runnerFunc(func(_ context.Context, args []string, input string) ([]byte, error) {
			if input != "" {
				t.Fatal("readback wrote nft input")
			}
			if reflect.DeepEqual(args, []string{"-j", "list", "table", "ip", "reladock"}) {
				return raw, nil
			}
			if reflect.DeepEqual(args, []string{"-a", "list", "table", "ip", "reladock"}) {
				return []byte(text), nil
			}
			t.Fatal("unexpected readback command", args)
			return nil, nil
		})
		e = ReadbackVerifier(r)(context.Background(), p)
		if (e == nil) != valid {
			t.Fatalf("valid=%t err=%v", valid, e)
		}
	}
}

func TestNFTTextCommentParserFailsClosed(t *testing.T) {
	valid := "table ip reladock { # handle 24\n\tcomment \"reladock-dataplane-v1\"\n\tchain relay { # handle 4\n\t\tcomment \"reladock-plan-abc\"\n\t}\n}\n"
	for name, raw := range map[string]string{
		"valid":               valid,
		"wrong table":         strings.Replace(valid, "table ip reladock", "table ip foreign", 1),
		"duplicate owner":     strings.Replace(valid, "\tchain relay", "\tcomment \"second\"\n\tchain relay", 1),
		"duplicate relay":     strings.Replace(valid, "\t}\n}", "\t\tcomment \"second\"\n\t}\n}", 1),
		"missing close":       strings.TrimSuffix(valid, "}\n"),
		"trailing table":      valid + "table ip second {}\n",
		"quoted brace bypass": "table ip reladock {\n\tchain \"evil }\" {\n\t\tcomment \"reladock-dataplane-v1\"\n\t\tcounter comment \"{\"\n\t}\n}\n",
	} {
		t.Run(name, func(t *testing.T) {
			comments, e := parseNFTTextComments([]byte(raw))
			if name == "valid" {
				if e != nil || comments.Owner != owner || comments.Relay != "reladock-plan-abc" {
					t.Fatalf("valid parse: %+v %v", comments, e)
				}
			} else if e == nil {
				t.Fatalf("unsafe text accepted: %+v", comments)
			}
		})
	}
}
