package dataplane

import (
	"context"
	"errors"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"

	"reladock/internal/platform"
)

type memoryStore struct {
	state    State
	failSave int
	saves    int
}

// A store may report a durability error after its atomic rename. The Adapter
// must re-establish pending intent before rollback, including across restart.
type uncertainCommitStore struct {
	memoryStore
	commitFailed bool
}

func (s *uncertainCommitStore) Save(c context.Context, v State) error {
	s.state = v
	if !v.Pending && v.Good != nil && !s.commitFailed {
		s.commitFailed = true
		return errors.Join(ErrStateUncertain, errors.New("directory sync failed after rename"))
	}
	return nil
}

func TestUncertainCommitAndFailedRollbackRemainBlockedAcrossRestart(t *testing.T) {
	s := &uncertainCommitStore{}
	r := &fakeRunner{failAt: map[int]bool{6: true}}
	a := newTestAdapter(t, s, r, &mutexLock{}, goodVerify)
	if _, e := a.Apply(context.Background(), fixture()); e == nil {
		t.Fatal("uncertain commit lost")
	}
	if !s.state.Pending || s.state.Good != nil {
		t.Fatalf("uncertain commit left false last-good state: %+v", s.state)
	}
	a = newTestAdapter(t, s, r, &mutexLock{}, goodVerify)
	c := fixture()
	c.Generation = 2
	if _, e := a.Apply(context.Background(), c); !errors.Is(e, ErrRecoveryRequired) {
		t.Fatalf("restart did not block: %v", e)
	}
}

func (s *memoryStore) Load(context.Context) (State, error) { return s.state, nil }
func (s *memoryStore) Save(_ context.Context, v State) error {
	s.saves++
	if s.saves == s.failSave {
		return errors.New("disk failed")
	}
	s.state = v
	return nil
}

type mutexLock struct{ sync.Mutex }

func (m *mutexLock) Lock(context.Context) (func(), error) { m.Mutex.Lock(); return m.Mutex.Unlock, nil }

type call struct {
	args   []string
	script string
}
type fakeRunner struct {
	calls         []call
	failAt        map[int]bool
	foreign       bool
	exists        bool
	token         string
	freezeRules   int
	invalidFreeze bool
	tableReads    int
	foreignAtRead int
}

func (r *fakeRunner) Run(_ context.Context, args []string, input string) ([]byte, error) {
	r.calls = append(r.calls, call{append([]string(nil), args...), input})
	if r.failAt[len(r.calls)] {
		return nil, errors.New("nft failed")
	}
	if reflect.DeepEqual(args, []string{"-j", "list", "tables"}) {
		if r.foreign || r.exists {
			return []byte(`{"nftables":[{"table":{"family":"ip","name":"reladock"}}]}`), nil
		}
		return []byte(`{"nftables":[]}`), nil
	}
	if reflect.DeepEqual(args, []string{"-j", "list", "table", "ip", "reladock"}) {
		r.tableReads++
		comment := owner
		if r.foreign || r.foreignAtRead == r.tableReads {
			comment = "foreign"
		}
		chain := ""
		if r.token != "" {
			chain = `,{"chain":{"name":"relay","comment":"` + r.token + `"}}`
		}
		rules := ""
		for i := 0; i < r.freezeRules; i++ {
			expr := `[{"drop":null}]`
			if r.invalidFreeze {
				expr = `[{"accept":null}]`
			}
			rules += `,{"rule":{"family":"ip","table":"reladock","chain":"relay","comment":"reladock-meter-freeze","expr":` + expr + `}}`
		}
		return []byte(`{"nftables":[{"table":{"family":"ip","name":"reladock","comment":"` + comment + `"}}` + chain + rules + `]}`), nil
	}
	if reflect.DeepEqual(args, []string{"-f", "-"}) {
		if strings.Contains(input, `reladock-meter-freeze`) {
			r.freezeRules++
		}
		if strings.Contains(input, "chain forward") {
			r.exists = true
			r.token = regexp.MustCompile(`reladock-plan-[0-9a-f]+`).FindString(input)
			r.freezeRules = 0
		}
	}
	return nil, nil
}

