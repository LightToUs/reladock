package metering

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sync"
)

// Bounds include cursor metadata and every unacknowledged payload. Limits are
// persisted; reopening with different limits is rejected. No event is evicted.
type Options struct {
	NodeID                 string
	MaxBytes               int64
	MaxPending, MaxStreams int
}
type Report struct {
	RuleID            string `json:"ruleId"`
	SubscriptionID    string `json:"subscriptionId,omitempty"`
	Epoch             string `json:"epoch"`
	Sequence          int64  `json:"sequence"`
	UploadBytes       int64  `json:"uploadBytes"`
	DownloadBytes     int64  `json:"downloadBytes"`
	ConfigGeneration  int64  `json:"configGeneration,omitempty"`
	DevelopmentBypass bool   `json:"developmentBypass,omitempty"`
}
type Receipt struct {
	RuleID         string `json:"ruleId"`
	SubscriptionID string `json:"subscriptionId,omitempty"`
	Epoch          string `json:"epoch"`
	Sequence       int64  `json:"sequence"`
}
type Record struct {
	Report  Report
	Payload []byte
}

// Sender must return a Receipt only after an authenticated successful durable
// usage ACK. Timeouts, non-2xx, invalid JSON or mismatched server identity are
// errors, never success. This package does not implement a network transport.
type Sender func(context.Context, []byte) (Receipt, error)

type stream struct {
	group
	Epoch    string
	Sequence int64
	Active   bool
}
type periodTotal struct {
	RuleID, SubscriptionID     string
	UploadBytes, DownloadBytes int64
	Baseline                   *periodBaseline
}

type periodBaseline struct{ UploadBytes, DownloadBytes int64 }

// Older images cannot reconstruct ACKed/pruned period baselines safely.
const walVersion = 3

type snapshot struct {
	Version            int
	Options            Options
	ID                 string
	EpochNumber        int64
	Epoch, Incarnation string
	Streams            []stream
	Pending            [][]byte
	Totals             []periodTotal
}

type WAL struct {
	mu                sync.Mutex
	flushMu           sync.Mutex
	dir               string
	state             snapshot
	unlock            func() error
	persist           func(context.Context, []byte) error
	closed, uncertain bool
}

func normalized(o Options) (Options, error) {
	if o.MaxBytes == 0 {
		o.MaxBytes = 4 << 20
	}
	if o.MaxPending == 0 {
		o.MaxPending = 1024
	}
	if o.MaxStreams == 0 {
		o.MaxStreams = 4096
	}
	if !nodeName.MatchString(o.NodeID) || o.MaxBytes < 4096 || o.MaxBytes > 64<<20 || o.MaxPending < 1 || o.MaxPending > 100000 || o.MaxStreams < 1 || o.MaxStreams > 100000 {
		return o, errors.New("invalid metering options")
	}
	return o, nil
}

func (w *WAL) ready() error {
	if w.closed {
		return ErrClosed
	}
	if w.uncertain {
		return ErrUncertain
	}
	return nil
}
func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	return w.unlock()
}

// BeginEpoch is an explicit confirmation that the physical counters were reset.
// First readings count from zero, not from an inferred baseline. Call ONLY after
// actual replacement/reset is known, under the same external dataplane lock.
func (w *WAL) BeginEpoch(ctx context.Context, readings []Reading) (string, error) {
	return w.begin(ctx, "", readings)
}

// BeginEpochFor makes retries of one physical incarnation idempotent: if its
// token is still current, read/sample it instead of allocating a new epoch.
// Tokens must identify actual counter rebuilds, including rollback rebuilds, not
// a config generation which can be reused during recovery. Noncurrent tokens
// must never be reused. The caller owns verification of the kernel incarnation.
func (w *WAL) BeginEpochFor(ctx context.Context, incarnation string, readings []Reading) (string, error) {
	if !incarnationName.MatchString(incarnation) {
		return "", ErrEpochRequired
	}
	return w.begin(ctx, incarnation, readings)
}

