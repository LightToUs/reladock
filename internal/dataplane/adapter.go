package dataplane

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"reladock/internal/platform"
)

var ErrStale = errors.New("generation must exceed persisted high-water mark")
var ErrRecoveryRequired = errors.New("unfinished transaction: explicit recovery required")
var ErrMeterIdentity = errors.New("metering WAL identity differs from persisted dataplane; explicit reconciliation required")

// Runner executes only nft, with a literal argument vector and stdin batch.
type Runner interface {
	Run(context.Context, []string, string) ([]byte, error)
}

// StateStore must durably and atomically replace state, under the same Locker.
// Its directory is privileged local state, not a source of untrusted scripts.
type StateStore interface {
	Load(context.Context) (State, error)
	Save(context.Context, State) error
}

// Locker must serialize every writer of this table, across processes too.
type Locker interface {
	Lock(context.Context) (unlock func(), err error)
}

// Verifier performs post-apply local checks. Success is not a platform ACK or a
// promise of end-to-end health. A verifier is required, including for rollback.
type Verifier func(context.Context, Plan) error
type State struct {
	NodeID    string
	HighWater int64
	Pending   bool
	Good      *Plan
	// GoodDesired preserves the exact controller payload before DNS pinning,
	// permitting lost-ACK retries without re-resolving a changed DNS answer.
	GoodDesired   *platform.NodeConfig
	MeterIdentity string
	// Candidate is durable before kernel execution. Recovery must know every
	// candidate allocation even if its conntrack entries outlive rollback.
	Candidate *Plan
	// Recovery is persisted before a rollback rebuild changes counter identity.
	// It remains separate from Candidate so its failed allocations stay fenced.
	Recovery         *Plan
	PreviousRecovery *Plan
	// Tombstones are failed candidate allocations which must never be reused
	// until a separate explicit conntrack reconciliation (not implemented).
	Tombstones []Allocation
}
type Adapter struct {
	options Options
	runner  Runner
	store   StateStore
	lock    Locker
	verify  Verifier
	hooks   TransitionHooks
}

// TransitionHooks runs under the same cross-process network/state lock. The
// adapter first freezes only its relay chain, so the final counter sample is
// stable. An unavailable WAL leaves traffic frozen, never silently unmetered.
type TransitionHooks interface {
	BeforeReplace(context.Context, Plan) error
	AfterReplace(context.Context, Plan) error
}

// TransitionPreparer installs and verifies dependent resources (such as tc)
// while old traffic is frozen and before any new forwarding is made visible.
type TransitionPreparer interface {
	PrepareReplace(context.Context, Plan) error
}

type TransitionBudget interface {
	QuotaUsed(context.Context, platform.NodeConfig) (map[string]int64, error)
}

type MeterIdentityProvider interface{ MeterIdentity() (string, error) }

func (a *Adapter) bindMeter(s *State) error {
	h, ok := a.hooks.(MeterIdentityProvider)
	if !ok {
		if s.MeterIdentity != "" {
			return ErrMeterIdentity
		}
		return nil
	}
	id, e := h.MeterIdentity()
	if e != nil {
		return e
	}
	if len(id) < 1 || len(id) > 128 {
		return ErrMeterIdentity
	}
	if s.HighWater == 0 && s.Good == nil && s.Candidate == nil {
		s.MeterIdentity = id
		return nil
	}
	if s.MeterIdentity != id {
		return errors.Join(ErrRecoveryRequired, ErrMeterIdentity)
	}
	return nil
}

// SetTransitionHooks must be called before the adapter is used concurrently.
func (a *Adapter) SetTransitionHooks(h TransitionHooks) { a.hooks = h }

func NewAdapter(o Options, r Runner, s StateStore, l Locker, v Verifier) (*Adapter, error) {
	if !identifier.MatchString(o.NodeID) || r == nil || s == nil || l == nil || v == nil {
		return nil, errors.New("node identity and all adapter dependencies are required")
	}
	if _, e := ipv4(o.SNATAddress); e != nil {
		return nil, e
	}
	return &Adapter{options: o, runner: r, store: s, lock: l, verify: v}, nil
}

