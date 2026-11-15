package sourceflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/netip"
	"reflect"
	"strconv"
	"sync"
	"time"
)

var (
	ErrFull      = errors.New("source-flow WAL capacity exhausted")
	ErrAck       = errors.New("source-flow ACK does not match oldest durable batch")
	ErrGap       = errors.New("invalid source-flow collection gap")
	ErrClosed    = errors.New("source-flow WAL is closed")
	ErrUncertain = errors.New("source-flow persistence failed: close and reopen before continuing")
	ErrLocked    = errors.New("source-flow WAL already open by another owner")
	ErrCorrupt   = errors.New("invalid source-flow WAL: manual reconciliation required")
)

var gapReasons = map[string]bool{
	"netlink-overrun": true, "collector-restart": true, "collector-error": true,
	"queue-full": true, "wal-full": true, "wal-unavailable": true,
}

type Options struct {
	NodeID            string
	MaxBytes          int64
	MaxEvents         int
	MaxGaps           int
	GapReserveBytes   int64
	MaxBatch          int
	CollectionEnabled bool `json:"-"`
}

type storedOptions struct {
	NodeID          string
	MaxBytes        int64
	MaxEvents       int
	MaxGaps         int
	GapReserveBytes int64
	MaxBatch        int
}

func normalizedOptions(options Options) (Options, storedOptions, error) {
	if options.MaxBytes == 0 {
		options.MaxBytes = 16 << 20
	}
	if options.MaxEvents == 0 {
		options.MaxEvents = 50000
	}
	if options.MaxGaps == 0 {
		options.MaxGaps = 1024
	}
	if options.GapReserveBytes == 0 {
		options.GapReserveBytes = 64 << 10
	}
	if options.MaxBatch == 0 {
		options.MaxBatch = 256
	}
	if !nodeIDPattern.MatchString(options.NodeID) || options.MaxBytes < 8192 || options.MaxBytes > 256<<20 || options.MaxEvents < 1 || options.MaxEvents > 1000000 || options.MaxGaps < 1 || options.MaxGaps > 100000 || options.GapReserveBytes < 1024 || options.GapReserveBytes >= options.MaxBytes/2 || options.MaxBatch < 1 || options.MaxBatch > 4096 {
		return Options{}, storedOptions{}, errors.New("invalid source-flow WAL options")
	}
	stored := storedOptions{NodeID: options.NodeID, MaxBytes: options.MaxBytes, MaxEvents: options.MaxEvents, MaxGaps: options.MaxGaps, GapReserveBytes: options.GapReserveBytes, MaxBatch: options.MaxBatch}
	return options, stored, nil
}

type Event struct {
	EventID          string    `json:"eventId"`
	TenantID         string    `json:"tenantId"`
	RuleID           string    `json:"ruleId"`
	NodeID           string    `json:"nodeId"`
	AllocationID     string    `json:"allocationId"`
	ConfigGeneration int64     `json:"configGeneration"`
	RuleVersion      int64     `json:"ruleVersion"`
	Protocol         string    `json:"protocol"`
	SourceIP         string    `json:"sourceIp"`
	SourcePort       int       `json:"sourcePort"`
	RelayIP          string    `json:"relayIp"`
	RelayPort        int       `json:"relayPort"`
	TargetIP         string    `json:"targetIp"`
	TargetPort       int       `json:"targetPort"`
	ObservedAt       time.Time `json:"observedAt"`
	KernelBootID     string    `json:"kernelBootId"`
	ConntrackZone    uint16    `json:"conntrackZone"`
	ConntrackID      uint32    `json:"conntrackId"`
}

type Gap struct {
	NodeID         string     `json:"nodeId"`
	CollectorEpoch string     `json:"collectorEpoch"`
	Sequence       int64      `json:"sequence"`
	Reason         string     `json:"reason"`
	DroppedCount   int64      `json:"droppedCount"`
	OpenedAt       time.Time  `json:"openedAt"`
	ClosedAt       *time.Time `json:"closedAt,omitempty"`
}

type GapReceipt struct {
	CollectorEpoch string     `json:"collectorEpoch"`
	Sequence       int64      `json:"sequence"`
	DroppedCount   int64      `json:"droppedCount"`
	ClosedAt       *time.Time `json:"closedAt,omitempty"`
}