func TestFreezeOwnedIsIdempotent(t *testing.T) {
	r := &fakeRunner{exists: true, token: "reladock-plan-0123456789abcdef"}
	a := newTestAdapter(t, &memoryStore{}, r, &mutexLock{}, goodVerify)
	if err := a.freezeOwned(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := a.freezeOwned(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.freezeRules != 1 {
		t.Fatalf("freeze insertions=%d, want 1", r.freezeRules)
	}
}

func TestFreezeOwnedRecognizesExistingDuplicateDropsWithoutGrowingThem(t *testing.T) {
	r := &fakeRunner{exists: true, token: "reladock-plan-0123456789abcdef", freezeRules: 3}
	a := newTestAdapter(t, &memoryStore{}, r, &mutexLock{}, goodVerify)
	if err := a.freezeOwned(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.freezeRules != 3 {
		t.Fatalf("existing freezes grew to %d", r.freezeRules)
	}
}

func TestFreezeOwnedRejectsCommentThatIsNotADrop(t *testing.T) {
	r := &fakeRunner{exists: true, token: "reladock-plan-0123456789abcdef", freezeRules: 1, invalidFreeze: true}
	a := newTestAdapter(t, &memoryStore{}, r, &mutexLock{}, goodVerify)
	if err := a.freezeOwned(context.Background()); err == nil {
		t.Fatal("non-drop freeze comment accepted")
	}
	if r.freezeRules != 1 {
		t.Fatalf("wrote after malformed freeze: %d", r.freezeRules)
	}
}

func TestFreezeOwnedRevalidatesOwnershipOnDecisionSnapshot(t *testing.T) {
	r := &fakeRunner{exists: true, token: "reladock-plan-0123456789abcdef", foreignAtRead: 2}
	a := newTestAdapter(t, &memoryStore{}, r, &mutexLock{}, goodVerify)
	if err := a.freezeOwned(context.Background()); err == nil {
		t.Fatal("foreign replacement accepted between ownership and freeze reads")
	}
	if r.freezeRules != 0 {
		t.Fatalf("foreign table modified with %d freeze rules", r.freezeRules)
	}
}
func newTestAdapter(t *testing.T, s StateStore, r Runner, l Locker, v Verifier) *Adapter {
	t.Helper()
	a, e := NewAdapter(options(), r, s, l, v)
	if e != nil {
		t.Fatal(e)
	}
	return a
}
func goodVerify(context.Context, Plan) error { return nil }

func TestApplyCheckThenAtomicApplyAndStaleAcrossRestart(t *testing.T) {
	s := &memoryStore{}
	r := &fakeRunner{}
	a := newTestAdapter(t, s, r, &mutexLock{}, goodVerify)
	p, e := a.Apply(context.Background(), fixture())
	if e != nil {
		t.Fatal(e)
	}
	if s.state.Pending || s.state.HighWater != 1 || s.state.Good == nil || s.state.Good.Script != p.Script {
		t.Fatalf("state %+v", s.state)
	}
	if len(r.calls) != 3 || !reflect.DeepEqual(r.calls[1].args, []string{"-c", "-f", "-"}) || !reflect.DeepEqual(r.calls[2].args, []string{"-f", "-"}) || r.calls[1].script != r.calls[2].script {
		t.Fatalf("calls %+v", r.calls)
	}
	a = newTestAdapter(t, s, r, &mutexLock{}, goodVerify)
	for _, gen := range []int64{1, 0} {
		c := fixture()
		c.Generation = gen
		if _, e = a.Apply(context.Background(), c); e == nil {
			t.Fatal("stale accepted")
		}
	}
	if len(r.calls) != 3 {
		t.Fatal("stale config invoked nft")
	}
}

func TestInspectUsesStrictTextFallbackWhenJSONOmitsTableComment(t *testing.T) {
	for _, text := range []string{
		"table ip reladock { # handle 24\n\tcomment \"reladock-dataplane-v1\"\n}\n",
		"table ip reladock { # handle 24\n\tcomment \"foreign\"\n}\n",
	} {
		r := runnerFunc(func(_ context.Context, args []string, input string) ([]byte, error) {
			if input != "" {
				t.Fatal("inspection wrote nft input")
			}
			switch {
			case reflect.DeepEqual(args, []string{"-j", "list", "tables"}):
				return []byte(`{"nftables":[{"table":{"family":"ip","name":"reladock"}}]}`), nil
			case reflect.DeepEqual(args, []string{"-j", "list", "table", "ip", "reladock"}):
				return []byte(`{"nftables":[{"table":{"family":"ip","name":"reladock"}}]}`), nil
			case reflect.DeepEqual(args, []string{"-a", "list", "table", "ip", "reladock"}):
				return []byte(text), nil
			default:
				t.Fatal("unexpected inspection command", args)
				return nil, nil
			}
		})
		a := newTestAdapter(t, &memoryStore{}, r, &mutexLock{}, goodVerify)
		found, e := a.inspect(context.Background())
		if (e == nil) != strings.Contains(text, owner) || found != (e == nil) {
			t.Fatalf("text=%q found=%t err=%v", text, found, e)
		}
	}
}

func TestActualPlanUsesTextFallbackWhenJSONOmitsRelayComment(t *testing.T) {
	p, e := Compile(context.Background(), fixture(), options())
	if e != nil {
		t.Fatal(e)
	}
	r := runnerFunc(func(_ context.Context, args []string, input string) ([]byte, error) {
		if input != "" {
			t.Fatal("recovery inspection wrote nft input")
		}
		switch {
		case reflect.DeepEqual(args, []string{"-j", "list", "tables"}):
			return []byte(`{"nftables":[{"table":{"family":"ip","name":"reladock"}}]}`), nil
		case reflect.DeepEqual(args, []string{"-j", "list", "table", "ip", "reladock"}):
			return []byte(`{"nftables":[{"table":{"family":"ip","name":"reladock","comment":"reladock-dataplane-v1"}},{"chain":{"family":"ip","table":"reladock","name":"relay"}}]}`), nil
		case reflect.DeepEqual(args, []string{"-a", "list", "table", "ip", "reladock"}):
			return []byte("table ip reladock {\n\tcomment \"reladock-dataplane-v1\"\n\tchain relay {\n\t\tcomment \"" + planToken(p) + "\"\n\t}\n}\n"), nil
		default:
			t.Fatal("unexpected recovery inspection command", args)
			return nil, nil
		}
	})
	a := newTestAdapter(t, &memoryStore{}, r, &mutexLock{}, goodVerify)
	actual, e := a.actualPlan(context.Background(), State{Candidate: &p})
	if e != nil || actual == nil || actual.Incarnation != p.Incarnation {
		t.Fatalf("physical plan not recovered: actual=%+v err=%v", actual, e)
	}
}

func TestCheckFailureNeverApplies(t *testing.T) {
	s := &memoryStore{}
	r := &fakeRunner{failAt: map[int]bool{2: true}}
	a := newTestAdapter(t, s, r, &mutexLock{}, goodVerify)
	if _, e := a.Apply(context.Background(), fixture()); e == nil {
		t.Fatal("missing failure")
	}
	if len(r.calls) != 2 || s.state.Pending || s.state.Good != nil {
		t.Fatalf("mutated: %+v %+v", r.calls, s.state)
	}
}

func TestRuntimeFailureRestoresPinnedLastGood(t *testing.T) {
	s := &memoryStore{}
	r := &fakeRunner{}
	a := newTestAdapter(t, s, r, &mutexLock{}, goodVerify)
	old, e := a.Apply(context.Background(), fixture())
	if e != nil {
		t.Fatal(e)
	}
	a.verify = func(_ context.Context, p Plan) error {
		if p.Generation == 2 {
			return errors.New("verification failed")
		}
		return nil
	}
	c := fixture()
	c.Generation = 2
	c.Rules[0].TargetHost = "198.51.100.99"
	if _, e = a.Apply(context.Background(), c); e == nil {
		t.Fatal("failure lost")
	}
	if s.state.Pending || s.state.HighWater != 2 || s.state.Good.Generation != 1 {
		t.Fatalf("state %+v", s.state)
	}
	if got := r.calls[len(r.calls)-1]; got.script != old.Script || !reflect.DeepEqual(got.args, []string{"-f", "-"}) {
		t.Fatal("did not restore old frozen transaction")
	}
	if _, e = a.Apply(context.Background(), c); !errors.Is(e, ErrStale) {
		t.Fatalf("failed generation replay: %v", e)
	}
}

func TestSchemaV1ToV2FailedApplyDoesNotFenceExistingActiveTuples(t *testing.T) {
	s := &memoryStore{}
	r := &fakeRunner{}
	a := newTestAdapter(t, s, r, &mutexLock{}, goodVerify)
	legacy := fixture()
	if _, err := a.Apply(context.Background(), legacy); err != nil {
		t.Fatal(err)
	}

	candidate := fixture()
	candidate.SchemaVersion = 2
	candidate.Generation = 2
	candidate.Allocations = []platform.ConfigAllocation{
		{ID: "11111111-1111-4111-8111-111111111111", RuleID: candidate.Rules[0].ID, Protocol: "tcp", Address: candidate.ListenAddress, Port: candidate.Rules[0].ListenPort},
		{ID: "22222222-2222-4222-8222-222222222222", RuleID: candidate.Rules[0].ID, Protocol: "udp", Address: candidate.ListenAddress, Port: candidate.Rules[0].ListenPort},
	}
	a.verify = func(_ context.Context, p Plan) error {
		if p.Generation == candidate.Generation {
			return errors.New("injected schema-v2 readback failure")
		}
		return nil
	}
	if _, err := a.Apply(context.Background(), candidate); err == nil || errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("schema migration failure did not complete rollback: %v", err)
	}
	if s.state.Pending || len(s.state.Tombstones) != 0 || len(s.state.TombstoneGuards) != 0 || s.state.Good == nil || s.state.Good.Config.SchemaVersion != 1 {
		t.Fatalf("legacy active tuples were fenced during rollback: %+v", s.state)
	}
	a = newTestAdapter(t, s, r, &mutexLock{}, goodVerify)
	if _, err := a.load(context.Background()); err != nil {
		t.Fatal("rolled-back schema-v1 state did not survive restart:", err)
	}
}

func TestSchemaV1ToV2RejectsActiveTupleReassignedToDifferentRule(t *testing.T) {
	s := &memoryStore{}
	r := &fakeRunner{}
	a := newTestAdapter(t, s, r, &mutexLock{}, goodVerify)
	legacy := fixture()
	if _, err := a.Apply(context.Background(), legacy); err != nil {
		t.Fatal(err)
	}
	callsBefore := len(r.calls)

	candidate := fixture()
	candidate.SchemaVersion = 2
	candidate.Generation = 2
	candidate.Rules[0].ID = "rule-replacement"
	candidate.Allocations = []platform.ConfigAllocation{
		{ID: "11111111-1111-4111-8111-111111111111", RuleID: candidate.Rules[0].ID, Protocol: "tcp", Address: candidate.ListenAddress, Port: candidate.Rules[0].ListenPort},
		{ID: "22222222-2222-4222-8222-222222222222", RuleID: candidate.Rules[0].ID, Protocol: "udp", Address: candidate.ListenAddress, Port: candidate.Rules[0].ListenPort},
	}
	if _, err := a.Apply(context.Background(), candidate); err == nil {
		t.Fatal("schema migration reassigned an active tuple to a different rule")
	}
	if len(r.calls) != callsBefore || s.state.Pending || s.state.HighWater != legacy.Generation {
		t.Fatalf("unsafe migration reached kernel or durable intent: calls=%d state=%+v", len(r.calls)-callsBefore, s.state)
	}
}

func TestSchemaV1ToV2FailedApplyFencesNewTupleAcrossRestart(t *testing.T) {
	s := &memoryStore{}
	r := &fakeRunner{}
	a := newTestAdapter(t, s, r, &mutexLock{}, goodVerify)
	legacy := fixture()
	if _, err := a.Apply(context.Background(), legacy); err != nil {
		t.Fatal(err)
	}

	candidate := fixture()
	candidate.SchemaVersion = 2
	candidate.Generation = 2
	candidate.Allocations = []platform.ConfigAllocation{
		{ID: "11111111-1111-4111-8111-111111111111", RuleID: candidate.Rules[0].ID, Protocol: "tcp", Address: candidate.ListenAddress, Port: candidate.Rules[0].ListenPort},
		{ID: "22222222-2222-4222-8222-222222222222", RuleID: candidate.Rules[0].ID, Protocol: "udp", Address: candidate.ListenAddress, Port: candidate.Rules[0].ListenPort},
	}
	newRule := candidate.Rules[0]
	newRule.ID = "rule-new"
	newRule.Protocol = "tcp"
	newRule.ListenPort = 20001
	candidate.Rules = append(candidate.Rules, newRule)
	candidate.Allocations = append(candidate.Allocations, platform.ConfigAllocation{ID: "33333333-3333-4333-8333-333333333333", RuleID: newRule.ID, Protocol: "tcp", Address: candidate.ListenAddress, Port: newRule.ListenPort})
	a.verify = func(_ context.Context, p Plan) error {
		if p.Generation == candidate.Generation {
			return errors.New("injected schema-v2 readback failure")
		}
		return nil
	}
	if _, err := a.Apply(context.Background(), candidate); err == nil || errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("schema migration failure did not complete rollback: %v", err)
	}
	want := Allocation{Address: candidate.ListenAddress, Protocol: "tcp", Port: newRule.ListenPort}
	if s.state.Pending || len(s.state.Tombstones) != 1 || s.state.Tombstones[0] != want || len(s.state.TombstoneGuards) != 0 {
		t.Fatalf("new migration tuple was not durably fenced: %+v", s.state)
	}
	a = newTestAdapter(t, s, r, &mutexLock{}, goodVerify)
	if _, err := a.load(context.Background()); err != nil {
		t.Fatal("rollback tombstone did not survive restart:", err)
	}
}

func TestRollbackFailureBlocksUntilExplicitRecovery(t *testing.T) {
	s := &memoryStore{}
	r := &fakeRunner{failAt: map[int]bool{3: true, 5: true}}
	a := newTestAdapter(t, s, r, &mutexLock{}, goodVerify)
	if _, e := a.Apply(context.Background(), fixture()); e == nil {
		t.Fatal("runtime failure lost")
	}
	if !s.state.Pending {
		t.Fatal("uncertain state not persisted")
	}
	c := fixture()
	c.Generation = 2
	if _, e := a.Apply(context.Background(), c); !errors.Is(e, ErrRecoveryRequired) {
		t.Fatalf("not blocked: %v", e)
	}
	r.failAt = nil
	if e := a.Recover(context.Background()); e != nil {
		t.Fatal(e)
	}
	if s.state.Pending || s.state.HighWater != 1 || !r.exists || s.state.Good == nil || len(s.state.Good.Counters) != 0 {
		t.Fatal("initial failure did not retain a guard-only protection plan")
	}
	c.Rules[0].ListenPort = 20002 // Failed original allocation stays fenced.
	if _, e := a.Apply(context.Background(), c); e != nil {
		t.Fatal(e)
	}
}

func TestWriteAheadAndCommitFailures(t *testing.T) {
	for _, fail := range []int{1, 2} {
		t.Run(string(rune('0'+fail)), func(t *testing.T) {
			s := &memoryStore{failSave: fail}
			r := &fakeRunner{}
			a := newTestAdapter(t, s, r, &mutexLock{}, goodVerify)
			if _, e := a.Apply(context.Background(), fixture()); e == nil {
				t.Fatal("disk error lost")
			}
			if fail == 1 && len(r.calls) != 2 {
				t.Fatal("mutated kernel without durable intent")
			}
			if fail == 2 && (!r.exists || s.state.Pending || s.state.Good == nil || len(s.state.Good.Counters) != 0) {
				t.Fatal("commit failure didn't restore guard-only baseline")
			}
		})
	}
}

func TestForeignTableAndCorruptStateFailClosed(t *testing.T) {
	r := &fakeRunner{foreign: true}
	s := &memoryStore{}
	a := newTestAdapter(t, s, r, &mutexLock{}, goodVerify)
	if _, e := a.Apply(context.Background(), fixture()); e == nil || len(r.calls) != 2 {
		t.Fatal("foreign table overwritten")
	}
	s.state = State{NodeID: "other", HighWater: 10}
	r.foreign = false
	if _, e := a.Apply(context.Background(), fixture()); e == nil || len(r.calls) != 2 {
		t.Fatal("node mismatch accepted")
	}
	s.state = State{NodeID: "hk-01", HighWater: 1, Good: &Plan{NodeID: "hk-01", Generation: 1, Script: "flush ruleset"}}
	if e := a.Recover(context.Background()); e == nil {
		t.Fatal("corrupt persisted script executed")
	}
}

func TestDeletedAllocationsStayScopedAndArePersisted(t *testing.T) {
	s := &memoryStore{}
	r := &fakeRunner{}
	a := newTestAdapter(t, s, r, &mutexLock{}, goodVerify)
	if _, e := a.Apply(context.Background(), fixture()); e != nil {
		t.Fatal(e)
	}
	c := fixture()
	c.Generation = 2
	c.Rules = nil
	p, e := a.Apply(context.Background(), c)
	if e != nil {
		t.Fatal(e)
	}
	if len(p.Allocations) != 2 || !strings.Contains(p.Script, "ct original proto-dst 20000 jump relay") || strings.Contains(p.Script, "20000-60000") || strings.Contains(p.Script, "dnat to") {
		t.Fatal("lost retired allocation guards", p.Script)
	}
	a = newTestAdapter(t, s, r, &mutexLock{}, goodVerify)
	c.Generation = 3
	p, e = a.Apply(context.Background(), c)
	if e != nil || len(p.Allocations) != 2 {
		t.Fatalf("retired allocations lost on restart: %+v %v", p, e)
	}
}

func TestSchemaV2ApplyAndFailedCandidateIdentitySurviveRestart(t *testing.T) {
	s := &memoryStore{}
	r := &fakeRunner{}
	a := newTestAdapter(t, s, r, &mutexLock{}, goodVerify)
	if _, err := a.Apply(context.Background(), fixtureV2()); err != nil {
		t.Fatal(err)
	}
	a = newTestAdapter(t, s, r, &mutexLock{}, goodVerify)
	if _, err := a.ApplyVerified(context.Background(), fixtureV2()); err != nil {
		t.Fatal("schema-v2 last-good did not survive restart", err)
	}

	s = &memoryStore{}
	r = &fakeRunner{failAt: map[int]bool{3: true}}
	a = newTestAdapter(t, s, r, &mutexLock{}, goodVerify)
	failedConfig := fixtureV2()
	failedConfig.Retirements = nil
	if _, err := a.Apply(context.Background(), failedConfig); err == nil {
		t.Fatal("injected apply failure was accepted")
	} else if errors.Is(err, ErrRecoveryRequired) {
		t.Log("rollback remained pending:", err)
	}
	if s.state.Pending || s.state.Good == nil || len(s.state.TombstoneGuards) != 1 || len(s.state.Good.ForwardGuards) != 1 {
		t.Fatalf("failed allocation identity was not durably fenced: %+v", s.state)
	}
	if s.state.TombstoneGuards[0].ID != failedConfig.Allocations[0].ID {
		t.Fatal("wrong failed allocation identity", s.state.TombstoneGuards)
	}
	a = newTestAdapter(t, s, r, &mutexLock{}, goodVerify)
	next := failedConfig
	next.Generation = 2
	next.Allocations[0].ID = "88888888-8888-4888-8888-888888888888"
	if _, err := a.Apply(context.Background(), next); err == nil {
		t.Fatal("failed candidate tuple was reused with a different allocation ID")
	}
}

func TestReusableQuarantineRequiresPredecessorAndIsConsumedAtomically(t *testing.T) {
	base := fixtureV2()
	retirement := base.Retirements[0]
	guard := AllocationGuard{ID: retirement.AllocationID, RetirementID: retirement.ID, Address: retirement.Address, Protocol: retirement.Protocol, Port: retirement.Port, State: "reusable"}
	empty := base
	empty.Rules = nil
	empty.Allocations = nil
	empty.Retirements = nil
	o := options()
	o.quarantines = []AllocationGuard{guard}
	p, err := Compile(context.Background(), empty, o)
	if err != nil {
		t.Fatal(err)
	}
	s := &memoryStore{state: State{NodeID: empty.NodeID, HighWater: empty.Generation, Good: &p, ReusableQuarantines: []AllocationGuard{guard}}}
	r := &fakeRunner{exists: true, token: planToken(p)}
	a := newTestAdapter(t, s, r, &mutexLock{}, goodVerify)
	replacement := empty
	replacement.Generation++
	replacement.Rules = []platform.ConfigRule{{ID: "replacement", TenantID: "tenant-a", RuleInput: platform.RuleInput{Name: "Replacement", NodeID: empty.NodeID, Protocol: guard.Protocol, TargetHost: "198.51.100.3", TargetPort: 443, BandwidthMbps: 100}, ListenPort: guard.Port, Version: 1, Status: "pending-agent"}}
	replacement.Allocations = []platform.ConfigAllocation{{ID: "99999999-9999-4999-8999-999999999999", RuleID: "replacement", Protocol: guard.Protocol, Address: guard.Address, Port: guard.Port}}
	if _, err = a.Apply(context.Background(), replacement); err == nil {
		t.Fatal("tuple takeover without predecessor was accepted")
	}
	replacement.Allocations[0].PredecessorRetirementID = guard.RetirementID
	plan, err := a.Apply(context.Background(), replacement)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Quarantines) != 0 || len(s.state.ReusableQuarantines) != 0 || strings.Contains(plan.Script, "reladock-quarantine-"+guard.ID) {
		t.Fatalf("quarantine was not consumed in the successful replacement: %+v", s.state)
	}
	a = newTestAdapter(t, s, r, &mutexLock{}, goodVerify)
	if _, err = a.ApplyVerified(context.Background(), replacement); err != nil {
		t.Fatal("consumed predecessor lineage did not survive restart", err)
	}
}

