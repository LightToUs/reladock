package dataplane

import (
	"context"
	"errors"
	"reladock/internal/platform"
	"reladock/internal/shaping"
)

// ShapedMeterHooks is the composed transition boundary: freeze (Adapter),
// durable final counter sample, quota budget, tc verification, atomic nft, new
// counter incarnation. Both directions must leave via the same leased device.
type ShapedMeterHooks struct {
	MeterHooks
	Shaper *shaping.Adapter
}

func (h ShapedMeterHooks) PrepareReplace(ctx context.Context, p Plan) error {
	if h.Shaper == nil {
		return errors.New("verified shaper required")
	}
	if e := h.Shaper.Recover(ctx); e != nil {
		return e
	}
	if len(p.Config.Rules) == 0 {
		return h.Shaper.PrepareEmpty(ctx)
	}
	_, e := h.Shaper.Ensure(ctx, p.Config)
	if errors.Is(e, shaping.ErrStale) {
		_, e = h.Shaper.RestoreConfig(ctx, p.Config)
	}
	return e
}

func ComposedVerifier(r Runner, s *shaping.Adapter) Verifier {
	return func(ctx context.Context, p Plan) error {
		if e := ReadbackVerifier(r)(ctx, p); e != nil {
			return e
		}
		if s == nil {
			return errors.New("shaper required")
		}
		if len(p.Config.Rules) == 0 {
			return s.PrepareEmpty(ctx)
		}
		return s.Verify(ctx, p.Config)
	}
}

// ValidatePaidConfig is required by the automatic runtime. Standalone compiler
// fixtures may omit entitlements; deployed active rules must never do so.
func ValidatePaidConfig(cfg platform.NodeConfig) error {
	for _, r := range cfg.Rules {
		if r.Status == "pending-agent" && !r.DevelopmentBypass && (r.SubscriptionID == "" || r.ExpiresAt == nil || r.QuotaBytes <= 0) {
			return errors.New("active rule lacks quota, paid period or kernel expiry")
		}
	}
	return nil
}
