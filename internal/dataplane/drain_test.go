package dataplane

import (
	"context"
	"testing"

	"reladock/internal/platform"
)

func TestDrainEvidenceRequiresStableEmptyGoodAndExactReusableQuarantines(t *testing.T) {
	cfg := fixtureV2()
	cfg.Rules = nil
	cfg.Allocations = nil
	cfg.Retirements = nil
	guard := AllocationGuard{ID: "33333333-3333-4333-8333-333333333333", RetirementID: "22222222-2222-4222-8222-222222222222", Address: cfg.ListenAddress, Protocol: "tcp", Port: 20001, State: "reusable"}
	o := options()
	o.quarantines = []AllocationGuard{guard}
	plan, err := Compile(context.Background(), cfg, o)
	if err != nil {
		t.Fatal(err)
	}
	store := &memoryStore{state: State{NodeID: cfg.NodeID, HighWater: cfg.Generation, Good: &plan, ReusableQuarantines: []AllocationGuard{guard}}}
	adapter := newTestAdapter(t, store, &fakeRunner{}, &mutexLock{}, goodVerify)
	evidence, err := adapter.DrainEvidence(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	item := platform.DrainQuarantine{RetirementID: guard.RetirementID, AllocationID: guard.ID, Address: guard.Address, Protocol: guard.Protocol, Port: guard.Port}
	item.GuardDigest = platform.DrainGuardDigest(item)
	count, digest, digestErr := platform.DrainQuarantineDigest([]platform.DrainQuarantine{item})
	if evidence.Generation != cfg.Generation || len(evidence.GoodDigest) != 64 || digestErr != nil || evidence.QuarantineCount != count || evidence.QuarantineDigest != digest {
		t.Fatalf("evidence %+v", evidence)
	}
	store.state.Pending = true
	store.state.Candidate = &plan
	if _, err = adapter.DrainEvidence(context.Background()); err == nil {
		t.Fatal("pending dataplane attested")
	}
}