type Receipt struct {
	EventIDs []string     `json:"eventIds"`
	Gaps     []GapReceipt `json:"gaps"`
}

type Batch struct {
	NodeID string  `json:"nodeId"`
	Events []Event `json:"events"`
	Gaps   []Gap   `json:"gaps"`
}

func (b Batch) Receipt() Receipt {
	receipt := Receipt{EventIDs: make([]string, len(b.Events)), Gaps: make([]GapReceipt, len(b.Gaps))}
	for i, event := range b.Events {
		receipt.EventIDs[i] = event.EventID
	}
	for i, gap := range b.Gaps {
		receipt.Gaps[i] = gapReceipt(gap)
	}
	return receipt
}

type Status struct {
	ID                string
	NodeID            string
	CollectorEpoch    string
	Sequence          int64
	PendingEvents     int
	PendingGaps       int
	OpenGap           bool
	DroppedTotal      int64
	BytesUsed         int64
	MaxBytes          int64
	OldestObservedAt  *time.Time
	CollectionEnabled bool
}

const sourceWALVersion = 1

type walSnapshot struct {
	Version           int
	Options           storedOptions
	ID                string
	CollectorEpoch    string
	Sequence          int64
	Events            []Event
	Gaps              []Gap
	LastReceipt       Receipt
	AcknowledgedGaps  []GapReceipt
	DroppedTotal      int64
	CollectionEnabled bool
	CleanShutdown     bool
	CheckpointAt      time.Time
}

