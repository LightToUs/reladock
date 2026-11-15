package sourceflow

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type fakeEventSource struct {
	events []KernelEvent
	err    error
}

func (s fakeEventSource) Run(ctx context.Context, ready func(), emit func(KernelEvent) bool) error {
	ready()
	for _, event := range s.events {
		emit(event)
	}
	if s.err != nil {
		return s.err
	}
	<-ctx.Done()
	return ctx.Err()
}

type mutableManifest struct {
	mu       sync.RWMutex
	manifest Manifest
	digest   string
}

func (m *mutableManifest) SourceFlowManifest() (Manifest, string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.manifest, m.digest, nil
}

func collectorManifest(t *testing.T, generation int64, relayPort int) *mutableManifest {
	t.Helper()
	manifest, digest, err := NewManifest("hk-01", generation, []AppliedRule{{
		AllocationID: "11111111-1111-4111-8111-111111111111", TenantID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		RuleID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", RuleVersion: generation, SourceLoggingEnabled: true,
		Protocol: "tcp", RelayIP: "203.0.113.10", RelayPort: relayPort, TargetIP: "198.51.100.20", TargetPort: 443,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return &mutableManifest{manifest: manifest, digest: digest}
}

func TestCollectorAttributesOriginalTupleAndKeepsFiveTupleReuseDistinct(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	event := KernelEvent{Protocol: "tcp", SourceIP: "192.0.2.40", SourcePort: 53000, DestinationIP: "203.0.113.10", DestinationPort: 24443, ObservedAt: now, ID: 41}
	source := fakeEventSource{events: []KernelEvent{
		event,
		event, // a second accepted NEW notification gets its own durable event ID
		{Protocol: "tcp", SourceIP: "192.0.2.40", SourcePort: 53000, DestinationIP: "203.0.113.10", DestinationPort: 24443, ObservedAt: now.Add(time.Microsecond), ID: 42},
		{Protocol: "udp", SourceIP: "192.0.2.50", SourcePort: 53001, DestinationIP: "203.0.113.10", DestinationPort: 24443, ObservedAt: now.Add(2 * time.Microsecond), ID: 43},
		{Protocol: "tcp", SourceIP: "192.0.2.50", SourcePort: 53001, DestinationIP: "203.0.113.99", DestinationPort: 24443, ObservedAt: now.Add(3 * time.Microsecond), ID: 44},
	}}
	w := openSourceWAL(t, filepath.Join(t.TempDir(), "source-wal"), Options{NodeID: "hk-01"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	collector := Collector{Source: source, WAL: w, Manifest: collectorManifest(t, 7, 24443), KernelBootID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd"}
	go func() { done <- collector.Run(ctx) }()
	waitSourceEvents(t, w, 3)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	batch, ok, err := w.NextBatch()
	if err != nil || !ok || len(batch.Events) != 3 {
		t.Fatalf("batch=%+v ok=%v err=%v", batch, ok, err)
	}
	if batch.Events[0].SourceIP != "192.0.2.40" || batch.Events[0].SourcePort != 53000 || batch.Events[0].RelayIP != "203.0.113.10" || batch.Events[0].TargetIP != "198.51.100.20" || batch.Events[0].ConntrackID != 41 || batch.Events[1].ConntrackID != 41 || batch.Events[2].ConntrackID != 42 || batch.Events[0].EventID == batch.Events[1].EventID {
		t.Fatalf("attribution/reuse mismatch: %+v", batch.Events)
	}
}

func TestCollectorUsesLatestDurablyPromotedManifest(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	manifest := collectorManifest(t, 8, 24444)
	source := fakeEventSource{events: []KernelEvent{
		{Protocol: "tcp", SourceIP: "192.0.2.40", SourcePort: 53000, DestinationIP: "203.0.113.10", DestinationPort: 24443, ObservedAt: now, ID: 51},
		{Protocol: "tcp", SourceIP: "192.0.2.41", SourcePort: 53001, DestinationIP: "203.0.113.10", DestinationPort: 24444, ObservedAt: now.Add(time.Microsecond), ID: 52},
	}}
	w := openSourceWAL(t, filepath.Join(t.TempDir(), "source-wal"), Options{NodeID: "hk-01"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (&Collector{Source: source, WAL: w, Manifest: manifest, KernelBootID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd"}).Run(ctx)
	}()
	waitSourceEvents(t, w, 1)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	batch, _, _ := w.NextBatch()
	if len(batch.Events) != 1 || batch.Events[0].RelayPort != 24444 || batch.Events[0].ConfigGeneration != 8 {
		t.Fatalf("last-good attribution: %+v", batch.Events)
	}
}

func TestCollectorPersistsNetlinkFailureGapAndCancelsCleanly(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	w := openSourceWAL(t, filepath.Join(t.TempDir(), "source-wal"), Options{NodeID: "hk-01"})
	collector := Collector{Source: fakeEventSource{err: errors.Join(ErrNetlinkOverrun, errors.New("ENOBUFS"))}, WAL: w, Manifest: collectorManifest(t, 7, 24443), KernelBootID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd", Now: func() time.Time { return now }}
	if err := collector.Run(context.Background()); !errors.Is(err, ErrNetlinkOverrun) {
		t.Fatalf("overrun error=%v", err)
	}
	status, err := w.Status()
	if err != nil || !status.OpenGap || status.DroppedTotal != 1 || status.CollectionEnabled {
		t.Fatalf("gap status=%+v err=%v", status, err)
	}
}

func waitSourceEvents(t *testing.T, w *WAL, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		status, err := w.Status()
		if err != nil {
			t.Fatal(err)
		}
		if status.PendingEvents == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	status, _ := w.Status()
	t.Fatalf("pending events=%d want=%d", status.PendingEvents, want)
}