func (a *Adapter) load(ctx context.Context) (State, error) {
	s, e := a.store.Load(ctx)
	if e != nil {
		return s, fmt.Errorf("load state: %w", e)
	}
	if s.NodeID == "" && s.HighWater == 0 && !s.Pending && s.Good == nil && s.Candidate == nil && s.Recovery == nil && s.PreviousRecovery == nil && len(s.Tombstones) == 0 {
		e = a.bindMeter(&s)
		return s, e
	}
	if s.NodeID != a.options.NodeID || s.HighWater < 1 {
		return s, errors.New("state node/high-water mismatch")
	}
	for _, g := range []*Plan{s.Good, s.Candidate, s.Recovery, s.PreviousRecovery} {
		if g == nil {
			continue
		}
		// Recompile pinned literal targets without a resolver, so corrupt on-disk
		// Script can never become executable input and rollback never re-resolves DNS.
		p, e := Compile(ctx, g.Config, Options{NodeID: a.options.NodeID, SNATAddress: g.SNATAddress, retired: g.Allocations, incarnation: g.Incarnation, Shaping: g.Shaping, builtAt: g.BuiltAt, quotaUsed: g.QuotaUsed})
		if e != nil || !reflect.DeepEqual(p, *g) || g.Generation > s.HighWater {
			return s, errors.New("invalid persisted plan")
		}
	}
	if s.Pending && (s.Candidate == nil || s.Candidate.Generation != s.HighWater) {
		return s, errors.Join(ErrRecoveryRequired, errors.New("pending state lacks its complete candidate; manual reconciliation required"))
	}
	if !s.Pending && s.Candidate != nil {
		return s, errors.New("candidate without pending intent")
	}
	if !s.Pending && (s.Recovery != nil || s.PreviousRecovery != nil) {
		return s, errors.New("recovery plan without pending intent")
	}
	if s.Good != nil && s.Candidate != nil && (s.Good.Config.ListenAddress != s.Candidate.Config.ListenAddress || s.Good.SNATAddress != s.Candidate.SNATAddress) {
		return s, errors.New("persisted candidate changes ingress/SNAT")
	}
	known := map[Allocation]bool{}
	for _, p := range []*Plan{s.Good, s.Candidate} {
		if p != nil {
			for _, v := range p.Allocations {
				known[v] = true
			}
		}
	}
	for _, v := range s.Tombstones {
		if !known[v] {
			return s, errors.New("tombstone absent from persisted plan guards")
		}
	}
	if s.Good != nil && intersectsActive(s.Good.Config, s.Tombstones) {
		return s, errors.New("last-good reuses a fenced allocation")
	}
	e = a.bindMeter(&s)
	return s, e
}

func activeAllocations(cfg platform.NodeConfig) map[Allocation]bool {
	active := map[Allocation]bool{}
	for _, r := range cfg.Rules {
		if r.Status == "pending-agent" {
			for _, proto := range protocols(r.Protocol) {
				active[Allocation{cfg.ListenAddress, proto, r.ListenPort}] = true
			}
		}
	}
	return active
}

func intersectsActive(cfg platform.NodeConfig, blocked []Allocation) bool {
	active := activeAllocations(cfg)
	for _, v := range blocked {
		if active[v] {
			return true
		}
	}
	return false
}

// inspect checks a fixed, marked table before any replacement. An absent table
// is distinct from an arbitrary command error; errors are never treated as absence.
func (a *Adapter) inspect(ctx context.Context) (bool, error) {
	out, e := a.runner.Run(ctx, []string{"-j", "list", "tables"}, "")
	if e != nil {
		return false, fmt.Errorf("inspect ownership: %w", e)
	}
	var doc struct {
		NFTables []struct {
			Table *struct {
				Family  string `json:"family"`
				Name    string `json:"name"`
				Comment string `json:"comment"`
			} `json:"table"`
		} `json:"nftables"`
	}
	if e = json.Unmarshal(out, &doc); e != nil || doc.NFTables == nil {
		return false, errors.New("invalid nft table inventory")
	}
	found := false
	for _, v := range doc.NFTables {
		if v.Table != nil && v.Table.Family == "ip" && v.Table.Name == Table {
			if found {
				return false, errors.New("duplicate table inventory")
			}
			found = true
		}
	}
	if found {
		// list tables may contain names only. Read the full table before trusting
		// the ownership comment; never treat a read failure as an absent table.
		out, e = a.runner.Run(ctx, []string{"-j", "list", "table", "ip", Table}, "")
		if e != nil {
			return false, e
		}
		doc.NFTables = nil
		if e = json.Unmarshal(out, &doc); e != nil {
			return false, e
		}
		tables := 0
		tableComment := ""
		for _, v := range doc.NFTables {
			if v.Table != nil && v.Table.Family == "ip" && v.Table.Name == Table {
				tables++
				tableComment = v.Table.Comment
			}
		}
		if tables != 1 {
			return false, errors.New("refusing table without platform ownership comment")
		}
		if tableComment == "" {
			comments, textErr := readNFTTextComments(ctx, a.runner)
			if textErr != nil {
				return false, textErr
			}
			tableComment = comments.Owner
		}
		if tableComment != owner {
			return false, errors.New("refusing table without platform ownership comment")
		}
	}
	return found, nil
}

