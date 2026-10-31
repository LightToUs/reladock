package dataplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"reladock/internal/platform"
	retirementexec "reladock/internal/retirement"
)

// RetirementPurger performs only a fixed exact conntrack purge. Implementations
// must not accept raw command arguments; the production implementation is the
// retirement.Executor.
type RetirementPurger interface {
	Purge(context.Context, retirementexec.Allocation) (retirementexec.PurgeResult, error)
}

type PurgeOutcome struct {
	ConntrackVersion  string
	DeletedEntries    int
	GuardBeforeDigest string
	GuardAfterDigest  string
	GoodDigest        string
}

type RetirementIdentity struct {
	TaskID               string
	AllocationID         string
	NodeID               string
	RuleID               string
	Address              string
	Protocol             string
	Port                 int
	QuarantineGeneration int64
}

type RetirementTransition struct {
	Identity RetirementIdentity
	Phase    string
	Outcome  PurgeOutcome
}

type RetirementRecord struct {
	Identity RetirementIdentity
	Outcome  PurgeOutcome
}

type retirementOperationError struct{ code string }

func (e retirementOperationError) Error() string { return "dataplane retirement failed: " + e.code }

func RetirementErrorCode(err error) string {
	var target retirementOperationError
	if errors.As(err, &target) {
		return target.code
	}
	return retirementexec.ErrorCode(err)
}

func retirementFailure(code string) error { return retirementOperationError{code: code} }

func claimIdentity(claim platform.RetirementClaim) RetirementIdentity {
	return RetirementIdentity{
		TaskID: claim.ID, AllocationID: claim.AllocationID, NodeID: claim.NodeID, RuleID: claim.RuleID,
		Address: claim.Address, Protocol: claim.Protocol, Port: claim.Port, QuarantineGeneration: claim.QuarantineGeneration,
	}
}

func validRetirementIdentity(identity RetirementIdentity) bool {
	if !uuid.MatchString(identity.TaskID) || !uuid.MatchString(identity.AllocationID) || !identifier.MatchString(identity.NodeID) || !identifier.MatchString(identity.RuleID) || identity.QuarantineGeneration < 1 || (identity.Protocol != "tcp" && identity.Protocol != "udp") || identity.Port < 20000 || identity.Port > 60000 {
		return false
	}
	address, err := ipv4(identity.Address)
	return err == nil && address.String() == identity.Address
}

func sameKernelGuard(a, b AllocationGuard) bool {
	return a.ID == b.ID && a.RetirementID == b.RetirementID && a.Address == b.Address && a.Protocol == b.Protocol && a.Port == b.Port && a.PredecessorRetirementID == b.PredecessorRetirementID
}

func matchingRetirement(config platform.NodeConfig, identity RetirementIdentity) bool {
	if config.SchemaVersion != 2 || config.NodeID != identity.NodeID || config.Generation < identity.QuarantineGeneration {
		return false
	}
	for _, item := range config.Retirements {
		if item.ID == identity.TaskID {
			return item.AllocationID == identity.AllocationID && item.RuleID == identity.RuleID && item.Address == identity.Address && item.Protocol == identity.Protocol && item.Port == identity.Port && item.QuarantineGeneration == identity.QuarantineGeneration
		}
	}
	return false
}

func exactRetirementGuards(plan Plan, identity RetirementIdentity) (AllocationGuard, error) {
	want := AllocationGuard{ID: identity.AllocationID, RetirementID: identity.TaskID, Address: identity.Address, Protocol: identity.Protocol, Port: identity.Port}
	quarantineCount, forwardCount := 0, 0
	var quarantine AllocationGuard
	for _, guard := range plan.Quarantines {
		if guard.ID == identity.AllocationID {
			quarantineCount++
			quarantine = guard
			if !sameKernelGuard(guard, want) {
				return AllocationGuard{}, retirementFailure("guard-mismatch")
			}
		}
	}
	for _, guard := range plan.ForwardGuards {
		if guard.ID == identity.AllocationID {
			forwardCount++
			if !sameKernelGuard(guard, want) {
				return AllocationGuard{}, retirementFailure("guard-mismatch")
			}
		}
	}
	if quarantineCount == 0 || forwardCount == 0 {
		return AllocationGuard{}, retirementFailure("guard-missing")
	}
	if quarantineCount != 1 || forwardCount != 1 {
		return AllocationGuard{}, retirementFailure("guard-mismatch")
	}
	return quarantine, nil
}

