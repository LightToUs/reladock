package shaping

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"reladock/internal/platform"
)

var ErrStale = errors.New("shaping generation must exceed persisted high-water mark")
var ErrRecoveryRequired = errors.New("shaping recovery/operator reconciliation required")

type ClassState struct {
	ID       string
	Parent   string
	Kind     string
	RateBits uint64
	CeilBits uint64
}
type FilterState struct {
	Kind     string
	Parent   string
	Protocol string
	Priority int
	Mark     uint32
	Mask     uint32
	ClassID  string
	Drop     bool
}

// Foreign covers unrecognized qdiscs, actions, classes or filter attributes.
// Backends must fail closed when their readback cannot be interpreted safely.
type Snapshot struct {
	NamespaceID     string
	Device          string
	IfIndex         int
	Alias           string
	RootKind        string
	RootHandle      string
	RootDefault     uint32
	RootFingerprint string
	Classes         []ClassState
	Filters         []FilterState
	Foreign         bool
}
type Backend interface {
	Inspect(context.Context, Lease) (Snapshot, error)
	RunTC(context.Context, Lease, []string) error
}
type Store interface {
	Load(context.Context) (State, error)
	Save(context.Context, State) error
}
type Locker interface {
	Lock(context.Context) (func(), error)
}
type State struct {
	HighWater       int64
	Pending         bool
	Good            *Plan
	PriorGood       *Plan
	Candidate       *Plan
	Restoring       bool
	VerifiedDigests []string
}
type Adapter struct {
	opts    Options
	backend Backend
	store   Store
	lock    Locker
}

func NewAdapter(o Options, b Backend, s Store, l Locker) (*Adapter, error) {
	if e := validateOptions(o); e != nil {
		return nil, e
	}
	if b == nil || s == nil || l == nil {
		return nil, errors.New("backend, durable store and namespace-wide lease lock are required")
	}
	return &Adapter{o, b, s, l}, nil
}

func (a *Adapter) load(ctx context.Context) (State, error) {
	s, e := a.store.Load(ctx)
	if e != nil {
		return s, e
	}
	if s.HighWater == 0 && !s.Pending && !s.Restoring && s.Good == nil && s.PriorGood == nil && s.Candidate == nil && len(s.VerifiedDigests) == 0 {
		return s, nil
	}
	if s.HighWater < 1 || s.Pending != (s.Candidate != nil) || (!s.Pending && s.Restoring) {
		return s, ErrRecoveryRequired
	}
	for _, p := range []*Plan{s.Good, s.PriorGood, s.Candidate} {
		if p == nil {
			continue
		}
		if !reflect.DeepEqual(p.Options, a.opts) || p.Config.Generation > s.HighWater {
			return s, errors.New("persisted shaping lease/version differs")
		}
		q, e := compile(p.Config, a.opts, p.Classes)
		if e != nil || !reflect.DeepEqual(q, *p) {
			return s, errors.New("invalid persisted shaping plan")
		}
	}
	if s.Candidate != nil && !s.Restoring && s.Candidate.Config.Generation != s.HighWater {
		return s, ErrRecoveryRequired
	}
	return s, nil
}

func expected(p Plan) ([]ClassState, []FilterState) {
	link := uint64(p.Options.Lease.LinkMbps) * 1000000
	classes := []ClassState{{ID: ParentClass, Parent: RootHandle, Kind: "htb", RateBits: link, CeilBits: link}, {ID: DefaultClass, Parent: ParentClass, Kind: "htb", RateBits: 8, CeilBits: link}}
	var filters []FilterState
	for _, c := range p.Classes {
		classes = append(classes, ClassState{ID: c.ClassID, Parent: ParentClass, Kind: "htb", RateBits: 8, CeilBits: uint64(c.CeilMbps) * 1000000})
		filters = append(filters, FilterState{Kind: "fw", Parent: RootHandle, Protocol: "ip", Priority: 10, Mark: c.Mark, Mask: MarkMask, ClassID: c.ClassID})
	}
	filters = append(filters, FilterState{Kind: "fw", Parent: RootHandle, Protocol: "ip", Priority: 20, Mark: uint32(p.Options.Lease.MarkNamespace) << 24, Mask: 0xff000000, Drop: true})
	return classes, filters
}