func (a *Adapter) check(ctx context.Context, script string) error {
	_, e := a.runner.Run(ctx, []string{"-c", "-f", "-"}, script)
	return e
}
func (a *Adapter) execute(ctx context.Context, script string) error {
	_, e := a.runner.Run(ctx, []string{"-f", "-"}, script)
	return e
}

func (a *Adapter) Apply(ctx context.Context, cfg platform.NodeConfig) (Plan, error) {
	return a.apply(ctx, cfg, false)
}

// ApplyVerified permits an exact retry only after reading back the durable
// last-good kernel configuration. It never reapplies a stale generation.
func (a *Adapter) ApplyVerified(ctx context.Context, cfg platform.NodeConfig) (Plan, error) {
	return a.apply(ctx, cfg, true)
}

func sameConfigAllowingEmptyRules(a, b platform.NodeConfig) bool {
	if len(a.Rules) != 0 || len(b.Rules) != 0 {
		return reflect.DeepEqual(a, b)
	}
	a.Rules = nil
	b.Rules = nil
	return reflect.DeepEqual(a, b)
}

func (a *Adapter) apply(ctx context.Context, cfg platform.NodeConfig, retry bool) (Plan, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	unlock, e := a.lock.Lock(ctx)
	if e != nil {
		return Plan{}, e
	}
	defer unlock()
	s, e := a.load(ctx)
	if e != nil {
		if errors.Is(e, ErrMeterIdentity) {
			return Plan{}, errors.Join(e, a.freezeOwned(ctx))
		}
		return Plan{}, e
	}
	if s.Pending {
		return Plan{}, ErrRecoveryRequired
	}
	if cfg.Generation <= s.HighWater {
		exactDesired := s.GoodDesired != nil && reflect.DeepEqual(cfg, *s.GoodDesired)
		if s.GoodDesired == nil && s.Good != nil {
			exactDesired = sameConfigAllowingEmptyRules(cfg, s.Good.Config)
		}
		if retry && cfg.Generation == s.HighWater && s.Good != nil && exactDesired && s.Good.Generation == cfg.Generation {
			if exists, err := a.inspect(ctx); err != nil {
				return Plan{}, err
			} else if !exists {
				return Plan{}, errors.New("last-good table missing")
			}
			if err := a.verify(ctx, *s.Good); err != nil {
				return Plan{}, err
			}
			return *s.Good, nil
		}
		return Plan{}, ErrStale
	}
	o := a.options
	if a.hooks != nil {
		o.incarnation = newIncarnation()
	}
	if s.Good != nil {
		o.retired = s.Good.Allocations
	}
	p, e := Compile(ctx, cfg, o)
	if e != nil {
		return Plan{}, e
	}
	if s.Good != nil && (s.Good.Config.ListenAddress != p.Config.ListenAddress || s.Good.SNATAddress != p.SNATAddress) {
		return Plan{}, errors.New("ingress/SNAT migration requires separate conntrack reconciliation")
	}
	if s.Good != nil && !reflect.DeepEqual(s.Good.Shaping, p.Shaping) {
		return Plan{}, errors.New("shaping lease migration requires explicit reconciliation")
	}
	if intersectsActive(p.Config, s.Tombstones) {
		return Plan{}, errors.New("allocation fenced after failed apply; explicit conntrack reconciliation required")
	}
	exists, e := a.inspect(ctx)
	if e != nil {
		return Plan{}, e
	}
	if exists && s.Good == nil {
		return Plan{}, errors.New("owned table exists without last-good state; refusing adoption")
	}
	if e = a.check(ctx, p.Script); e != nil {
		return Plan{}, fmt.Errorf("nft check: %w", e)
	}
	s.NodeID = p.NodeID
	s.HighWater = p.Generation
	s.Pending = true
	s.Candidate = &p
	if e = a.store.Save(ctx, s); e != nil {
		return Plan{}, fmt.Errorf("persist write-ahead intent: %w", e)
	}
	if a.hooks != nil && exists {
		if e = a.freezeAndSample(ctx, *s.Good); e != nil {
			return Plan{}, errors.Join(ErrRecoveryRequired, e)
		}
	}
	if h, ok := a.hooks.(TransitionBudget); ok {
		o.quotaUsed, e = h.QuotaUsed(ctx, cfg)
		if e != nil {
			return Plan{}, errors.Join(ErrRecoveryRequired, e)
		}
		p, e = Compile(ctx, cfg, o)
		if e != nil {
			return Plan{}, errors.Join(ErrRecoveryRequired, e)
		}
		if e = a.check(ctx, p.Script); e != nil {
			return Plan{}, errors.Join(ErrRecoveryRequired, e)
		}
		s.Candidate = &p
		if e = a.store.Save(ctx, s); e != nil {
			return Plan{}, errors.Join(ErrRecoveryRequired, e)
		}
	}
	if h, ok := a.hooks.(TransitionPreparer); ok {
		if e = h.PrepareReplace(ctx, p); e != nil {
			return Plan{}, a.failed(ctx, s, e, false)
		}
	}
	if e = a.execute(ctx, p.Script); e != nil {
		return Plan{}, a.failed(ctx, s, fmt.Errorf("nft apply: %w", e), false)
	}
	if e = a.verify(ctx, p); e != nil {
		return Plan{}, a.failed(ctx, s, fmt.Errorf("post-apply verification: %w", e), false)
	}
	if a.hooks != nil {
		if e = a.hooks.AfterReplace(ctx, p); e != nil {
			return Plan{}, a.failed(ctx, s, e, false)
		}
	}
	committed := s
	committed.Pending = false
	committed.Good = &p
	desiredBytes, _ := json.Marshal(cfg)
	var desired platform.NodeConfig
	if e = json.Unmarshal(desiredBytes, &desired); e != nil {
		return Plan{}, a.failed(ctx, s, e, false)
	}
	committed.GoodDesired = &desired
	committed.Candidate = nil
	if e = a.store.Save(ctx, committed); e != nil {
		return Plan{}, a.failed(ctx, s, fmt.Errorf("persist last-good: %w", e), true)
	}
	return p, nil
}

