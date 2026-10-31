package dataplane

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"reladock/internal/platform"
	retirementexec "reladock/internal/retirement"
)

type fakeRetirementPurger struct {
	calls  []retirementexec.Allocation
	result retirementexec.PurgeResult
	err    error
}

func (p *fakeRetirementPurger) Purge(_ context.Context, allocation retirementexec.Allocation) (retirementexec.PurgeResult, error) {
	p.calls = append(p.calls, allocation)
	return p.result, p.err
}

func retirementClaim(cfg platform.NodeConfig) platform.RetirementClaim {
	r := cfg.Retirements[0]
	return platform.RetirementClaim{ID: r.ID, AllocationID: r.AllocationID, NodeID: cfg.NodeID, RuleID: r.RuleID, Address: r.Address, Protocol: r.Protocol, Port: r.Port, QuarantineGeneration: r.QuarantineGeneration}
}

func retirementState(t *testing.T) (State, platform.RetirementClaim) {
	t.Helper()
	cfg := fixtureV2()
	plan, err := Compile(context.Background(), cfg, options())
	if err != nil {
		t.Fatal(err)
	}
	return State{NodeID: cfg.NodeID, HighWater: cfg.Generation, Good: &plan, GoodDesired: &cfg}, retirementClaim(cfg)
}

func retirementAdapter(t *testing.T, store StateStore, runner Runner, purger RetirementPurger) *Adapter {
	t.Helper()
	adapter := newTestAdapter(t, store, runner, &mutexLock{}, goodVerify)
	adapter.SetRetirementPurger(purger)
	return adapter
}

func TestPurgeAllocationPersistsExactResultAndRemovesOnlyForwardGuard(t *testing.T) {
	state, claim := retirementState(t)
	store := &memoryStore{state: state}
	runner := &fakeRunner{exists: true, token: planToken(*state.Good)}
	purger := &fakeRetirementPurger{result: retirementexec.PurgeResult{ConntrackVersion: "conntrack v1.4.7 (conntrack-tools)", DeletedEntries: 3}}
	adapter := retirementAdapter(t, store, runner, purger)

	result, err := adapter.PurgeAllocation(context.Background(), claim)
	if err != nil {
		t.Fatal(err)
	}
	if result.ConntrackVersion != purger.result.ConntrackVersion || result.DeletedEntries != 3 || len(result.GuardBeforeDigest) != 64 || len(result.GuardAfterDigest) != 64 || len(result.GoodDigest) != 64 {
		t.Fatalf("incomplete purge result: %+v", result)
	}
	if len(purger.calls) != 1 || purger.calls[0] != (retirementexec.Allocation{Address: claim.Address, Protocol: claim.Protocol, Port: claim.Port}) {
		t.Fatalf("wrong purge tuple: %+v", purger.calls)
	}
	if store.state.Pending || store.state.Candidate != nil || store.state.Retirement != nil || len(store.state.PurgedPendingACK) != 1 || len(store.state.RetirementResults) != 1 {
		t.Fatalf("purge was not durably committed: %+v", store.state)
	}
	if len(store.state.Good.Quarantines) != 1 || len(store.state.Good.ForwardGuards) != 1 || store.state.Good.ForwardGuards[0].ID == claim.AllocationID {
		t.Fatalf("wrong post-purge guards: %+v", store.state.Good)
	}

	replayed, err := adapter.PurgeAllocation(context.Background(), claim)
	if err != nil || replayed != result || len(purger.calls) != 1 {
		t.Fatalf("purge result was not replay-stable: %+v %+v %v", result, replayed, err)
	}
}

func TestPurgeFailureAfterConntrackRestoresBothGuardsAndResumesWithoutRepurge(t *testing.T) {
	state, claim := retirementState(t)
	store := &memoryStore{state: state}
	runner := &fakeRunner{exists: true, token: planToken(*state.Good), failAt: map[int]bool{2: true}}
	purger := &fakeRetirementPurger{result: retirementexec.PurgeResult{ConntrackVersion: "conntrack v1.4.7 (conntrack-tools)", DeletedEntries: 4}}
	adapter := retirementAdapter(t, store, runner, purger)

	if _, err := adapter.PurgeAllocation(context.Background(), claim); err == nil || errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("expected safely rolled back apply error, got %v", err)
	}
	if store.state.Pending || store.state.Candidate != nil || store.state.Retirement == nil || store.state.Retirement.Phase != "purged" || len(store.state.Good.Quarantines) != 1 || len(store.state.Good.ForwardGuards) != 2 {
		t.Fatalf("rollback lost guards or durable purge: %+v", store.state)
	}
	result, err := adapter.PurgeAllocation(context.Background(), claim)
	if err != nil || result.DeletedEntries != 4 || len(purger.calls) != 1 {
		t.Fatalf("resume repeated side effect or lost result: result=%+v calls=%d err=%v", result, len(purger.calls), err)
	}
}

