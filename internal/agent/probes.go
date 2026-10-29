package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reladock/internal/dataplane"
	"reladock/internal/probe"
	"time"
)

// ProbeCycle measures only reachability from this node to pinned customer
// targets. It never marks the forwarding path healthy. Four rules per cycle
// bound work; the cursor rotates so large configurations do not starve tails.
func (e *NodeEngine) ProbeCycle(ctx context.Context, c *Client) error {
	var plan dataplane.Plan
	if err := e.Adapter.Observe(ctx, func(_ context.Context, p dataplane.Plan) error { plan = p; return nil }); err != nil {
		return err
	}
	if len(plan.Config.Rules) == 0 {
		return nil
	}
	p, err := probe.New(probe.Config{})
	if err != nil {
		return err
	}
	count := len(plan.Config.Rules)
	if count > 4 {
		count = 4
	}
	for i := 0; i < count; i++ {
		r := plan.Config.Rules[(e.probeCursor+i)%len(plan.Config.Rules)]
		if r.Status != "pending-agent" {
			continue
		}
		types := []probe.Type{probe.TCP}
		if r.Protocol == "udp" {
			types = []probe.Type{probe.UDP}
		} else if r.Protocol == "both" {
			types = append(types, probe.UDP)
		}
		for _, kind := range types {
			result, err := p.Run(ctx, probe.Request{NodeID: plan.NodeID, RuleID: r.ID, Generation: plan.Generation, Type: kind, TargetHost: r.TargetHost, TargetPort: r.TargetPort, Timeout: 2 * time.Second})
			if err != nil {
				return err
			}
			if err = c.ReportProbe(ctx, result); err != nil {
				return err
			}
		}
	}
	e.probeCursor = (e.probeCursor + count) % len(plan.Config.Rules)
	return nil
}

func (c *Client) ReportProbe(ctx context.Context, result probe.Result) error {
	if err := ValidateEndpoint(c.BaseURL, c.NodeID); err != nil {
		return err
	}
	if c.HTTP == nil || result.NodeID != c.NodeID {
		return errors.New("probe identity/client mismatch")
	}
	b, err := json.Marshal(result)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/agent/probe", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(req)
	if err != nil {
		return errors.New("probe report unavailable")
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 8192))
	if resp.StatusCode != 200 {
		return errors.New("probe report rejected")
	}
	return nil
}
