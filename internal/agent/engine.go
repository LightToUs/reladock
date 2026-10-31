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
	if e.WAL != nil {
		return e.WAL.Close()
	}
	return nil
}
