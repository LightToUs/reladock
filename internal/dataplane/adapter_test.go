package dataplane

import (
	"context"
	"errors"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
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
	calls   []call
	failAt  map[int]bool
	foreign bool
	exists  bool
	token   string
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
		comment := owner
		if r.foreign {
			comment = "foreign"
		}
		chain := ""
		if r.token != "" {
			chain = `,{"chain":{"name":"relay","comment":"` + r.token + `"}}`
		}
		return []byte(`{"nftables":[{"table":{"family":"ip","name":"reladock","comment":"` + comment + `"}}` + chain + `]}`), nil
	}
	if reflect.DeepEqual(args, []string{"-f", "-"}) {
		if strings.Contains(input, "chain forward") {
			r.exists = true
			r.token = regexp.MustCompile(`reladock-plan-[0-9a-f]+`).FindString(input)
		}
	}
	return nil, nil
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