func canonicalDigest(value any) string {
	raw, _ := json.Marshal(value)
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func guardDigests(before, after Plan, beforeGuard, afterGuard AllocationGuard) (string, string, string) {
	beforeDigest := canonicalDigest(struct {
		Schema     int
		Plan       string
		Quarantine AllocationGuard
		Forward    AllocationGuard
	}{1, planToken(before), beforeGuard, beforeGuard})
	afterDigest := canonicalDigest(struct {
		Schema         int
		Plan           string
		Quarantine     AllocationGuard
		ForwardRemoved bool
	}{1, planToken(after), afterGuard, true})
	return beforeDigest, afterDigest, canonicalDigest(struct {
		Schema int
		Plan   string
	}{1, planToken(after)})
}

func validatePurgeOutcome(outcome PurgeOutcome, complete bool) bool {
	if len(outcome.ConntrackVersion) < 1 || len(outcome.ConntrackVersion) > 80 || outcome.DeletedEntries < 0 {
		return false
	}
	if !complete {
		empty := outcome.GuardBeforeDigest == "" && outcome.GuardAfterDigest == "" && outcome.GoodDigest == ""
		return empty || validHexDigest(outcome.GuardBeforeDigest) && validHexDigest(outcome.GuardAfterDigest) && validHexDigest(outcome.GoodDigest)
	}
	return validHexDigest(outcome.GuardBeforeDigest) && validHexDigest(outcome.GuardAfterDigest) && validHexDigest(outcome.GoodDigest)
}

func validHexDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32
}

func validateRetirementState(state State) error {
	records := map[string]RetirementRecord{}
	allocations := map[string]bool{}
	for _, record := range state.RetirementResults {
		if !validRetirementIdentity(record.Identity) || !validatePurgeOutcome(record.Outcome, true) || records[record.Identity.TaskID].Identity.TaskID != "" || allocations[record.Identity.AllocationID] {
			return errors.New("invalid persisted retirement result")
		}
		records[record.Identity.TaskID] = record
		allocations[record.Identity.AllocationID] = true
		guardFound := false
		for _, guard := range append(append([]AllocationGuard(nil), state.PurgedPendingACK...), state.ReusableQuarantines...) {
			if guard.ID == record.Identity.AllocationID || guard.RetirementID == record.Identity.TaskID {
				if guard.ID != record.Identity.AllocationID || guard.RetirementID != record.Identity.TaskID || guard.Address != record.Identity.Address || guard.Protocol != record.Identity.Protocol || guard.Port != record.Identity.Port {
					return errors.New("persisted retirement result guard mismatch")
				}
				guardFound = true
			}
		}
		if !guardFound {
			return errors.New("persisted retirement result lacks quarantine")
		}
	}
	if state.Retirement == nil {
		return nil
	}
	transition := state.Retirement
	if !validRetirementIdentity(transition.Identity) || (transition.Phase != "prepared" && transition.Phase != "purged") || transition.Phase == "prepared" && transition.Outcome != (PurgeOutcome{}) || transition.Phase == "purged" && !validatePurgeOutcome(transition.Outcome, false) || records[transition.Identity.TaskID].Identity.TaskID != "" || allocations[transition.Identity.AllocationID] {
		return errors.New("invalid persisted retirement transition")
	}
	if state.Good == nil || !matchingRetirement(state.Good.Config, transition.Identity) {
		return errors.New("retirement transition absent from last-good config")
	}
	if state.Pending {
		if state.Candidate == nil {
			return errors.New("pending retirement lacks candidate")
		}
		if _, err := exactPostPurgeGuard(*state.Candidate, transition.Identity); err != nil {
			return errors.New("pending retirement candidate retains forward guard")
		}
	}
	return nil
}