func replaceableDefault(s Snapshot, l Lease) bool {
	return validLease(l) && l.ReplaceDefaultQdisc && s.NamespaceID == l.NamespaceID && s.Device == l.Device && s.IfIndex == l.IfIndex && s.Alias == l.Alias() && !s.Foreign && s.RootKind == "fq" && s.RootHandle == "0:" && s.RootFingerprint == l.DefaultQdiscFingerprint && len(s.Classes) == 0 && len(s.Filters) == 0
}

func (a *Adapter) inspect(ctx context.Context, s State) (Snapshot, error) {
	snap, e := a.backend.Inspect(ctx, a.opts.Lease)
	if e != nil {
		return snap, e
	}
	l := a.opts.Lease
	if snap.Foreign || snap.NamespaceID != l.NamespaceID || snap.Device != l.Device || snap.IfIndex != l.IfIndex || snap.Alias != l.Alias() {
		return snap, fmt.Errorf("interface lease mismatch: namespace=%s device=%s ifindex=%d alias=%s foreign=%t root=%s/%s filters=%+v", snap.NamespaceID, snap.Device, snap.IfIndex, snap.Alias, snap.Foreign, snap.RootKind, snap.RootHandle, snap.Filters)
	}
	idle := snap.RootKind == "" || (snap.RootKind == "noqueue" && snap.RootHandle == "0:")
	if idle {
		if len(snap.Classes) > 0 || len(snap.Filters) > 0 {
			return snap, errors.New("orphan/foreign tc objects")
		}
		return snap, nil
	}
	if replaceableDefault(snap, l) {
		return snap, nil
	}
	if snap.RootKind != "htb" || snap.RootHandle != RootHandle || snap.RootDefault != 65535 || (s.Good == nil && s.Candidate == nil) {
		return snap, errors.New("refusing to adopt or overwrite an external root qdisc")
	}
	var allowedC []ClassState
	var allowedF []FilterState
	for _, p := range []*Plan{s.Good, s.Candidate} {
		if p != nil {
			c, f := expected(*p)
			allowedC = append(allowedC, c...)
			allowedF = append(allowedF, f...)
		}
	}
	seenC := map[string]bool{}
	for _, v := range snap.Classes {
		if seenC[v.ID] {
			return snap, errors.New("duplicate tc class")
		}
		seenC[v.ID] = true
		found := false
		for _, c := range allowedC {
			if v == c {
				found = true
				break
			}
		}
		if !found {
			return snap, errors.New("external or modified tc class")
		}
	}
	seenF := map[uint64]bool{}
	for _, v := range snap.Filters {
		key := uint64(v.Mark)<<32 | uint64(v.Mask)
		if seenF[key] {
			return snap, errors.New("duplicate tc filter")
		}
		seenF[key] = true
		found := false
		for _, f := range allowedF {
			if v == f {
				found = true
				break
			}
		}
		if !found {
			return snap, errors.New("external or modified tc filter")
		}
	}
	return snap, nil
}

