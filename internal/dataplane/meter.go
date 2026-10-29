package dataplane

import (
	"context"
	"errors"
	"math"
	"reladock/internal/metering"
	"reladock/internal/platform"
)

// MeterHooks bridges full forward counters to a private durable WAL. Never
// attach it to an already unmetered table: a nonempty physical incarnation is
// required, including during every rollback. It does not imply billing health.
type MeterHooks struct {
	WAL    *metering.WAL
	Runner Runner
}

func (h MeterHooks) MeterIdentity() (string, error) {
	if h.WAL == nil {
		return "", errors.New("WAL required")
	}
	return h.WAL.Identity()
}

func (h MeterHooks) QuotaUsed(ctx context.Context, cfg platform.NodeConfig) (map[string]int64, error) {
	if h.WAL == nil {
		return nil, errors.New("WAL required")
	}
	used := map[string]int64{}
	for _, r := range cfg.Rules {
		if r.Status != "pending-agent" || r.QuotaBytes == 0 {
			continue
		}
		if r.SubscriptionID == "" {
			return nil, errors.New("quota lacks paid period")
		}
		if e := h.WAL.BindPeriod(ctx, r.ID, r.SubscriptionID, r.UsageUploadBytes, r.UsageDownloadBytes); e != nil {
			return nil, e
		}
		up, down, e := h.WAL.CumulativeFor(ctx, r.ID, r.SubscriptionID)
		if e != nil {
			return nil, e
		}
		if up < 0 || down < 0 || up > math.MaxInt64-down {
			return nil, errors.New("quota usage overflow")
		}
		used[r.ID] = up + down
		if r.UsageUploadBytes > math.MaxInt64-r.UsageDownloadBytes {
			return nil, errors.New("snapshot usage overflow")
		}
		if server := r.UsageUploadBytes + r.UsageDownloadBytes; server > used[r.ID] {
			used[r.ID] = server
		}
	}
	return used, nil
}

func (h MeterHooks) BeforeReplace(ctx context.Context, p Plan) error { return h.Sample(ctx, p) }
func (h MeterHooks) AfterReplace(ctx context.Context, p Plan) error  { return h.Sample(ctx, p) }
func (h MeterHooks) Sample(ctx context.Context, p Plan) error {
	if h.WAL == nil || h.Runner == nil || p.Incarnation == "" {
		return errors.New("meter requires WAL, runner and physical incarnation")
	}
	periods := map[string]string{}
	for _, r := range p.Config.Rules {
		periods[r.ID] = r.SubscriptionID
	}
	manifest := make([]metering.Counter, 0, len(p.Counters))
	for _, c := range p.Counters {
		period, ok := periods[c.RuleID]
		if !ok || period == "" {
			return errors.New("business counter lacks paid period")
		}
		manifest = append(manifest, metering.Counter{Name: c.Name, RuleID: c.RuleID, SubscriptionID: period, Protocol: c.Protocol, Direction: c.Direction})
	}
	raw, e := h.Runner.Run(ctx, []string{"-j", "list", "table", "ip", Table}, "")
	if e != nil {
		return e
	}
	readings, e := metering.ParseCounters(raw, manifest)
	if e != nil {
		return e
	}
	_, e = h.WAL.BeginEpochFor(ctx, p.Incarnation, readings)
	return e
}