func retainRetirementResults(state State) []RetirementRecord {
	retained := map[string]bool{}
	for _, guard := range append(append([]AllocationGuard(nil), state.PurgedPendingACK...), state.ReusableQuarantines...) {
		retained[guard.RetirementID] = true
	}
	result := make([]RetirementRecord, 0, len(state.RetirementResults))
	for _, record := range state.RetirementResults {
		if retained[record.Identity.TaskID] {
			result = append(result, record)
		}
	}
	return result
}

func findRecord(state State, identity RetirementIdentity) (PurgeOutcome, bool, error) {
	for _, record := range state.RetirementResults {
		if record.Identity.TaskID != identity.TaskID && record.Identity.AllocationID != identity.AllocationID {
			continue
		}
		if record.Identity != identity {
			return PurgeOutcome{}, false, retirementFailure("state-mismatch")
		}
		found := false
		for _, guard := range append(append([]AllocationGuard(nil), state.PurgedPendingACK...), state.ReusableQuarantines...) {
			found = found || guard.ID == identity.AllocationID && guard.RetirementID == identity.TaskID && guard.Address == identity.Address && guard.Protocol == identity.Protocol && guard.Port == identity.Port
		}
		if !found {
			return PurgeOutcome{}, false, retirementFailure("state-mismatch")
		}
		return record.Outcome, true, nil
	}
	return PurgeOutcome{}, false, nil
}

func (a *Adapter) compileRetirement(ctx context.Context, state State, guard AllocationGuard) (Plan, error) {
	options := a.options
	options.retired = state.Good.Allocations
	options.acceptedPredecessors = acceptedPredecessorsForPlan(*state.Good)
	options.retiredGuards = append([]AllocationGuard(nil), state.TombstoneGuards...)
	options.quarantines = append(localQuarantineOverlay(state), guard)
	if a.hooks != nil {
		options.incarnation = newIncarnation()
	}
	return Compile(ctx, state.Good.Config, options)
}