func (a *Adapter) failed(ctx context.Context, s State, cause error, commitUncertain bool) error {
	// Cancellation of the failed operation must not suppress rollback. Bound
	// recovery separately; pending intent survives any unsuccessful recovery.
	recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	// Save may fail after rename but before directory fsync. Reassert the old
	// baseline and pending flag before attempting any rollback in that case.
	var intentErr error
	if commitUncertain {
		if e := a.store.Save(recovery, s); e != nil {
			intentErr = fmt.Errorf("persist recovery intent (commit uncertain): %w", e)
		}
	}
	// A storage failure must never suppress the kernel restoration attempt.
	// Runtime failures already have durable intent; commit failures may have
	// exposed the candidate, so reassert that intent best-effort above.
	restoreErr := a.restore(recovery, s)
	if intentErr != nil || restoreErr != nil {
		return errors.Join(cause, ErrRecoveryRequired, intentErr, restoreErr)
	}
	return cause
}

func (a *Adapter) restore(ctx context.Context, s State) error {
	if _, e := a.inspect(ctx); e != nil {
		return e
	}
	// Recompile the old business config with the union of all allocations,
	// rather than replaying a script that predates failed candidate conntrack.
	if s.Candidate == nil {
		return errors.New("cannot restore without durable candidate allocations")
	}
	var physical *Plan
	if a.hooks != nil {
		actual, e := a.actualPlan(ctx, s)
		if e != nil {
			return e
		}
		if actual != nil {
			physical = actual
			if e = a.freezeAndSample(ctx, *actual); e != nil {
				return e
			}
		}
	}
	cfg := s.Candidate.Config
	snat := s.Candidate.SNATAddress
	var history []Allocation
	if s.Good != nil {
		cfg = s.Good.Config
		snat = s.Good.SNATAddress
		history = append(history, s.Good.Allocations...)
	} else {
		cfg.Rules = nil
	}
	history = append(history, s.Candidate.Allocations...)
	history = append(history, s.Tombstones...)
	o := Options{NodeID: s.NodeID, SNATAddress: snat, retired: history, Shaping: a.options.Shaping}
	if h, ok := a.hooks.(TransitionBudget); ok {
		var err error
		o.quotaUsed, err = h.QuotaUsed(ctx, cfg)
		if err != nil {
			return err
		}
	}
	if a.hooks != nil {
		o.incarnation = newIncarnation()
	}
	p, e := Compile(ctx, cfg, o)
	if e != nil {
		return fmt.Errorf("compile rollback protection: %w", e)
	}
	// Only the prior Good's active tuples may forward. New active candidate
	// tuples become permanent exact tombstones, including initial failures.
	goodActive := map[Allocation]bool{}
	if s.Good != nil {
		goodActive = activeAllocations(s.Good.Config)
	}
	blocked := map[Allocation]bool{}
	for _, v := range s.Tombstones {
		blocked[v] = true
	}
	for v := range activeAllocations(s.Candidate.Config) {
		if !goodActive[v] {
			blocked[v] = true
		}
	}
	s.Tombstones = nil
	for _, v := range p.Allocations {
		if blocked[v] {
			s.Tombstones = append(s.Tombstones, v)
		}
	}
	if e := a.check(ctx, p.Script); e != nil {
		return fmt.Errorf("rollback check: %w", e)
	}
	if a.hooks != nil {
		s.PreviousRecovery = physical
		s.Recovery = &p
		if e = a.store.Save(ctx, s); e != nil {
			return errors.Join(ErrRecoveryRequired, fmt.Errorf("persist recovery incarnation: %w", e))
		}
	}
	if h, ok := a.hooks.(TransitionPreparer); ok {
		if e = h.PrepareReplace(ctx, p); e != nil {
			return e
		}
	}
	if e := a.execute(ctx, p.Script); e != nil {
		return fmt.Errorf("rollback apply: %w", e)
	}
	if e := a.verify(ctx, p); e != nil {
		return fmt.Errorf("rollback verification: %w", e)
	}
	if a.hooks != nil {
		if e := a.hooks.AfterReplace(ctx, p); e != nil {
			return e
		}
	}
	s.Pending = false
	s.Good = &p
	s.Candidate = nil
	s.Recovery = nil
	s.PreviousRecovery = nil
	if e := a.store.Save(ctx, s); e != nil {
		return fmt.Errorf("persist recovery: %w", e)
	}
	return nil
}