func TestPurgeStateWriteFailuresNeverReleaseGuardOrRepeatKnownPurge(t *testing.T) {
	for _, failSave := range []int{1, 2, 3, 4} {
		t.Run(string(rune('0'+failSave)), func(t *testing.T) {
			state, claim := retirementState(t)
			store := &memoryStore{state: state, failSave: failSave}
			runner := &fakeRunner{exists: true, token: planToken(*state.Good)}
			purger := &fakeRetirementPurger{result: retirementexec.PurgeResult{ConntrackVersion: "conntrack v1.4.7 (conntrack-tools)", DeletedEntries: 2}}
			adapter := retirementAdapter(t, store, runner, purger)
			if _, err := adapter.PurgeAllocation(context.Background(), claim); err == nil {
				t.Fatal("state write failure reported success")
			}
			if failSave == 1 {
				if len(purger.calls) != 0 || len(store.state.Good.ForwardGuards) != 2 {
					t.Fatalf("side effect preceded durable intent: %+v calls=%d", store.state, len(purger.calls))
				}
				return
			}
			if store.state.Pending || store.state.Retirement == nil || len(store.state.Good.Quarantines) != 1 || len(store.state.Good.ForwardGuards) != 2 || len(purger.calls) != 1 {
				t.Fatalf("failed boundary was not safely recovered: %+v calls=%d", store.state, len(purger.calls))
			}
			if _, err := adapter.PurgeAllocation(context.Background(), claim); err != nil || len(purger.calls) != 1 {
				t.Fatalf("resume repeated known purge: calls=%d err=%v", len(purger.calls), err)
			}
		})
	}
}

