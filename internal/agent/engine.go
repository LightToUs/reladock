package agent

import (
	"context"
	"errors"
	"reladock/internal/dataplane"
	"reladock/internal/metering"
	"reladock/internal/platform"
	"reladock/internal/sourceflow"
	"sync"
	"time"
)

type NodeEngine struct {
	NodeID                   string
	Adapter                  *dataplane.Adapter
	WAL                      *metering.WAL
	SourceFlowWAL            *sourceflow.WAL
	Meter                    dataplane.MeterHooks
	sourceFlowSource         sourceflow.EventSource
	sourceFlowKernelBootID   string
	probeCursor              int
	manifestMu               sync.RWMutex
	sourceFlowHealthMu       sync.Mutex
	sourceFlowPressureSince  time.Time
	sourceFlowManifest       *sourceflow.Manifest
	sourceFlowManifestDigest string
}

func (e *NodeEngine) Apply(ctx context.Context, cfg platform.NodeConfig) error {
	if e.Adapter == nil {
		return errors.New("composed adapter required")
	}
	if err := dataplane.ValidatePaidConfig(cfg); err != nil {
		stable, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if e.Adapter.Stable(stable) == nil && stable.Err() == nil {
			return &SafeApplyFailure{Err: err}
		}
		return err
	}
	if err := e.Adapter.Recover(ctx); err != nil {
		return err
	}
	plan, err := e.Adapter.ApplyVerified(ctx, cfg)
	if err != nil {
		stable, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if e.Adapter.Stable(stable) == nil && stable.Err() == nil {
			return &SafeApplyFailure{Err: err}
		}
		return err
	}
	e.manifestMu.Lock()
	e.sourceFlowManifest = plan.SourceFlowManifest
	e.sourceFlowManifestDigest = plan.SourceFlowManifestDigest
	e.manifestMu.Unlock()
	return nil
}

func (e *NodeEngine) SourceFlowManifest() (sourceflow.Manifest, string, error) {
	e.manifestMu.RLock()
	defer e.manifestMu.RUnlock()
	if e.sourceFlowManifest == nil || e.sourceFlowManifestDigest == "" {
		return sourceflow.Manifest{}, "", errors.New("source-flow manifest unavailable")
	}
	manifest := *e.sourceFlowManifest
	manifest.Rules = append([]sourceflow.AppliedRule(nil), manifest.Rules...)
	return manifest, e.sourceFlowManifestDigest, nil
}

func (e *NodeEngine) ConfigureSourceFlow(kernelBootID string) error {
	if e.SourceFlowWAL == nil || e.sourceFlowSource == nil {
		return errors.New("source-flow collection is not configured")
	}
	if err := sourceflow.ValidateKernelBootID(kernelBootID); err != nil {
		return err
	}
	e.sourceFlowKernelBootID = kernelBootID
	return nil
}

func (e *NodeEngine) SourceFlowAvailable() bool {
	return e != nil && e.SourceFlowWAL != nil && e.sourceFlowSource != nil && e.sourceFlowKernelBootID != ""
}

func (e *NodeEngine) CaptureSourceFlows(ctx context.Context) error {
	if !e.SourceFlowAvailable() {
		return errors.New("source-flow collection is not configured")
	}
	collector := sourceflow.Collector{Source: e.sourceFlowSource, WAL: e.SourceFlowWAL, Manifest: e, KernelBootID: e.sourceFlowKernelBootID}
	return collector.Run(ctx)
}

func (e *NodeEngine) SourceFlowStatus(now time.Time) (*platform.SourceFlowStatus, error) {
	if !e.SourceFlowAvailable() {
		return nil, nil
	}
	status, err := e.SourceFlowWAL.Status()
	if err != nil {
		return nil, err
	}
	collectorStatus := "normal"
	backlog := int64(status.PendingEvents + status.PendingGaps)
	pressureDegraded := e.sourceFlowPressureDegraded(now, status.BytesUsed, status.MaxBytes)
	if status.OpenGap || !status.CollectionEnabled || pressureDegraded || status.OldestObservedAt != nil && now.Sub(*status.OldestObservedAt) > 90*time.Second {
		collectorStatus = "degraded"
	}
	return &platform.SourceFlowStatus{NodeID: e.NodeID, CollectorStatus: collectorStatus, SubscriptionAlive: status.CollectionEnabled, BacklogCount: backlog, OldestBacklogAt: status.OldestObservedAt, WALBytesUsed: status.BytesUsed, WALBytesLimit: status.MaxBytes, DroppedTotal: status.DroppedTotal, OpenGap: status.OpenGap, CollectorEpoch: status.CollectorEpoch, CollectorSequence: status.Sequence, ObservedAt: now.UTC().Truncate(time.Microsecond)}, nil
}

func (e *NodeEngine) sourceFlowPressureDegraded(now time.Time, used, limit int64) bool {
	e.sourceFlowHealthMu.Lock()
	defer e.sourceFlowHealthMu.Unlock()
	if limit < 1 || used*100 < limit*80 {
		e.sourceFlowPressureSince = time.Time{}
		return false
	}
	if e.sourceFlowPressureSince.IsZero() || now.Before(e.sourceFlowPressureSince) {
		e.sourceFlowPressureSince = now
		return false
	}
	return now.Sub(e.sourceFlowPressureSince) >= 90*time.Second
}

// Stable is a read-only local ownership/configuration check. Success proves
// only that the owned nftables/tc state matches durable local state.
func (e *NodeEngine) Stable(ctx context.Context) error {
	if e == nil || e.Adapter == nil {
		return errors.New("composed adapter required")
	}
	return e.Adapter.Stable(ctx)
}
func (e *NodeEngine) Tick(ctx context.Context, c *Client) error {
	// The one-shot compatibility path samples before network work. Long-running
	// mode schedules these methods independently in the Supervisor.
	sampleErr := e.Sample(ctx)
	sendErr := e.FlushUsage(ctx, c)
	probeErr := e.ProbeCycle(ctx, c)
	return errors.Join(sampleErr, sendErr, probeErr)
}

func (e *NodeEngine) Sample(ctx context.Context) error {
	return e.Adapter.Observe(ctx, e.Meter.Sample)
}

func (e *NodeEngine) FlushUsage(ctx context.Context, c *Client) error {
	return c.FlushUsage(ctx, e.WAL)
}
func (e *NodeEngine) Close() error {
	var usageErr, sourceErr error
	if e.WAL != nil {
		usageErr = e.WAL.Close()
	}
	if e.SourceFlowWAL != nil {
		sourceErr = e.SourceFlowWAL.Close()
	}
	return errors.Join(usageErr, sourceErr)
}
