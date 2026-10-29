package agent

import (
	"context"
	"errors"
	"reladock/internal/dataplane"
	"reladock/internal/metering"
	"reladock/internal/platform"
	"time"
)

type NodeEngine struct {
	Adapter     *dataplane.Adapter
	WAL         *metering.WAL
	Meter       dataplane.MeterHooks
	probeCursor int
	lastProbe   time.Time
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
	return (VerifiedApplier{Adapter: e.Adapter}).Apply(ctx, cfg)
}
func (e *NodeEngine) Tick(ctx context.Context, c *Client) error {
	// Flushing before sampling relieves bounded WAL backpressure. A network
	// failure is retained while local sampling still protects quota/counter facts.
	sendErr := c.FlushUsage(ctx, e.WAL)
	sampleErr := e.Adapter.Observe(ctx, e.Meter.Sample)
	var probeErr error
	if sampleErr == nil && time.Since(e.lastProbe) >= 30*time.Second {
		e.lastProbe = time.Now()
		probeErr = e.ProbeCycle(ctx, c)
	}
	return errors.Join(sendErr, sampleErr, probeErr)
}
func (e *NodeEngine) Close() error {
	if e.WAL != nil {
		return e.WAL.Close()
	}
	return nil
}