// PurgeAllocation transitions exactly one retirement from two kernel guards to
// a durable pre-conntrack quarantine. It must be called only after the platform
// has acknowledged begin-purge; this layer never stores the lease token.
func (a *Adapter) PurgeAllocation(ctx context.Context, claim platform.RetirementClaim) (PurgeOutcome, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	identity := claimIdentity(claim)
	if !validRetirementIdentity(identity) || identity.NodeID != a.options.NodeID {
		return PurgeOutcome{}, retirementFailure("state-mismatch")
	}
	unlock, err := a.lock.Lock(ctx)
	if err != nil {
		return PurgeOutcome{}, err
	}
	defer unlock()
	state, err := a.load(ctx)
	if err != nil {
		return PurgeOutcome{}, err
	}
	if state.Pending {
		return PurgeOutcome{}, ErrRecoveryRequired
	}
	if outcome, ok, replayErr := findRecord(state, identity); ok || replayErr != nil {
		return outcome, replayErr
	}
	if state.Good == nil || !matchingRetirement(state.Good.Config, identity) {
		return PurgeOutcome{}, retirementFailure("state-mismatch")
	}
	beforeGuard, err := exactRetirementGuards(*state.Good, identity)
	if err != nil {
		return PurgeOutcome{}, err
	}
	if state.Retirement != nil && state.Retirement.Identity != identity {
		return PurgeOutcome{}, retirementFailure("state-mismatch")
	}
	if state.Retirement == nil {
		state.Retirement = &RetirementTransition{Identity: identity, Phase: "prepared"}
	}
	guard := beforeGuard
	guard.State = "purged-pending-ack"
	candidate, err := a.compileRetirement(ctx, state, guard)
	if err != nil {
		return PurgeOutcome{}, retirementFailure("state-mismatch")
	}
	if _, err = exactPostPurgeGuard(candidate, identity); err != nil {
		return PurgeOutcome{}, err
	}
	if err = a.check(ctx, candidate.Script); err != nil {
		return PurgeOutcome{}, fmt.Errorf("retirement nft check: %w", err)
	}
	state.Pending = true
	state.Candidate = &candidate
	if err = a.store.Save(ctx, state); err != nil {
		return PurgeOutcome{}, fmt.Errorf("persist retirement intent: %w", err)
	}
	if err = a.verify(ctx, *state.Good); err != nil {
		return PurgeOutcome{}, a.failed(ctx, state, retirementFailure("guard-mismatch"), false)
	}
	if state.Retirement.Phase == "prepared" {
		if a.purger == nil {
			return PurgeOutcome{}, a.failed(ctx, state, retirementFailure("internal-error"), false)
		}
		purged, purgeErr := a.purger.Purge(ctx, retirementexec.Allocation{Address: identity.Address, Protocol: identity.Protocol, Port: identity.Port})
		if purgeErr != nil {
			return PurgeOutcome{}, a.failed(ctx, state, purgeErr, false)
		}
		if err = a.verify(ctx, *state.Good); err != nil {
			return PurgeOutcome{}, a.failed(ctx, state, retirementFailure("guard-mismatch"), false)
		}
		state.Retirement.Phase = "purged"
		state.Retirement.Outcome = PurgeOutcome{ConntrackVersion: purged.ConntrackVersion, DeletedEntries: purged.DeletedEntries}
		if !validatePurgeOutcome(state.Retirement.Outcome, false) {
			return PurgeOutcome{}, a.failed(ctx, state, retirementFailure("output-invalid"), false)
		}
		if err = a.store.Save(ctx, state); err != nil {
			return PurgeOutcome{}, a.failed(ctx, state, fmt.Errorf("persist conntrack result: %w", err), errors.Is(err, ErrStateUncertain))
		}
	}
	if a.hooks != nil {
		if err = a.freezeAndSample(ctx, *state.Good); err != nil {
			return PurgeOutcome{}, errors.Join(ErrRecoveryRequired, err)
		}
	}
	if budget, ok := a.hooks.(TransitionBudget); ok {
		options := a.options
		options.retired = state.Good.Allocations
		options.acceptedPredecessors = acceptedPredecessorsForPlan(*state.Good)
		options.retiredGuards = append([]AllocationGuard(nil), state.TombstoneGuards...)
		options.quarantines = append(localQuarantineOverlay(state), guard)
		options.incarnation = newIncarnation()
		options.quotaUsed, err = budget.QuotaUsed(ctx, state.Good.Config)
		if err != nil {
			return PurgeOutcome{}, errors.Join(ErrRecoveryRequired, err)
		}
		candidate, err = Compile(ctx, state.Good.Config, options)
		if err != nil {
			return PurgeOutcome{}, errors.Join(ErrRecoveryRequired, err)
		}
		if err = a.check(ctx, candidate.Script); err != nil {
			return PurgeOutcome{}, errors.Join(ErrRecoveryRequired, err)
		}
		state.Candidate = &candidate
	}
	before, after, good := guardDigests(*state.Good, candidate, beforeGuard, guard)
	state.Retirement.Outcome.GuardBeforeDigest = before
	state.Retirement.Outcome.GuardAfterDigest = after
	state.Retirement.Outcome.GoodDigest = good
	state.Candidate = &candidate
	if err = a.store.Save(ctx, state); err != nil {
		return PurgeOutcome{}, a.failed(ctx, state, fmt.Errorf("persist retirement transition: %w", err), errors.Is(err, ErrStateUncertain))
	}
	if preparer, ok := a.hooks.(TransitionPreparer); ok {
		if err = preparer.PrepareReplace(ctx, candidate); err != nil {
			return PurgeOutcome{}, a.failed(ctx, state, err, false)
		}
	}
	if err = a.execute(ctx, candidate.Script); err != nil {
		return PurgeOutcome{}, a.failed(ctx, state, fmt.Errorf("retirement nft apply: %w", err), false)
	}
	if err = a.verify(ctx, candidate); err != nil {
		return PurgeOutcome{}, a.failed(ctx, state, fmt.Errorf("retirement post-apply verification: %w", err), false)
	}
	if a.hooks != nil {
		if err = a.hooks.AfterReplace(ctx, candidate); err != nil {
			return PurgeOutcome{}, a.failed(ctx, state, err, false)
		}
	}
	committed := state
	committed.Pending = false
	committed.Good = &candidate
	committed.Candidate = nil
	committed.PurgedPendingACK = append(committed.PurgedPendingACK, guard)
	result := committed.Retirement.Outcome
	committed.RetirementResults = append(committed.RetirementResults, RetirementRecord{Identity: identity, Outcome: result})
	committed.Retirement = nil
	if err = a.store.Save(ctx, committed); err != nil {
		return PurgeOutcome{}, a.failed(ctx, state, fmt.Errorf("persist purged quarantine: %w", err), true)
	}
	return result, nil
}