type WAL struct {
	mu                sync.Mutex
	dir               string
	options           Options
	state             walSnapshot
	unlock            func() error
	persist           func(context.Context, []byte) error
	closed, uncertain bool
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

func validEvent(event Event, nodeID string) bool {
	source, sourceErr := netip.ParseAddr(event.SourceIP)
	relay, relayErr := netip.ParseAddr(event.RelayIP)
	target, targetErr := netip.ParseAddr(event.TargetIP)
	return uuidPattern.MatchString(event.EventID) && uuidPattern.MatchString(event.TenantID) && uuidPattern.MatchString(event.RuleID) && uuidPattern.MatchString(event.AllocationID) && event.NodeID == nodeID && event.ConfigGeneration > 0 && event.RuleVersion > 0 && (event.Protocol == "tcp" || event.Protocol == "udp") && sourceErr == nil && source.IsValid() && !source.IsUnspecified() && relayErr == nil && relay.Is4() && relay.IsGlobalUnicast() && !relay.IsLoopback() && targetErr == nil && target.Is4() && target.IsGlobalUnicast() && !target.IsLoopback() && event.SourcePort > 0 && event.SourcePort <= 65535 && event.RelayPort > 0 && event.RelayPort <= 65535 && event.TargetPort > 0 && event.TargetPort <= 65535 && !event.ObservedAt.IsZero() && uuidPattern.MatchString(event.KernelBootID)
}

func ValidateEvent(event Event, nodeID string) error {
	copy := event
	copy.ObservedAt = copy.ObservedAt.UTC().Truncate(time.Microsecond)
	if !validEvent(copy, nodeID) {
		return errors.New("invalid source-flow event")
	}
	return nil
}

func ValidateGap(gap Gap, nodeID string) error {
	if gap.NodeID != nodeID || !uuidPattern.MatchString(gap.CollectorEpoch) || gap.Sequence < 1 || !gapReasons[gap.Reason] || gap.DroppedCount < 0 || gap.OpenedAt.IsZero() || gap.ClosedAt != nil && gap.ClosedAt.Before(gap.OpenedAt) {
		return ErrGap
	}
	return nil
}

func (w *WAL) AppendEvent(ctx context.Context, event Event) (Event, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.ready(); err != nil {
		return Event{}, err
	}
	if err := ctx.Err(); err != nil {
		return Event{}, err
	}
	if event.EventID == "" {
		var err error
		event.EventID, err = randomUUID()
		if err != nil {
			return Event{}, err
		}
	}
	event.ObservedAt = event.ObservedAt.UTC().Truncate(time.Microsecond)
	if !validEvent(event, w.options.NodeID) {
		return Event{}, errors.New("invalid source-flow event")
	}
	if len(w.state.Events) >= w.options.MaxEvents {
		return Event{}, ErrFull
	}
	for _, pending := range w.state.Events {
		if pending.EventID == event.EventID {
			if reflect.DeepEqual(pending, event) {
				return pending, nil
			}
			return Event{}, ErrCorrupt
		}
	}
	next := cloneSnapshot(w.state)
	next.Events = append(next.Events, event)
	encoded, err := encodeWALSnapshot(next)
	if err != nil {
		return Event{}, err
	}
	if int64(len(encoded)) > w.options.MaxBytes-w.options.GapReserveBytes {
		return Event{}, ErrFull
	}
	if err = w.commitEncoded(ctx, next, encoded); err != nil {
		return Event{}, err
	}
	return event, nil
}

func (w *WAL) OpenGap(ctx context.Context, reason string, openedAt time.Time, dropped int64) (Gap, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.ready(); err != nil {
		return Gap{}, err
	}
	if err := ctx.Err(); err != nil {
		return Gap{}, err
	}
	openedAt = openedAt.UTC().Truncate(time.Microsecond)
	if !gapReasons[reason] || openedAt.IsZero() || dropped < 0 || dropped == 0 && reason != "collector-restart" {
		return Gap{}, ErrGap
	}
	next := cloneSnapshot(w.state)
	for i := len(next.Gaps) - 1; i >= 0; i-- {
		gap := &next.Gaps[i]
		if gap.ClosedAt == nil && gap.Reason == reason {
			if gap.DroppedCount > int64(^uint64(0)>>1)-dropped {
				return Gap{}, ErrFull
			}
			gap.DroppedCount += dropped
			next.DroppedTotal += dropped
			if err := w.commit(ctx, next); err != nil {
				return Gap{}, err
			}
			return *gap, nil
		}
	}
	if len(next.Gaps) >= w.options.MaxGaps || next.Sequence == int64(^uint64(0)>>1) {
		return Gap{}, ErrFull
	}
	next.Sequence++
	gap := Gap{NodeID: w.options.NodeID, CollectorEpoch: next.CollectorEpoch, Sequence: next.Sequence, Reason: reason, DroppedCount: dropped, OpenedAt: openedAt}
	next.Gaps = append(next.Gaps, gap)
	next.DroppedTotal += dropped
	if err := w.commit(ctx, next); err != nil {
		return Gap{}, err
	}
	return gap, nil
}

func (w *WAL) CloseGap(ctx context.Context, closedAt time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.ready(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	closedAt = closedAt.UTC().Truncate(time.Microsecond)
	if closedAt.IsZero() {
		return ErrGap
	}
	next := cloneSnapshot(w.state)
	changed := false
	for i := range next.Gaps {
		gap := &next.Gaps[i]
		if gap.ClosedAt != nil {
			continue
		}
		if closedAt.Before(gap.OpenedAt) {
			return ErrGap
		}
		value := closedAt
		gap.ClosedAt = &value
		changed = true
	}
	if !changed {
		return nil
	}
	return w.commit(ctx, next)
}

func (w *WAL) SetCollectionEnabled(ctx context.Context, enabled bool) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.ready(); err != nil {
		return err
	}
	if w.state.CollectionEnabled == enabled {
		return nil
	}
	next := cloneSnapshot(w.state)
	next.CollectionEnabled = enabled
	w.options.CollectionEnabled = enabled
	return w.commit(ctx, next)
}

func (w *WAL) NextBatch() (Batch, bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.ready(); err != nil {
		return Batch{}, false, err
	}
	batch := w.nextBatchLocked()
	return batch, len(batch.Events)+len(batch.Gaps) > 0, nil
}

func (w *WAL) nextBatchLocked() Batch {
	batch := Batch{NodeID: w.options.NodeID, Events: []Event{}, Gaps: []Gap{}}
	remaining := w.options.MaxBatch
	for _, gap := range w.state.Gaps {
		if remaining == 0 {
			break
		}
		if gapReceiptAcknowledged(w.state.AcknowledgedGaps, gapReceipt(gap)) {
			continue
		}
		batch.Gaps = append(batch.Gaps, gap)
		remaining--
	}
	if remaining > len(w.state.Events) {
		remaining = len(w.state.Events)
	}
	batch.Events = append(batch.Events, w.state.Events[:remaining]...)
	return batch
}

func (w *WAL) Ack(ctx context.Context, receipt Receipt) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.ready(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if reflect.DeepEqual(receipt, w.state.LastReceipt) && len(receipt.EventIDs)+len(receipt.Gaps) > 0 {
		return nil
	}
	if len(receipt.EventIDs)+len(receipt.Gaps) < 1 || len(receipt.EventIDs)+len(receipt.Gaps) > w.options.MaxBatch {
		return ErrAck
	}
	expected := w.nextBatchLocked()
	if !reflect.DeepEqual(receipt, expected.Receipt()) {
		return ErrAck
	}
	next := cloneSnapshot(w.state)
	next.Events = append([]Event(nil), next.Events[len(receipt.EventIDs):]...)
	acknowledged := make(map[string]GapReceipt, len(next.AcknowledgedGaps)+len(receipt.Gaps))
	for _, ack := range next.AcknowledgedGaps {
		acknowledged[gapReceiptKey(ack)] = ack
	}
	removeGaps := make(map[string]bool, len(receipt.Gaps))
	for _, ack := range receipt.Gaps {
		key := gapReceiptKey(ack)
		if ack.ClosedAt != nil {
			removeGaps[key] = true
			delete(acknowledged, key)
		} else {
			acknowledged[key] = ack
		}
	}
	kept := next.Gaps[:0]
	for _, gap := range next.Gaps {
		if !removeGaps[gapReceiptKey(gapReceipt(gap))] {
			kept = append(kept, gap)
		}
	}
	next.Gaps = append([]Gap(nil), kept...)
	next.AcknowledgedGaps = next.AcknowledgedGaps[:0]
	for _, gap := range next.Gaps {
		if ack, ok := acknowledged[gapReceiptKey(gapReceipt(gap))]; ok {
			next.AcknowledgedGaps = append(next.AcknowledgedGaps, ack)
		}
	}
	next.LastReceipt = cloneReceipt(receipt)
	return w.commit(ctx, next)
}

func (w *WAL) Status() (Status, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.ready(); err != nil {
		return Status{}, err
	}
	encoded, err := encodeWALSnapshot(w.state)
	if err != nil {
		return Status{}, err
	}
	status := Status{ID: w.state.ID, NodeID: w.options.NodeID, CollectorEpoch: w.state.CollectorEpoch, Sequence: w.state.Sequence, PendingEvents: len(w.state.Events), PendingGaps: len(w.state.Gaps), DroppedTotal: w.state.DroppedTotal, BytesUsed: int64(len(encoded)), MaxBytes: w.options.MaxBytes, CollectionEnabled: w.state.CollectionEnabled}
	var oldest *time.Time
	for _, gap := range w.state.Gaps {
		if gap.ClosedAt == nil {
			status.OpenGap = true
		}
		if oldest == nil || gap.OpenedAt.Before(*oldest) {
			value := gap.OpenedAt
			oldest = &value
		}
	}
	if len(w.state.Events) > 0 {
		eventOldest := w.state.Events[0].ObservedAt
		for _, event := range w.state.Events[1:] {
			if event.ObservedAt.Before(eventOldest) {
				eventOldest = event.ObservedAt
			}
		}
		if oldest == nil || eventOldest.Before(*oldest) {
			oldest = &eventOldest
		}
	}
	status.OldestObservedAt = oldest
	return status, nil
}

func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	var persistErr error
	if !w.uncertain {
		next := cloneSnapshot(w.state)
		next.CleanShutdown = true
		persistErr = w.commit(context.Background(), next)
	}
	w.closed = true
	return errors.Join(persistErr, w.unlock())
}