func (a *Adapter) install(ctx context.Context, p Plan, s State) error {
	snap, e := a.inspect(ctx, s)
	if e != nil {
		return e
	}
	if snap.RootKind == "" || snap.RootKind == "noqueue" {
		// add refuses a raced-in external root.
		if e = a.backend.RunTC(ctx, a.opts.Lease, []string{"qdisc", "add", "dev", a.opts.Lease.Device, "root", "handle", RootHandle, "htb", "default", "ffff"}); e != nil {
			return e
		}
	} else if snap.RootKind == "fq" && snap.RootHandle == "0:" {
		if e = a.backend.RunTC(ctx, a.opts.Lease, []string{"qdisc", "replace", "dev", a.opts.Lease.Device, "root", "handle", RootHandle, "htb", "default", "ffff"}); e != nil {
			return e
		}
	}
	// Recovery may use the stricter combination of Good/Candidate. Authorize
	// those intended classes during the sequential, non-atomic restoration.
	checkState := s
	checkState.Candidate = &p
	for _, cmd := range p.Commands {
		// Preserve both original candidates and the restoration in the allowed
		// readback lineage, including halfway changed class ceilings.
		snap, e = a.inspect(ctx, s)
		if e != nil {
			if _, e2 := a.inspect(ctx, checkState); e2 != nil {
				return e
			}
		}
		if e = a.backend.RunTC(ctx, a.opts.Lease, cmd); e != nil {
			return e
		}
	}
	snap, e = a.inspect(ctx, State{Good: &p})
	if e != nil {
		return e
	}
	cs, fs := expected(p)
	if len(snap.Classes) != len(cs) || len(snap.Filters) != len(fs) || snap.RootKind != "htb" {
		return errors.New("incomplete tc readback")
	}
	return nil
}

func (a *Adapter) Apply(ctx context.Context, cfg platform.NodeConfig) (Plan, error) {
	unlock, e := a.lock.Lock(ctx)
	if e != nil {
		return Plan{}, e
	}
	defer unlock()
	s, e := a.load(ctx)
	if e != nil {
		return Plan{}, e
	}
	if s.Pending {
		return Plan{}, ErrRecoveryRequired
	}
	if cfg.Generation <= s.HighWater {
		return Plan{}, ErrStale
	}
	return a.applyLocked(ctx, cfg, s)
}

func (a *Adapter) applyLocked(ctx context.Context, cfg platform.NodeConfig, s State) (Plan, error) {
	var history []Class
	if s.Good != nil {
		history = s.Good.Classes
	}
	p, e := compile(cfg, a.opts, history)
	if e != nil {
		return Plan{}, e
	}
	if _, e = a.inspect(ctx, s); e != nil {
		return Plan{}, e
	}
	s.HighWater = cfg.Generation
	s.Pending = true
	s.Candidate = &p
	s.PriorGood = s.Good
	if e = a.store.Save(ctx, s); e != nil {
		return Plan{}, fmt.Errorf("persist shaping intent: %w", e)
	}
	if e = a.install(ctx, p, s); e != nil {
		return Plan{}, a.failed(ctx, s, e, false)
	}
	done := s
	done.Pending = false
	done.Good = &p
	done.Candidate = nil
	done.Restoring = false
	register(&done, p)
	if e = a.store.Save(ctx, done); e != nil {
		return Plan{}, a.failed(ctx, s, e, true)
	}
	return p, nil
}

func recoveryPlan(s State) (Plan, error) {
	if s.Candidate == nil {
		return Plan{}, ErrRecoveryRequired
	}
	if s.Restoring {
		return *s.Candidate, nil
	}
	cfg := s.Candidate.Config
	cfg.Rules = nil
	o := s.Candidate.Options
	byID := map[string]Class{}
	for _, p := range []*Plan{s.Good, s.Candidate} {
		if p == nil {
			continue
		}
		for _, c := range p.Classes {
			key := c.TenantID + "/" + c.RuleID
			if prev, ok := byID[key]; ok && prev.CeilMbps < c.CeilMbps {
				c.CeilMbps = prev.CeilMbps
			}
			c.Active = false
			byID[key] = c
		}
	}
	if s.Good != nil {
		cfg = s.Good.Config
		cfg.Rules = append([]platform.ConfigRule(nil), cfg.Rules...)
		for i := range cfg.Rules {
			r := &cfg.Rules[i]
			c := byID[r.TenantID+"/"+r.ID]
			r.BandwidthMbps = c.CeilMbps
		}
	}
	var history []Class
	for _, c := range byID {
		history = append(history, c)
	}
	return compile(cfg, o, history)
}

