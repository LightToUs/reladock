// Package metering persists node usage independently of the control plane and
// dataplane. It never executes nft, changes firewall rules or opens a network.
package metering

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
)

const MaxCounterJSONBytes = 8 << 20
const MaxCounters = 16384

var (
	ErrCounters      = errors.New("invalid or incomplete business counters")
	ErrCounterReset  = errors.New("counter decreased: confirmed reset requires a new epoch")
	ErrEpochRequired = errors.New("counter/period mapping changed: explicit new epoch required")
	ErrFull          = errors.New("metering WAL capacity exhausted")
	ErrAck           = errors.New("ACK does not match the oldest durable report")
	ErrClosed        = errors.New("metering WAL is closed")
	ErrUncertain     = errors.New("metering persistence failed: close and reopen before continuing")
	ErrLocked        = errors.New("metering WAL already open by another owner")
	ErrCorrupt       = errors.New("invalid metering WAL: manual reconciliation required")
)

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var counterName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,79}$`)
var nodeName = regexp.MustCompile(`^[a-z][a-z0-9-]{1,63}$`)
var incarnationName = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,128}$`)

// Counter is copied from Plan.Counters plus its ConfigRule.SubscriptionID.
// Only verified forward-path named counters belong here, never NAT first-packet
// or node-wide statistics. The complete expected manifest is mandatory.
type Counter struct{ Name, RuleID, SubscriptionID, Protocol, Direction string }
type Reading struct {
	Counter
	Bytes, Packets int64
}

type group struct {
	RuleID, SubscriptionID     string
	Readings                   []Reading
	UploadBytes, DownloadBytes int64
}

func bytePair(up, down int64) bool { return up >= 0 && down >= 0 && up <= math.MaxInt64-down }

func groupReadings(readings []Reading) ([]group, error) {
	if len(readings) > MaxCounters {
		return nil, ErrCounters
	}
	byRule := map[string]*group{}
	names := map[string]bool{}
	tuples := map[string]bool{}
	for _, r := range readings {
		if !counterName.MatchString(r.Name) || !uuidPattern.MatchString(r.RuleID) || r.SubscriptionID != "" && !uuidPattern.MatchString(r.SubscriptionID) || (r.Protocol != "tcp" && r.Protocol != "udp") || (r.Direction != "original" && r.Direction != "reply") || r.Bytes < 0 || r.Packets < 0 || names[r.Name] {
			return nil, ErrCounters
		}
		names[r.Name] = true
		key := r.RuleID + "/" + r.Protocol + "/" + r.Direction
		if tuples[key] {
			return nil, ErrCounters
		}
		tuples[key] = true
		g := byRule[r.RuleID]
		if g == nil {
			g = &group{RuleID: r.RuleID, SubscriptionID: r.SubscriptionID}
			byRule[r.RuleID] = g
		}
		if g.SubscriptionID != r.SubscriptionID {
			return nil, ErrCounters
		}
		if r.Direction == "original" {
			if g.UploadBytes > math.MaxInt64-r.Bytes {
				return nil, ErrCounters
			}
			g.UploadBytes += r.Bytes
		} else {
			if g.DownloadBytes > math.MaxInt64-r.Bytes {
				return nil, ErrCounters
			}
			g.DownloadBytes += r.Bytes
		}
		g.Readings = append(g.Readings, r)
	}
	result := make([]group, 0, len(byRule))
	for _, g := range byRule {
		if !bytePair(g.UploadBytes, g.DownloadBytes) {
			return nil, ErrCounters
		}
		for _, r := range g.Readings {
			opposite := "original"
			if r.Direction == "original" {
				opposite = "reply"
			}
			if !tuples[r.RuleID+"/"+r.Protocol+"/"+opposite] {
				return nil, ErrCounters
			}
		}
		sort.Slice(g.Readings, func(i, j int) bool { return g.Readings[i].Name < g.Readings[j].Name })
		result = append(result, *g)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].RuleID < result[j].RuleID })
	return result, nil
}