func (w *WAL) begin(ctx context.Context, incarnation string, readings []Reading) (string, error) {
	grouped, e := groupReadings(readings)
	if e != nil {
		return "", e
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if e = w.ready(); e != nil {
		return "", e
	}
	if e = ctx.Err(); e != nil {
		return "", e
	}
	if incarnation != "" && incarnation == w.state.Incarnation {
		return w.state.Epoch, w.sample(ctx, grouped)
	}
	if w.state.EpochNumber == math.MaxInt64 {
		return "", ErrFull
	}
	next := clone(w.state)
	next.EpochNumber++
	next.Epoch = fmt.Sprintf("wal-%s-%x", next.ID, next.EpochNumber)
	next.Incarnation = incarnation
	for i := range next.Streams {
		next.Streams[i].Active = false
	}
	prune(&next)
	index := cumulativeIndex(next)
	for _, g := range grouped {
		if e = addCumulative(&next, index, g.RuleID, g.SubscriptionID, g.UploadBytes, g.DownloadBytes); e != nil {
			return "", e
		}
		s := stream{group: g, Epoch: next.Epoch, Sequence: 1, Active: true}
		next.Streams = append(next.Streams, s)
		if e = appendReport(&next, s); e != nil {
			return "", e
		}
	}
	if e = w.commit(ctx, next); e != nil {
		return "", e
	}
	return next.Epoch, nil
}

// Sample requires exactly the current manifest and period bindings. Individual
// protocol/direction counters (including packets) must never decrease; aggregate
// growth cannot hide a reset. Successful return means the report is fsynced.
func (w *WAL) Sample(ctx context.Context, readings []Reading) error {
	grouped, e := groupReadings(readings)
	if e != nil {
		return e
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if e = w.ready(); e != nil {
		return e
	}
	return w.sample(ctx, grouped)
}

func (w *WAL) sample(ctx context.Context, grouped []group) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	if w.state.Epoch == "" {
		return ErrEpochRequired
	}
	next := clone(w.state)
	index := cumulativeIndex(next)
	active := []int{}
	for i, s := range next.Streams {
		if s.Active {
			active = append(active, i)
		}
	}
	if len(active) != len(grouped) {
		return ErrEpochRequired
	}
	changed := false
	for j, i := range active {
		previous := next.Streams[i]
		g := grouped[j]
		if previous.RuleID != g.RuleID || previous.SubscriptionID != g.SubscriptionID || len(previous.Readings) != len(g.Readings) {
			return ErrEpochRequired
		}
		for k, r := range g.Readings {
			old := previous.Readings[k]
			if old.Counter != r.Counter {
				return ErrEpochRequired
			}
			if r.Bytes < old.Bytes || r.Packets < old.Packets {
				return ErrCounterReset
			}
		}
		if reflect.DeepEqual(previous.group, g) {
			continue
		}
		changed = true
		next.Streams[i].group = g
		if g.UploadBytes != previous.UploadBytes || g.DownloadBytes != previous.DownloadBytes {
			if e := addCumulative(&next, index, g.RuleID, g.SubscriptionID, g.UploadBytes-previous.UploadBytes, g.DownloadBytes-previous.DownloadBytes); e != nil {
				return e
			}
			if previous.Sequence == math.MaxInt64 {
				return ErrFull
			}
			next.Streams[i].Sequence++
			if e := appendReport(&next, next.Streams[i]); e != nil {
				return e
			}
		}
	}
	if !changed {
		return nil
	}
	return w.commit(ctx, next)
}

func appendReport(s *snapshot, c stream) error {
	p, e := json.Marshal(Report{RuleID: c.RuleID, SubscriptionID: c.SubscriptionID, Epoch: c.Epoch, Sequence: c.Sequence, UploadBytes: c.UploadBytes, DownloadBytes: c.DownloadBytes, ConfigGeneration: c.ConfigGeneration, DevelopmentBypass: c.DevelopmentBypass})
	if e != nil {
		return e
	}
	s.Pending = append(s.Pending, p)
	return nil
}

func clone(s snapshot) snapshot {
	next := s
	next.Streams = append([]stream(nil), s.Streams...)
	next.Pending = append([][]byte(nil), s.Pending...)
	next.Totals = append([]periodTotal(nil), s.Totals...)
	return next
}

func cumulativeIndex(s snapshot) map[string]int {
	index := make(map[string]int, len(s.Totals))
	for i, p := range s.Totals {
		index[p.RuleID+"/"+p.SubscriptionID] = i
	}
	return index
}

func addCumulative(s *snapshot, index map[string]int, rule, period string, up, down int64) error {
	key := rule + "/" + period
	i, ok := index[key]
	if !ok {
		if len(s.Totals) >= s.Options.MaxStreams {
			return ErrFull
		}
		i = len(s.Totals)
		s.Totals = append(s.Totals, periodTotal{RuleID: rule, SubscriptionID: period, Baseline: &periodBaseline{}})
		index[key] = i
	}
	p := &s.Totals[i]
	if !bytePair(up, down) || p.UploadBytes > math.MaxInt64-up || p.DownloadBytes > math.MaxInt64-down {
		return ErrFull
	}
	candidate := *p
	candidate.UploadBytes += up
	candidate.DownloadBytes += down
	if _, _, ok := effectiveUsage(candidate); !ok {
		return ErrFull
	}
	p.UploadBytes += up
	p.DownloadBytes += down
	return nil
}

func effectiveUsage(p periodTotal) (int64, int64, bool) {
	if p.Baseline == nil || !bytePair(p.UploadBytes, p.DownloadBytes) || !bytePair(p.Baseline.UploadBytes, p.Baseline.DownloadBytes) {
		return 0, 0, false
	}
	if p.Baseline.UploadBytes > math.MaxInt64-p.UploadBytes || p.Baseline.DownloadBytes > math.MaxInt64-p.DownloadBytes {
		return 0, 0, false
	}
	up, down := p.Baseline.UploadBytes+p.UploadBytes, p.Baseline.DownloadBytes+p.DownloadBytes
	return up, down, bytePair(up, down)
}

// BindPeriod fixes the first authoritative controller baseline for a period.
// Later valid snapshot values are deliberately ignored, because they may already
// include locally ACKed usage. Production callers MUST bind before BeginEpoch.
// For compatibility, a period first seen by BeginEpoch has an explicit fixed zero
// baseline; binding it later cannot add historical usage. Report payloads never
// contain the baseline. There is no automatic migration for missing baselines.
func (w *WAL) BindPeriod(ctx context.Context, ruleID, subscriptionID string, baselineUpload, baselineDownload int64) error {
	if !uuidPattern.MatchString(ruleID) || subscriptionID != "" && !uuidPattern.MatchString(subscriptionID) || !bytePair(baselineUpload, baselineDownload) {
		return ErrCounters
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if e := w.ready(); e != nil {
		return e
	}
	if e := ctx.Err(); e != nil {
		return e
	}
	for _, p := range w.state.Totals {
		if p.RuleID == ruleID && p.SubscriptionID == subscriptionID {
			if p.Baseline == nil {
				return ErrCorrupt
			}
			return nil
		}
	}
	if len(w.state.Totals) >= w.state.Options.MaxStreams {
		return ErrFull
	}
	next := clone(w.state)
	next.Totals = append(next.Totals, periodTotal{RuleID: ruleID, SubscriptionID: subscriptionID, Baseline: &periodBaseline{UploadBytes: baselineUpload, DownloadBytes: baselineDownload}})
	return w.commit(ctx, next)
}

// CumulativeFor returns fixed controller baseline + durable local sampled bytes
// for one rule/subscription across EVERY physical epoch. ACK/pruning never erase
// these totals. A never-recorded valid pair returns zero. These are effective
// usage facts for offline quota rebuilding, not another control-plane bill.
// Period totals are retained conservatively, with at most MaxStreams entries;
// no automatic eviction or implicit cross-period sharing is permitted.
func (w *WAL) CumulativeFor(ctx context.Context, ruleID, subscriptionID string) (int64, int64, error) {
	if !uuidPattern.MatchString(ruleID) || subscriptionID != "" && !uuidPattern.MatchString(subscriptionID) {
		return 0, 0, ErrCounters
	}
	if e := ctx.Err(); e != nil {
		return 0, 0, e
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if e := w.ready(); e != nil {
		return 0, 0, e
	}
	for _, p := range w.state.Totals {
		if p.RuleID == ruleID && p.SubscriptionID == subscriptionID {
			up, down, ok := effectiveUsage(p)
			if !ok {
				return 0, 0, ErrCorrupt
			}
			return up, down, nil
		}
	}
	return 0, 0, nil
}
func prune(s *snapshot) {
	pending := map[string]bool{}
	for _, p := range s.Pending {
		var r Report
		_ = json.Unmarshal(p, &r)
		pending[r.RuleID+"/"+r.Epoch] = true
	}
	kept := make([]stream, 0, len(s.Streams))
	for _, c := range s.Streams {
		if c.Active || pending[c.RuleID+"/"+c.Epoch] {
			kept = append(kept, c)
		}
	}
	s.Streams = kept
}

func (w *WAL) Next() (Record, bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if e := w.ready(); e != nil {
		return Record{}, false, e
	}
	if len(w.state.Pending) == 0 {
		return Record{}, false, nil
	}
	p := append([]byte(nil), w.state.Pending[0]...)
	var r Report
	if e := json.Unmarshal(p, &r); e != nil {
		return Record{}, false, ErrCorrupt
	}
	return Record{Report: r, Payload: p}, true, nil
}

// Ack removes ONLY the queue head, after the caller received its network ACK.
// Cursors/sequence survive compaction; old epochs retire only after all ACKs.
func (w *WAL) Ack(ctx context.Context, r Receipt) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if e := w.ready(); e != nil {
		return e
	}
	if e := ctx.Err(); e != nil {
		return e
	}
	if len(w.state.Pending) == 0 {
		return ErrAck
	}
	var head Report
	if e := json.Unmarshal(w.state.Pending[0], &head); e != nil {
		return ErrCorrupt
	}
	if r.RuleID != head.RuleID || r.SubscriptionID != head.SubscriptionID || r.Epoch != head.Epoch || r.Sequence != head.Sequence {
		return ErrAck
	}
	next := clone(w.state)
	next.Pending = next.Pending[1:]
	prune(&next)
	return w.commit(ctx, next)
}

func (w *WAL) Flush(ctx context.Context, send Sender) error {
	if send == nil {
		return errors.New("usage sender required")
	}
	w.flushMu.Lock()
	defer w.flushMu.Unlock()
	for {
		if e := ctx.Err(); e != nil {
			return e
		}
		r, ok, e := w.Next()
		if e != nil || !ok {
			return e
		}
		ack, e := send(ctx, r.Payload)
		if e != nil {
			return e
		}
		if e = w.Ack(ctx, ack); e != nil {
			return e
		}
	}
}

// Identity returns the durable WAL identity for binding to dataplane state.
// Reopening the same WAL preserves it; a new WAL directory has a new identity.
func (w *WAL) Identity() (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.ready(); err != nil {
		return "", err
	}
	return w.state.ID, nil
}

// Current returns the durable epoch/incarnation, including after process restart.
// A caller must still verify that it describes the actual kernel counter set.
func (w *WAL) Current() (epoch, incarnation string, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.state.Epoch, w.state.Incarnation, w.ready()
}

func (w *WAL) commit(ctx context.Context, next snapshot) error {
	if len(next.Pending) > next.Options.MaxPending || len(next.Streams) > next.Options.MaxStreams || len(next.Totals) > next.Options.MaxStreams {
		return ErrFull
	}
	data, e := encodeSnapshot(next)
	if e != nil {
		return e
	}
	if int64(len(data)) > next.Options.MaxBytes {
		return ErrFull
	}
	if e = ctx.Err(); e != nil {
		return e
	}
	if e = w.persist(ctx, data); e != nil {
		w.uncertain = true
		return errors.Join(ErrUncertain, e)
	}
	w.state = next
	return nil
}

func randomID() (string, error) {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		return "", e
	}
	return hex.EncodeToString(b[:]), nil
}