func (a *Adapter) restore(ctx context.Context, s State) error {
	p, e := recoveryPlan(s)
	if e != nil {
		return e
	}
	if e = a.install(ctx, p, s); e != nil {
		return e
	}
	s.Pending = false
	s.Good = &p
	s.Candidate = nil
	s.Restoring = false
	register(&s, p)
	if e = a.store.Save(ctx, s); e != nil {
		return fmt.Errorf("persist shaping restoration: %w", e)
	}
	return nil
}

func digest(p Plan) string { b, _ := json.Marshal(p); return fmt.Sprintf("%x", sha256.Sum256(b)) }
func registered(s State, p Plan) bool {
	d := digest(p)
	for _, v := range s.VerifiedDigests {
		if v == d {
			return true
		}
	}
	return false
}
func register(s *State, p Plan) {
	if registered(*s, p) {
		return
	}
	s.VerifiedDigests = append(append([]string(nil), s.VerifiedDigests...), digest(p))
}

// Ensure verifies an identical persisted Good, repairing a compatible missing
// tree only after durable intent. A newer generation follows normal Apply.
// The caller must keep nft forwarding frozen across all mutating operations.
func (a *Adapter) Ensure(ctx context.Context, cfg platform.NodeConfig) (Plan, error) {
	unlock, e := a.lock.Lock(ctx)
	if e != nil {
		return Plan{}, e
	}
	defer unlock()
	s, e := a.load(ctx)
	if e != nil {
		return Plan{}, e
	}
	if s.Pending {
		return Plan{}, ErrRecoveryRequired
	}
	if cfg.Generation > s.HighWater {
		return a.applyLocked(ctx, cfg, s)
	}
	if s.Good == nil || cfg.Generation != s.Good.Config.Generation {
		return Plan{}, ErrStale
	}
	p, e := compile(cfg, a.opts, s.Good.Classes)
	if e != nil {
		return Plan{}, e
	}
	if !reflect.DeepEqual(p, *s.Good) {
		return Plan{}, errors.New("same shaping generation has different content")
	}
	snap, e := a.inspect(ctx, s)
	if e != nil {
		return Plan{}, e
	}
	cs, fs := expected(p)
	if snap.RootKind == "htb" && len(snap.Classes) == len(cs) && len(snap.Filters) == len(fs) {
		return p, nil
	}
	return a.restoreTarget(ctx, p, s)
}
func (a *Adapter) ApplyVerified(ctx context.Context, cfg platform.NodeConfig) (Plan, error) {
	return a.Ensure(ctx, cfg)
}

// Verify is strictly read-only: it requires the persisted Good configuration
// and a complete matching runtime tree. It never installs/repairs tc objects or
// saves state, and is suitable for checks after nft traffic has been enabled.
func (a *Adapter) Verify(ctx context.Context, cfg platform.NodeConfig) error {
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
	if s.Good == nil || cfg.Generation != s.Good.Config.Generation {
		return errors.New("verification requires the persisted Good generation")
	}
	p, e := compile(cfg, a.opts, s.Good.Classes)
	if e != nil {
		return e
	}
	if !reflect.DeepEqual(p, *s.Good) {
		return errors.New("verification config does not match persisted Good")
	}
	snap, e := a.inspect(ctx, s)
	if e != nil {
		return e
	}
	cs, fs := expected(p)
	if snap.RootKind != "htb" || len(snap.Classes) != len(cs) || len(snap.Filters) != len(fs) {
		return errors.New("verification found an incomplete shaping tree; read-only check will not repair it")
	}
	return nil
}