// ParseCounters consumes a bounded `nft -j list counters` response. It selects
// exactly family ip/table reladock, ignores other tables, and rejects missing,
// duplicate or unexpected owned counters. Every expected TCP/UDP direction must
// be present; original is upload and reply is download. No floats or truncation.
func ParseCounters(raw []byte, expected []Counter) ([]Reading, error) {
	fail := func(e error) ([]Reading, error) { return nil, fmt.Errorf("%w: %v", ErrCounters, e) }
	if len(raw) > MaxCounterJSONBytes || len(expected) > MaxCounters {
		return fail(errors.New("size limit"))
	}
	if e := unambiguousJSON(raw); e != nil {
		return fail(e)
	}
	manifest := make([]Reading, len(expected))
	wanted := map[string]Counter{}
	for i, c := range expected {
		manifest[i] = Reading{Counter: c}
		if _, ok := wanted[c.Name]; ok {
			return fail(errors.New("duplicate manifest"))
		}
		wanted[c.Name] = c
	}
	if _, e := groupReadings(manifest); e != nil {
		return nil, e
	}
	var doc struct {
		NFTables []struct {
			Counter json.RawMessage `json:"counter"`
		} `json:"nftables"`
	}
	if e := json.Unmarshal(raw, &doc); e != nil || doc.NFTables == nil {
		return fail(errors.New("invalid nft JSON"))
	}
	found := map[string]bool{}
	result := make([]Reading, 0, len(expected))
	for _, item := range doc.NFTables {
		if len(item.Counter) == 0 {
			continue
		}
		var c struct {
			Family  string          `json:"family"`
			Table   string          `json:"table"`
			Name    string          `json:"name"`
			Bytes   json.RawMessage `json:"bytes"`
			Packets json.RawMessage `json:"packets"`
		}
		if e := json.Unmarshal(item.Counter, &c); e != nil {
			return fail(e)
		}
		if c.Family != "ip" || c.Table != "reladock" {
			continue
		}
		spec, ok := wanted[c.Name]
		if !ok || found[c.Name] {
			return fail(errors.New("unexpected/duplicate owned counter"))
		}
		found[c.Name] = true
		b, e := nonnegativeInteger(c.Bytes)
		if e != nil {
			return fail(e)
		}
		p, e := nonnegativeInteger(c.Packets)
		if e != nil {
			return fail(e)
		}
		result = append(result, Reading{Counter: spec, Bytes: b, Packets: p})
	}
	if len(found) != len(wanted) {
		return fail(errors.New("missing business counter"))
	}
	grouped, e := groupReadings(result)
	if e != nil {
		return nil, e
	}
	result = result[:0]
	for _, g := range grouped {
		result = append(result, g.Readings...)
	}
	return result, nil
}

// encoding/json normally accepts duplicate fields by taking the last value.
// Counter snapshots must not hide a negative/missing count behind another key.
func unambiguousJSON(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > 64 {
			return ErrCounters
		}
		t, e := d.Token()
		if e != nil {
			return e
		}
		delim, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			keys := map[string]bool{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return e
				}
				name, ok := k.(string)
				if !ok || keys[name] {
					return ErrCounters
				}
				keys[name] = true
				if e = value(depth + 1); e != nil {
					return e
				}
			}
		case '[':
			for d.More() {
				if e = value(depth + 1); e != nil {
					return e
				}
			}
		default:
			return ErrCounters
		}
		end, e := d.Token()
		if e != nil {
			return e
		}
		if delim == '{' && end != json.Delim('}') || delim == '[' && end != json.Delim(']') {
			return ErrCounters
		}
		return nil
	}
	if e := value(0); e != nil {
		return e
	}
	if _, e := d.Token(); e != io.EOF {
		return ErrCounters
	}
	return nil
}

func nonnegativeInteger(raw []byte) (int64, error) {
	if len(raw) == 0 || raw[0] < '0' || raw[0] > '9' {
		return 0, ErrCounters
	}
	n, e := strconv.ParseInt(string(raw), 10, 64)
	if e != nil || n < 0 {
		return 0, ErrCounters
	}
	return n, nil
}
