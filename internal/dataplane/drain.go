package dataplane

import (
	"context"
	"errors"

	"reladock/internal/platform"
)

type DrainEvidence struct {
	Generation       int64
	GoodDigest       string
	QuarantineCount  int
	QuarantineDigest string
}

// DrainEvidence proves only local stable kernel/config state. WAL, updater and
// Agent journal gates are supplied independently by their owning components.
func (a *Adapter) DrainEvidence(ctx context.Context) (DrainEvidence, error) {
	unlock, err := a.lock.Lock(ctx)
	if err != nil {
		return DrainEvidence{}, err
	}
	defer unlock()
	state, err := a.load(ctx)
	if err != nil {
		return DrainEvidence{}, err
	}
	if state.Pending || state.Candidate != nil || state.Recovery != nil || state.PreviousRecovery != nil || state.Retirement != nil || state.Good == nil || len(state.Good.Config.Rules) != 0 || len(state.PurgedPendingACK) != 0 {
		return DrainEvidence{}, errors.New("dataplane is not drain-stable")
	}
	if err = a.verify(ctx, *state.Good); err != nil {
		return DrainEvidence{}, err
	}
	evidence := DrainEvidence{Generation: state.Good.Generation, GoodDigest: canonicalDigest(struct {
		Schema int
		Plan   string
	}{1, planToken(*state.Good)})}
	quarantines := make([]platform.DrainQuarantine, 0, len(state.ReusableQuarantines))
	for _, guard := range state.ReusableQuarantines {
		item := platform.DrainQuarantine{RetirementID: guard.RetirementID, AllocationID: guard.ID, Address: guard.Address, Protocol: guard.Protocol, Port: guard.Port}
		item.GuardDigest = platform.DrainGuardDigest(item)
		quarantines = append(quarantines, item)
	}
	evidence.QuarantineCount, evidence.QuarantineDigest, err = platform.DrainQuarantineDigest(quarantines)
	if err != nil {
		return DrainEvidence{}, err
	}
	return evidence, nil
}