func exactPostPurgeGuard(plan Plan, identity RetirementIdentity) (AllocationGuard, error) {
	count := 0
	var found AllocationGuard
	for _, guard := range plan.Quarantines {
		if guard.ID == identity.AllocationID {
			count++
			found = guard
		}
	}
	for _, guard := range plan.ForwardGuards {
		if guard.ID == identity.AllocationID {
			return AllocationGuard{}, retirementFailure("guard-mismatch")
		}
	}
	if count != 1 || found.RetirementID != identity.TaskID || found.Address != identity.Address || found.Protocol != identity.Protocol || found.Port != identity.Port || found.State != "purged-pending-ack" {
		return AllocationGuard{}, retirementFailure("guard-mismatch")
	}
	return found, nil
}

// ConfirmRetirementACK records that the platform accepted the proof. Kernel
// state does not change: the exact quarantine remains until an atomic reuse.
func (a *Adapter) ConfirmRetirementACK(ctx context.Context, taskID, allocationID string) error {
	if !uuid.MatchString(taskID) || !uuid.MatchString(allocationID) {
		return retirementFailure("state-mismatch")
	}
	unlock, err := a.lock.Lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	state, err := a.load(ctx)
	if err != nil {
		return err
	}
	if state.Pending || state.Retirement != nil {
		return ErrRecoveryRequired
	}
	recorded := false
	for _, record := range state.RetirementResults {
		if record.Identity.TaskID == taskID && record.Identity.AllocationID == allocationID {
			recorded = true
		}
	}
	if !recorded {
		return retirementFailure("state-mismatch")
	}
	for _, guard := range state.ReusableQuarantines {
		if guard.RetirementID == taskID && guard.ID == allocationID {
			return nil
		}
	}
	index := -1
	for i, guard := range state.PurgedPendingACK {
		if guard.RetirementID == taskID || guard.ID == allocationID {
			if guard.RetirementID != taskID || guard.ID != allocationID {
				return retirementFailure("state-mismatch")
			}
			index = i
		}
	}
	if index < 0 {
		return retirementFailure("state-mismatch")
	}
	guard := state.PurgedPendingACK[index]
	guard.State = "reusable"
	state.PurgedPendingACK = append(state.PurgedPendingACK[:index], state.PurgedPendingACK[index+1:]...)
	state.ReusableQuarantines = append(state.ReusableQuarantines, guard)
	if err = a.store.Save(ctx, state); err != nil {
		return fmt.Errorf("persist retirement acknowledgement: %w", err)
	}
	return nil
}