func cloneReceipt(receipt Receipt) Receipt {
	cloned := Receipt{EventIDs: append([]string(nil), receipt.EventIDs...), Gaps: append([]GapReceipt(nil), receipt.Gaps...)}
	for i := range cloned.Gaps {
		if receipt.Gaps[i].ClosedAt != nil {
			value := *receipt.Gaps[i].ClosedAt
			cloned.Gaps[i].ClosedAt = &value
		}
	}
	return cloned
}

func cloneSnapshot(state walSnapshot) walSnapshot {
	next := state
	next.Events = append([]Event(nil), state.Events...)
	next.Gaps = append([]Gap(nil), state.Gaps...)
	for i := range next.Gaps {
		if state.Gaps[i].ClosedAt != nil {
			value := *state.Gaps[i].ClosedAt
			next.Gaps[i].ClosedAt = &value
		}
	}
	next.LastReceipt = cloneReceipt(state.LastReceipt)
	next.AcknowledgedGaps = cloneReceipt(Receipt{Gaps: state.AcknowledgedGaps}).Gaps
	return next
}

func gapReceipt(gap Gap) GapReceipt {
	receipt := GapReceipt{CollectorEpoch: gap.CollectorEpoch, Sequence: gap.Sequence, DroppedCount: gap.DroppedCount}
	if gap.ClosedAt != nil {
		value := *gap.ClosedAt
		receipt.ClosedAt = &value
	}
	return receipt
}