// Observe serializes a counter sample with replacement/recovery. A failed
// sample freezes the owned chain so WAL exhaustion cannot enable free traffic.
// A later successful configuration transition may unfreeze it after sampling.
func (a *Adapter) Observe(ctx context.Context, fn func(context.Context, Plan) error) error {
	if fn == nil {
		return errors.New("observer required")
	}
	unlock, e := a.lock.Lock(ctx)
	if e != nil {
		return e
	}
	defer unlock()
	s, e := a.load(ctx)
	if e != nil {
		if errors.Is(e, ErrMeterIdentity) {
			return errors.Join(e, a.freezeOwned(ctx))
		}
		return e
	}
	if s.Pending {
		return errors.Join(ErrRecoveryRequired, a.freezeOwned(ctx))
	}
	if s.Good == nil {
		return nil
	}
	if e = a.verify(ctx, *s.Good); e != nil {
		return errors.Join(e, a.freezeOwned(ctx))
	}
	if e = fn(ctx, *s.Good); e != nil {
		return errors.Join(e, a.freezeOwned(ctx))
	}
	return nil
}

func (a *Adapter) freezeOwned(ctx context.Context) error {
	stop, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	exists, e := a.inspect(stop)
	if e != nil {
		return e
	}
	if !exists {
		return ErrRecoveryRequired
	}
	return a.execute(stop, "insert rule ip "+Table+" relay drop comment \"reladock-meter-freeze\"\n")
}