func TestPurgeResultSurvivesFileStoreRestart(t *testing.T) {
	state, claim := retirementState(t)
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := NewFileStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Save(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{exists: true, token: planToken(*state.Good)}
	purger := &fakeRetirementPurger{result: retirementexec.PurgeResult{ConntrackVersion: "conntrack v1.4.7 (conntrack-tools)", DeletedEntries: 5}}
	adapter := retirementAdapter(t, store, runner, purger)
	want, err := adapter.PurgeAllocation(context.Background(), claim)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := NewFileStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	restarted := retirementAdapter(t, reopened, runner, purger)
	got, err := restarted.PurgeAllocation(context.Background(), claim)
	if err != nil || got != want || len(purger.calls) != 1 {
		t.Fatalf("file restart lost purge proof: want=%+v got=%+v calls=%d err=%v", want, got, len(purger.calls), err)
	}
}

func TestConfirmRetirementACKChangesOnlyLocalClassification(t *testing.T) {
	state, claim := retirementState(t)
	store := &memoryStore{state: state}
	runner := &fakeRunner{exists: true, token: planToken(*state.Good)}
	purger := &fakeRetirementPurger{result: retirementexec.PurgeResult{ConntrackVersion: "conntrack v1.4.7 (conntrack-tools)"}}
	adapter := retirementAdapter(t, store, runner, purger)
	result, err := adapter.PurgeAllocation(context.Background(), claim)
	if err != nil {
		t.Fatal(err)
	}
	beforeCalls := len(runner.calls)
	beforeGood := planToken(*store.state.Good)
	if err = adapter.ConfirmRetirementACK(context.Background(), claim.ID, claim.AllocationID); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != beforeCalls || planToken(*store.state.Good) != beforeGood || len(store.state.PurgedPendingACK) != 0 || len(store.state.ReusableQuarantines) != 1 || store.state.ReusableQuarantines[0].State != "reusable" {
		t.Fatalf("ACK changed kernel plan or wrong classification: %+v", store.state)
	}

	restarted := retirementAdapter(t, store, runner, purger)
	replayed, err := restarted.PurgeAllocation(context.Background(), claim)
	if err != nil || replayed != result || len(purger.calls) != 1 {
		t.Fatalf("restart lost reusable proof: %+v %v", replayed, err)
	}
}

func TestAcknowledgedQuarantineIsConsumedOnlyByExactAppliedPredecessor(t *testing.T) {
	state, claim := retirementState(t)
	store := &memoryStore{state: state}
	runner := &fakeRunner{exists: true, token: planToken(*state.Good)}
	purger := &fakeRetirementPurger{result: retirementexec.PurgeResult{ConntrackVersion: "conntrack v1.4.7 (conntrack-tools)"}}
	adapter := retirementAdapter(t, store, runner, purger)
	if _, err := adapter.PurgeAllocation(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if err := adapter.ConfirmRetirementACK(context.Background(), claim.ID, claim.AllocationID); err != nil {
		t.Fatal(err)
	}

	next := fixtureV2()
	next.Generation++
	next.Retirements = nil
	next.Rules = append(next.Rules, platform.ConfigRule{
		ID: "replacement", TenantID: "tenant-a", RuleInput: platform.RuleInput{Name: "Replacement", NodeID: next.NodeID, Protocol: claim.Protocol, TargetHost: "198.51.100.3", TargetPort: 443, BandwidthMbps: 100},
		ListenPort: claim.Port, Version: 1, Status: "pending-agent", DevelopmentBypass: true,
	})
	next.Allocations = append(next.Allocations, platform.ConfigAllocation{ID: "55555555-5555-4555-8555-555555555555", RuleID: "replacement", Protocol: claim.Protocol, Address: claim.Address, Port: claim.Port, PredecessorRetirementID: claim.ID})
	if _, err := adapter.Apply(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	if len(store.state.ReusableQuarantines) != 0 || len(store.state.RetirementResults) != 0 || len(store.state.Good.Quarantines) != 0 {
		t.Fatalf("applied exact predecessor did not consume local lineage: %+v", store.state)
	}
}

func TestPurgeAllocationRejectsMismatchedIdentityAndMissingGuardsBeforeSideEffect(t *testing.T) {
	state, claim := retirementState(t)
	for name, mutate := range map[string]func(*State, *platform.RetirementClaim){
		"allocation": func(_ *State, c *platform.RetirementClaim) { c.AllocationID = "55555555-5555-4555-8555-555555555555" },
		"tuple":      func(_ *State, c *platform.RetirementClaim) { c.Port++ },
		"node":       func(_ *State, c *platform.RetirementClaim) { c.NodeID = "other" },
		"quarantine": func(s *State, _ *platform.RetirementClaim) { s.Good.Quarantines = nil },
		"forward":    func(s *State, _ *platform.RetirementClaim) { s.Good.ForwardGuards = s.Good.ForwardGuards[:1] },
	} {
		t.Run(name, func(t *testing.T) {
			copyState := state
			copyPlan := *state.Good
			copyPlan.Quarantines = append([]AllocationGuard(nil), state.Good.Quarantines...)
			copyPlan.ForwardGuards = append([]AllocationGuard(nil), state.Good.ForwardGuards...)
			copyState.Good = &copyPlan
			copyClaim := claim
			mutate(&copyState, &copyClaim)
			store := &memoryStore{state: copyState}
			purger := &fakeRetirementPurger{}
			adapter := retirementAdapter(t, store, &fakeRunner{exists: true, token: planToken(copyPlan)}, purger)
			_, err := adapter.PurgeAllocation(context.Background(), copyClaim)
			if err == nil || len(purger.calls) != 0 || store.saves != 0 {
				t.Fatalf("unsafe purge accepted: err=%v calls=%d saves=%d", err, len(purger.calls), store.saves)
			}
		})
	}
}

func TestConfirmRetirementACKRejectsWrongIdentity(t *testing.T) {
	state, claim := retirementState(t)
	store := &memoryStore{state: state}
	adapter := retirementAdapter(t, store, &fakeRunner{}, &fakeRetirementPurger{})
	if err := adapter.ConfirmRetirementACK(context.Background(), claim.ID, strings.Repeat("0", 36)); err == nil || store.saves != 0 {
		t.Fatalf("wrong ACK identity accepted: %v", err)
	}
}