func gapReceiptKey(receipt GapReceipt) string {
	return receipt.CollectorEpoch + "/" + strconv.FormatInt(receipt.Sequence, 10)
}

func gapReceiptAcknowledged(receipts []GapReceipt, receipt GapReceipt) bool {
	for _, acknowledged := range receipts {
		if reflect.DeepEqual(acknowledged, receipt) {
			return true
		}
	}
	return false
}

func randomBytes(size int) ([]byte, error) {
	value := make([]byte, size)
	_, err := rand.Read(value)
	return value, err
}

func randomUUID() (string, error) {
	value, err := randomBytes(16)
	if err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	hexValue := hex.EncodeToString(value)
	return hexValue[:8] + "-" + hexValue[8:12] + "-" + hexValue[12:16] + "-" + hexValue[16:20] + "-" + hexValue[20:], nil
}

func validateSnapshotState(state walSnapshot, options storedOptions) error {
	id, err := hex.DecodeString(state.ID)
	if err != nil || len(id) != 16 || state.ID != hex.EncodeToString(id) || state.Version != sourceWALVersion || state.Options != options || !uuidPattern.MatchString(state.CollectorEpoch) || state.Sequence < 0 || state.DroppedTotal < 0 || state.CheckpointAt.IsZero() || len(state.Events) > options.MaxEvents || len(state.Gaps) > options.MaxGaps || len(state.AcknowledgedGaps) > options.MaxGaps || len(state.LastReceipt.EventIDs)+len(state.LastReceipt.Gaps) > options.MaxBatch {
		return ErrCorrupt
	}
	eventIDs := map[string]bool{}
	for _, event := range state.Events {
		if !validEvent(event, options.NodeID) || eventIDs[event.EventID] {
			return ErrCorrupt
		}
		eventIDs[event.EventID] = true
	}
	gapIDs := map[string]bool{}
	var dropped int64
	for _, gap := range state.Gaps {
		id := gapReceiptKey(gapReceipt(gap))
		if gap.NodeID != options.NodeID || !uuidPattern.MatchString(gap.CollectorEpoch) || gap.Sequence < 1 || !gapReasons[gap.Reason] || gap.DroppedCount < 0 || gap.OpenedAt.IsZero() || gap.ClosedAt != nil && gap.ClosedAt.Before(gap.OpenedAt) || gapIDs[id] {
			return ErrCorrupt
		}
		gapIDs[id] = true
		if gap.DroppedCount > int64(^uint64(0)>>1)-dropped {
			return ErrCorrupt
		}
		dropped += gap.DroppedCount
	}
	ackIDs := map[string]bool{}
	for _, receipt := range state.AcknowledgedGaps {
		key := gapReceiptKey(receipt)
		if !uuidPattern.MatchString(receipt.CollectorEpoch) || receipt.Sequence < 1 || receipt.DroppedCount < 0 || receipt.ClosedAt != nil || ackIDs[key] || !gapIDs[key] {
			return ErrCorrupt
		}
		ackIDs[key] = true
	}
	if state.DroppedTotal < dropped {
		return ErrCorrupt
	}
	return nil
}