// Restore accepts only a canonical plan whose digest was durably registered
// after verification. It never lowers HighWater or discards historical marks.
// The resulting ceiling may be stricter than the requested old plan.
func (a *Adapter) Restore(ctx context.Context, old Plan) (Plan, error) {
	unlock, e := a.lock.Lock(ctx)
	if e != nil {
		return Plan{}, e
	}
	defer unlock()
	s, e := a.load(ctx)
	if e != nil {
		return Plan{}, e
	}
	if s.Pending {
		return Plan{}, ErrRecoveryRequired
	}
	return a.restoreVerifiedLocked(ctx, old, s)
}

func (a *Adapter) restoreVerifiedLocked(ctx context.Context, old Plan, s State) (Plan, error) {
	canonical, e := compile(old.Config, a.opts, old.Classes)
	if e != nil || !reflect.DeepEqual(canonical, old) || !registered(s, old) {
		return Plan{}, errors.New("restore requires a previously verified canonical plan on the same lease")
	}
	if s.Good == nil {
		return Plan{}, ErrRecoveryRequired
	}
	// Build the safe union using the requested old plan as the business config,
	// and current Good as the candidate whose classifiers must remain reserved.
	p, e := recoveryPlan(State{Good: &old, Candidate: s.Good})
	if e != nil {
		return Plan{}, e
	}
	return a.restoreTarget(ctx, p, s)
}

// RestoreConfig retrieves a matching verified Good/PriorGood after process
// restart. Digest-only older history remains usable via Restore(fullPlan), but
// never authorizes compiling an arbitrary old NodeConfig into a restore target.
func (a *Adapter) RestoreConfig(ctx context.Context, cfg platform.NodeConfig) (Plan, error) {
	unlock, e := a.lock.Lock(ctx)
	if e != nil {
		return Plan{}, e
	}
	defer unlock()
	s, e := a.load(ctx)
	if e != nil {
		return Plan{}, e
	}
	if s.Pending {
		return Plan{}, ErrRecoveryRequired
	}
	normalized, e := Compile(cfg, a.opts)
	if e != nil {
		return Plan{}, e
	}
	for _, p := range []*Plan{s.Good, s.PriorGood} {
		if p != nil && reflect.DeepEqual(normalized.Config, p.Config) && registered(s, *p) {
			return a.restoreVerifiedLocked(ctx, *p, s)
		}
	}
	return Plan{}, errors.New("config does not match retrievable verified Good/PriorGood")
}

// PrepareEmpty is a read-only check for an externally frozen/empty dataplane.
// It neither supplies packet markings nor changes classes/generation/state.
func (a *Adapter) PrepareEmpty(ctx context.Context) error {
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
	snap, e := a.inspect(ctx, s)
	if e != nil {
		return e
	}
	if s.Good == nil {
		return nil
	}
	cs, fs := expected(*s.Good)
	if snap.RootKind != "htb" || len(snap.Classes) != len(cs) || len(snap.Filters) != len(fs) {
		return errors.New("existing shaping tree incomplete; empty preparation is verify-only")
	}
	return nil
}

func (a *Adapter) restoreTarget(ctx context.Context, p Plan, s State) (Plan, error) {
	if _, e := a.inspect(ctx, s); e != nil {
		return Plan{}, e
	}
	s.Pending = true
	s.Restoring = true
	s.Candidate = &p
	if e := a.store.Save(ctx, s); e != nil {
		return Plan{}, fmt.Errorf("persist controlled restore intent: %w", e)
	}
	if e := a.restore(ctx, s); e != nil {
		return Plan{}, errors.Join(ErrRecoveryRequired, e)
	}
	return p, nil
}
func (a *Adapter) failed(ctx context.Context, s State, cause error, commitUncertain bool) error {
	recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	var intentErr error
	if commitUncertain {
		intentErr = a.store.Save(recovery, s)
	}
	restoreErr := a.restore(recovery, s)
	if intentErr != nil || restoreErr != nil {
		return errors.Join(cause, ErrRecoveryRequired, intentErr, restoreErr)
	}
	return cause
}
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
	if e = a.restore(ctx, s); e != nil {
		return errors.Join(ErrRecoveryRequired, e)
	}
	return nil
}
