package metering

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
)

const ruleA = "10000000-0000-4000-8000-000000000001"
const ruleB = "10000000-0000-4000-8000-000000000002"
const periodA = "20000000-0000-4000-8000-000000000001"
const periodB = "20000000-0000-4000-8000-000000000002"

func readings(rule, period string, up, down int64) []Reading {
	return []Reading{
		{Counter: Counter{Name: "c_" + strings.ReplaceAll(rule, "-", "") + "_up", RuleID: rule, SubscriptionID: period, Protocol: "tcp", Direction: "original"}, Bytes: up, Packets: up},
		{Counter: Counter{Name: "c_" + strings.ReplaceAll(rule, "-", "") + "_down", RuleID: rule, SubscriptionID: period, Protocol: "tcp", Direction: "reply"}, Bytes: down, Packets: down},
	}
}

func counterJSON(t *testing.T, values []Reading) []byte {
	t.Helper()
	items := []any{map[string]any{"metainfo": map[string]any{"version": "test"}}, map[string]any{"counter": map[string]any{"family": "ip", "table": "foreign", "name": "ignored", "bytes": 999, "packets": 999}}}
	for _, r := range values {
		items = append(items, map[string]any{"counter": map[string]any{"family": "ip", "table": "reladock", "name": r.Name, "bytes": r.Bytes, "packets": r.Packets}})
	}
	b, e := json.Marshal(map[string]any{"nftables": items})
	if e != nil {
		t.Fatal(e)
	}
	return b
}

func TestParseAllNamedForwardCountersAndProtocolDirections(t *testing.T) {
	expected := readings(ruleA, periodA, 11, 23)
	udp := readings(ruleA, periodA, 31, 47)
	for i := range udp {
		udp[i].Protocol = "udp"
		udp[i].Name += "_udp"
	}
	expected = append(expected, udp...)
	expected = append(expected, readings(ruleB, periodB, 3, 5)...)
	specs := []Counter{}
	for _, r := range expected {
		specs = append(specs, r.Counter)
	}
	parsed, e := ParseCounters(counterJSON(t, expected), specs)
	if e != nil {
		t.Fatal(e)
	}
	if len(parsed) != 6 {
		t.Fatalf("missing protocol/direction: %+v", parsed)
	}
	var up, down int64
	for _, r := range parsed {
		if r.RuleID == ruleA {
			if r.Direction == "original" {
				up += r.Bytes
			} else {
				down += r.Bytes
			}
		}
	}
	if up != 42 || down != 70 {
		t.Fatalf("wrong all-packet direction sums: %d/%d", up, down)
	}
}

func TestParseRejectsMissingNegativeFractionOverflowAndAmbiguity(t *testing.T) {
	values := readings(ruleA, periodA, 11, 23)
	specs := []Counter{values[0].Counter, values[1].Counter}
	raw := string(counterJSON(t, values))
	for name, input := range map[string]string{
		"missing-counter": string(counterJSON(t, values[:1])),
		"negative":        strings.Replace(raw, `"bytes":11`, `"bytes":-1`, 1),
		"overflow":        strings.Replace(raw, `"bytes":11`, `"bytes":9223372036854775808`, 1),
		"fraction":        strings.Replace(raw, `"bytes":11`, `"bytes":1.5`, 1),
		"duplicate-field": strings.Replace(raw, `"bytes":11`, `"bytes":-1,"bytes":11`, 1),
		"missing-bytes":   strings.Replace(raw, `"bytes":11,`, "", 1),
		"null":            strings.Replace(raw, `"bytes":11`, `"bytes":null`, 1),
		"missing-packets": strings.Replace(raw, `"packets":11,`, "", 1),
		"duplicate":       string(counterJSON(t, append(values, values[0]))),
		"trailing":        raw + ` {}`,
		"no-array":        `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, e := ParseCounters([]byte(input), specs); !errors.Is(e, ErrCounters) {
				t.Fatalf("accepted malformed snapshot: %v", e)
			}
		})
	}
	tooBig := append([]byte(`{"nftables":[]}`), []byte(strings.Repeat(" ", MaxCounterJSONBytes))...)
	if _, e := ParseCounters(tooBig, nil); !errors.Is(e, ErrCounters) {
		t.Fatalf("unbounded parser: %v", e)
	}
	// Individual counters fit, but a rule's upload+download cannot overflow int64.
	values = readings(ruleA, periodA, math.MaxInt64, 1)
	if _, e := ParseCounters(counterJSON(t, values), specs); !errors.Is(e, ErrCounters) {
		t.Fatalf("aggregate overflow: %v", e)
	}
}