func TestFailedQuarantineTakeoverRestoresQuarantineAndCandidateGuard(t *testing.T) {
	base := fixtureV2()
	retirement := base.Retirements[0]
	quarantine := AllocationGuard{ID: retirement.AllocationID, RetirementID: retirement.ID, Address: retirement.Address, Protocol: retirement.Protocol, Port: retirement.Port, State: "reusable"}
	empty := base
	empty.Rules, empty.Allocations, empty.Retirements = nil, nil, nil
	o := options()
	o.quarantines = []AllocationGuard{quarantine}
	good, err := Compile(context.Background(), empty, o)
	if err != nil {
		t.Fatal(err)
	}
	s := &memoryStore{state: State{NodeID: empty.NodeID, HighWater: empty.Generation, Good: &good, ReusableQuarantines: []AllocationGuard{quarantine}}}
	r := &fakeRunner{exists: true, token: planToken(good)}
	verify := func(_ context.Context, plan Plan) error {
		if plan.Generation > empty.Generation && len(plan.Quarantines) == 0 {
			return errors.New("injected takeover verification failure")
		}
		return nil
	}
	a := newTestAdapter(t, s, r, &mutexLock{}, verify)
	replacement := empty
	replacement.Generation++
	replacement.Rules = []platform.ConfigRule{{ID: "replacement", TenantID: "tenant-a", RuleInput: platform.RuleInput{Name: "Replacement", NodeID: empty.NodeID, Protocol: quarantine.Protocol, TargetHost: "198.51.100.3", TargetPort: 443, BandwidthMbps: 100}, ListenPort: quarantine.Port, Version: 1, Status: "pending-agent"}}
	replacement.Allocations = []platform.ConfigAllocation{{ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", RuleID: "replacement", Protocol: quarantine.Protocol, Address: quarantine.Address, Port: quarantine.Port, PredecessorRetirementID: quarantine.RetirementID}}
	if _, err = a.Apply(context.Background(), replacement); err == nil || errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("takeover failure did not complete safe rollback: %v", err)
	}
	if s.state.Pending || s.state.Good == nil || len(s.state.Good.Quarantines) != 1 || len(s.state.Good.ForwardGuards) != 1 || len(s.state.TombstoneGuards) != 1 || len(s.state.ReusableQuarantines) != 1 {
		t.Fatalf("rollback lost quarantine or candidate guard: %+v", s.state)
	}
	a = newTestAdapter(t, s, r, &mutexLock{}, goodVerify)
	next := empty
	next.Generation += 2
	if _, err = a.Apply(context.Background(), next); err != nil {
		t.Fatal("safe takeover rollback did not survive restart", err)
	}
}

func TestSchemaV2AllocationIdentityCannotMoveAcrossGenerations(t *testing.T) {
	s := &memoryStore{}
	r := &fakeRunner{}
	a := newTestAdapter(t, s, r, &mutexLock{}, goodVerify)
	base := fixtureV2()
	base.Retirements = nil
	if _, err := a.Apply(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	moved := base
	moved.Generation++
	moved.Rules = append([]platform.ConfigRule(nil), base.Rules...)
	moved.Allocations = append([]platform.ConfigAllocation(nil), base.Allocations...)
	moved.Rules[0].ListenPort++
	moved.Allocations[0].Port++
	if _, err := a.Apply(context.Background(), moved); err == nil {
		t.Fatal("same allocation ID moved to another tuple")
	}
	replaced := base
	replaced.Generation++
	replaced.Allocations = append([]platform.ConfigAllocation(nil), base.Allocations...)
	replaced.Allocations[0].ID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	if _, err := a.Apply(context.Background(), replaced); err == nil {
		t.Fatal("occupied tuple changed allocation ID without predecessor")
	}
}

func TestConcurrentAdaptersShareSerializationAndRejectDuplicate(t *testing.T) {
	s := &memoryStore{}
	r := &fakeRunner{}
	lock := &mutexLock{}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := newTestAdapter(t, s, r, lock, goodVerify).Apply(context.Background(), fixture())
			errs <- e
		}()
	}
	wg.Wait()
	close(errs)
	success, stale := 0, 0
	for e := range errs {
		if e == nil {
			success++
		} else if errors.Is(e, ErrStale) {
			stale++
		} else {
			t.Fatal(e)
		}
	}
	if success != 1 || stale != 1 {
		t.Fatalf("success %d stale %d", success, stale)
	}
}