// CheckMeterBinding validates local persistent state before any collector can
// assign an epoch. It never adopts existing counters into an empty/new WAL.
func (a *Adapter) CheckMeterBinding(ctx context.Context) error {
	unlock, e := a.lock.Lock(ctx)
	if e != nil {
		return e
	}
	defer unlock()
	_, e = a.load(ctx)
	if errors.Is(e, ErrMeterIdentity) {
		return errors.Join(e, a.freezeOwned(ctx))
	}
	return e
}

// StopOwned is a fail-closed operation for a runtime prerequisite that changed.
// Node/state ownership is checked before touching the platform's fixed chain.
func (a *Adapter) StopOwned(ctx context.Context) error {
	unlock, e := a.lock.Lock(ctx)
	if e != nil {
		return e
	}
	defer unlock()
	s, e := a.load(ctx)
	if e != nil && !errors.Is(e, ErrMeterIdentity) {
		return e
	}
	if s.Good == nil && s.Candidate == nil {
		return nil
	}
	return a.freezeOwned(ctx)
}

// Stable proves a completed local baseline after a rejected/rolled-back apply.
// Only a caller that receives nil may turn an apply error into a terminal safe
// failure. Pending state, a frozen chain or missing kernel table stays fenced.
func (a *Adapter) Stable(ctx context.Context) error {
	unlock, e := a.lock.Lock(ctx)
	if e != nil {
		return e
	}
	defer unlock()
	s, e := a.load(ctx)
	if e != nil {
		return e
	}
	if s.Pending {
		return ErrRecoveryRequired
	}
	if s.Good == nil {
		exists, e := a.inspect(ctx)
		if e != nil {
			return e
		}
		if exists {
			return ErrRecoveryRequired
		}
		return nil
	}
	return a.verify(ctx, *s.Good)
}

func newIncarnation() string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return fmt.Sprintf("kernel-%x", b)
}

func (a *Adapter) freezeAndSample(ctx context.Context, p Plan) error {
	if e := a.execute(ctx, "insert rule ip "+Table+" relay drop comment \"reladock-meter-freeze\"\n"); e != nil {
		return e
	}
	return a.hooks.BeforeReplace(ctx, p)
}

// Identify the physical incarnation even if a prior meter freeze added a drop
// rule. A failed/uncertain nft command is not proof that old counters survived.
func (a *Adapter) actualPlan(ctx context.Context, s State) (*Plan, error) {
	exists, e := a.inspect(ctx)
	if e != nil {
		return nil, e
	}
	if !exists {
		if s.Good == nil {
			return nil, nil
		}
		return nil, errors.New("metered table disappeared")
	}
	b, e := a.runner.Run(ctx, []string{"-j", "list", "table", "ip", Table}, "")
	if e != nil {
		return nil, e
	}
	var doc struct {
		NFTables []struct {
			Chain *struct{ Name, Comment string } `json:"chain"`
		} `json:"nftables"`
	}
	if e = json.Unmarshal(b, &doc); e != nil {
		return nil, e
	}
	relayComment, relayCount := "", 0
	for _, v := range doc.NFTables {
		if v.Chain != nil && v.Chain.Name == "relay" {
			relayCount++
			relayComment = v.Chain.Comment
		}
	}
	if relayCount != 1 {
		return nil, errors.New("unknown physical counter incarnation; refusing to discard counters")
	}
	if relayComment == "" {
		comments, textErr := readNFTTextComments(ctx, a.runner)
		if textErr != nil {
			return nil, textErr
		}
		relayComment = comments.Relay
	}
	for _, p := range []*Plan{s.Recovery, s.PreviousRecovery, s.Candidate, s.Good} {
		if p != nil && relayComment == planToken(*p) {
			return p, nil
		}
	}
	return nil, errors.New("unknown physical counter incarnation; refusing to discard counters")
}

// Recover restores the old business config with exact candidate drop guards
// after an interrupted/unsuccessful apply. With no Good it installs a guard-only
// table. The high-water mark is never lowered; candidates are never adopted.
func (a *Adapter) Recover(ctx context.Context) error {
	unlock, e := a.lock.Lock(ctx)
	if e != nil {
		return e
	}
	defer unlock()
	s, e := a.load(ctx)
	if e != nil {
		return e
	}
	if !s.Pending {
		return nil
	}
	if e := a.restore(ctx, s); e != nil {
		return errors.Join(ErrRecoveryRequired, e)
	}
	return nil
}
