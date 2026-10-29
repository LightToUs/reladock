package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"reladock/internal/dataplane"
	"reladock/internal/platform"
	"reladock/internal/shaping"
)

func TestClientReclaimsAfterUnbegunShortLeaseOrExplicitBeginConflict(t *testing.T) {
	for _, scenario := range []string{"short-lease", "stale-generation-409", "lost-begin-then-expired-409"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now()
			old := applyJob()
			old.LeaseUntil = now.Add(time.Minute)
			if scenario == "short-lease" {
				old.LeaseUntil = now.Add(time.Second)
			}
			fresh := old
			fresh.ID = "29834c51-0b88-4d85-8c17-c2805c9ef532"
			fresh.LeaseToken = "new-lease-token"
			fresh.LeaseUntil = now.Add(5 * time.Minute)
			var cfg platform.NodeConfig
			if e := json.Unmarshal(old.Payload, &cfg); e != nil {
				t.Fatal(e)
			}
			cfg.Generation++
			fresh.Generation = cfg.Generation
			fresh.Payload, _ = json.Marshal(cfg)
			sum := sha256.Sum256(fresh.Payload)
			fresh.Checksum = hex.EncodeToString(sum[:])
			dir := filepath.Join(t.TempDir(), "state")
			state, e := NewApplyState(dir)
			if e != nil {
				t.Fatal(e)
			}
			claims, begins, applies, reports := 0, 0, 0, 0
			c := Client{BaseURL: "https://control.test", NodeID: "hk-01", State: state, Now: func() time.Time { return now }, Applier: applyFunc(func(_ context.Context, got platform.NodeConfig) error {
				applies++
				if got.Generation != fresh.Generation {
					t.Fatal("applied obsolete job")
				}
				return nil
			})}
			lost := scenario == "lost-begin-then-expired-409"
			c.HTTP = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/claim") {
					claims++
					j := fresh
					if claims == 1 {
						j = old
					}
					raw, _ := json.Marshal(j)
					return agentResponse(200, string(raw)), nil
				}
				if strings.HasSuffix(r.URL.Path, "/begin") {
					begins++
					if strings.Contains(r.URL.Path, old.ID) {
						if lost {
							return nil, errors.New("begin result unavailable")
						}
						return agentResponse(409, `{}`), nil
					}
					return agentResponse(200, `{}`), nil
				}
				if strings.HasSuffix(r.URL.Path, "/report") {
					reports++
					var proof platform.ConfigReport
					json.NewDecoder(r.Body).Decode(&proof)
					if proof.LeaseToken != fresh.LeaseToken || proof.Checksum != fresh.Checksum || proof.Outcome != "applied" {
						t.Fatal("reported obsolete job", proof)
					}
					return agentResponse(200, `{}`), nil
				}
				t.Fatal("unexpected request", r.URL.Path)
				return nil, nil
			})}
			if done, e := c.Once(ctx); done || e == nil {
				t.Fatalf("first cycle done=%v err=%v", done, e)
			}
			if applies != 0 || reports != 0 {
				t.Fatal("unbegun configuration executed/reported")
			}
			if lost {
				pending, e := state.load(ctx)
				if e != nil || pending == nil || !pending.BeginAttempted || pending.BeginConfirmed {
					t.Fatalf("lost begin was discarded: %+v %v", pending, e)
				}
				// Insufficient remaining time alone is not proof that this attempted
				// begin failed. Do not even retry begin until expiry can resolve it.
				now = old.LeaseUntil.Add(-time.Second)
				priorBegins := begins
				if done, e := c.Once(ctx); done || e == nil {
					t.Fatal("uncertain short lease succeeded")
				}
				if pending, e = state.load(ctx); e != nil || pending == nil || begins != priorBegins || claims != 1 {
					t.Fatal("uncertain short lease was cleared/reclaimed")
				}
				now = old.LeaseUntil.Add(time.Second)
				lost = false
				if done, e := c.Once(ctx); done || e == nil {
					t.Fatal("conflicting obsolete begin succeeded")
				}
			}
			if pending, e := state.load(ctx); e != nil || pending != nil {
				t.Fatalf("safe-to-discard obsolete journal stuck: %+v %v", pending, e)
			}
			if scenario == "short-lease" && begins != 0 {
				t.Fatal("short lease attempted begin")
			}
			// A fresh Once, including after process restart, may claim a replacement
			// lease/new generation; the rejecting cycle itself never executes it.
			now = old.LeaseUntil.Add(2 * time.Second)
			c.State, e = NewApplyState(dir)
			if e != nil {
				t.Fatal(e)
			}
			if done, e := c.Once(ctx); !done || e != nil {
				t.Fatalf("next Once stayed stuck: done=%v err=%v", done, e)
			}
			if claims != 2 || applies != 1 || reports != 1 {
				t.Fatalf("claims=%d applies=%d reports=%d", claims, applies, reports)
			}
		})
	}
}

func TestClientNeverDiscardsConfirmedOrUncertainBeginOnUnauthorizedOrNetworkFailure(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		confirmed bool
		status    int
	}{
		{"unconfirmed-401", false, 401}, {"confirmed-401", true, 401}, {"confirmed-409", true, 409}, {"unconfirmed-network", false, 0}, {"unconfirmed-500", false, 500},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx := context.Background()
			dir := filepath.Join(t.TempDir(), "state")
			state, e := NewApplyState(dir)
			if e != nil {
				t.Fatal(e)
			}
			job := applyJob()
			now := job.LeaseUntil.Add(time.Minute)
			record := &applyRecord{Origin: "https://control.test", NodeID: "hk-01", Job: job, BeginAttempted: true, BeginConfirmed: scenario.confirmed}
			if e = state.save(ctx, record); e != nil {
				t.Fatal(e)
			}
			c := Client{BaseURL: record.Origin, NodeID: record.NodeID, State: state, Now: func() time.Time { return now }, Applier: applyFunc(func(context.Context, platform.NodeConfig) error {
				t.Fatal("applied without current begin confirmation")
				return nil
			})}
			c.HTTP = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				if !strings.HasSuffix(r.URL.Path, "/begin") {
					t.Fatal("reclaimed/reported instead of retaining fence", r.URL.Path)
				}
				if scenario.status == 0 {
					return nil, errors.New("begin rejected: HTTP 409")
				}
				return agentResponse(scenario.status, `{}`), nil
			})}
			for cycle := 0; cycle < 2; cycle++ {
				if done, e := c.Once(ctx); done || e == nil {
					t.Fatal("rejected/uncertain begin succeeded")
				}
				pending, e := state.load(ctx)
				if e != nil || pending == nil || pending.Job.LeaseToken != job.LeaseToken || !pending.BeginAttempted || pending.BeginConfirmed != scenario.confirmed {
					t.Fatalf("unsafe journal clear: %+v %v", pending, e)
				}
				c.State, e = NewApplyState(dir)
				if e != nil {
					t.Fatal(e)
				}
			}
		})
	}
}

type applyFunc func(context.Context, platform.NodeConfig) error

func (f applyFunc) Apply(ctx context.Context, c platform.NodeConfig) error { return f(ctx, c) }

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func agentResponse(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}
func applyJob() platform.ConfigJob {
	j := validJob()
	j.ID = "19834c51-0b88-4d85-8c17-c2805c9ef532"
	j.LeaseToken = "lease-token"
	j.LeaseUntil = time.Now().Add(time.Minute)
	return j
}

func TestClientApplyRequiresBeginThenAppliedReport(t *testing.T) {
	j := applyJob()
	raw, _ := json.Marshal(j)
	var steps []string
	state, e := NewApplyState(filepath.Join(t.TempDir(), "state"))
	if e != nil {
		t.Fatal(e)
	}
	c := Client{BaseURL: "https://control.test", NodeID: "hk-01", State: state, Applier: applyFunc(func(ctx context.Context, cfg platform.NodeConfig) error {
		steps = append(steps, "apply")
		if cfg.Generation != j.Generation {
			t.Fatal("wrong configuration")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("unbounded application")
		}
		return nil
	})}
	c.HTTP = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/agent/config/claim":
			steps = append(steps, "claim")
			return agentResponse(200, string(raw)), nil
		case "/agent/config/" + j.ID + "/begin":
			var report platform.ConfigReport
			if json.NewDecoder(r.Body).Decode(&report) != nil || report.Outcome != "applying" || report.LeaseToken != j.LeaseToken || report.Checksum != j.Checksum {
				t.Fatal("invalid begin proof")
			}
			steps = append(steps, "begin")
			return agentResponse(200, `{}`), nil
		case "/agent/config/" + j.ID + "/report":
			var report platform.ConfigReport
			json.NewDecoder(r.Body).Decode(&report)
			steps = append(steps, report.Outcome)
			return agentResponse(200, `{}`), nil
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
			return nil, nil
		}
	})}
	done, e := c.Once(context.Background())
	if e != nil || !done || strings.Join(steps, ",") != "claim,begin,apply,applied" {
		t.Fatalf("steps=%v done=%v error=%v", steps, done, e)
	}
}

func TestClientApplyDeniedBeginOrShortLeaseCannotApply(t *testing.T) {
	for _, which := range []string{"denied", "short", "begin-consumes-lease"} {
		t.Run(which, func(t *testing.T) {
			j := applyJob()
			if which == "short" {
				j.LeaseUntil = time.Now().Add(time.Second)
			}
			raw, _ := json.Marshal(j)
			state, e := NewApplyState(filepath.Join(t.TempDir(), "state"))
			if e != nil {
				t.Fatal(e)
			}
			var applied bool
			clock := time.Now()
			c := Client{BaseURL: "https://control.test", NodeID: "hk-01", State: state, Now: func() time.Time { return clock }, Applier: applyFunc(func(context.Context, platform.NodeConfig) error { applied = true; return nil })}
			c.HTTP = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/agent/config/claim" {
					return agentResponse(200, string(raw)), nil
				}
				if strings.HasSuffix(r.URL.Path, "/begin") {
					if which == "begin-consumes-lease" {
						clock = clock.Add(55 * time.Second)
						return agentResponse(200, `{}`), nil
					}
					return agentResponse(409, `{}`), nil
				}
				return agentResponse(200, `{}`), nil
			})}
			_, e = c.Once(context.Background())
			if e == nil || applied {
				t.Fatalf("applied=%v error=%v", applied, e)
			}
		})
	}
}

func TestClientApplyFailureReportsInternalError(t *testing.T) {
	j := applyJob()
	raw, _ := json.Marshal(j)
	state, e := NewApplyState(filepath.Join(t.TempDir(), "state"))
	if e != nil {
		t.Fatal(e)
	}
	var report platform.ConfigReport
	c := Client{BaseURL: "https://control.test", NodeID: "hk-01", State: state, Applier: applyFunc(func(context.Context, platform.NodeConfig) error {
		return &SafeApplyFailure{Err: errors.New("private adapter diagnostic")}
	})}
	c.HTTP = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/claim") {
			return agentResponse(200, string(raw)), nil
		}
		if strings.HasSuffix(r.URL.Path, "/report") {
			json.NewDecoder(r.Body).Decode(&report)
		}
		return agentResponse(200, `{}`), nil
	})}
	done, e := c.Once(context.Background())
	if e == nil || !done || report.Outcome != "failed" || report.ErrorCode != "internal-error" {
		t.Fatalf("report=%+v done=%v error=%v", report, done, e)
	}
}

func TestClientDurableExactReportRetryAcrossRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	j := applyJob()
	raw, _ := json.Marshal(j)
	state, e := NewApplyState(dir)
	if e != nil {
		t.Fatal(e)
	}
	var first []byte
	calls := 0
	c := Client{BaseURL: "https://control.test", NodeID: "hk-01", State: state, Applier: applyFunc(func(context.Context, platform.NodeConfig) error { calls++; return nil })}
	c.HTTP = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/claim") {
			return agentResponse(200, string(raw)), nil
		}
		if strings.HasSuffix(r.URL.Path, "/report") {
			body, _ := io.ReadAll(r.Body)
			if first == nil {
				first = body
			} else if string(first) != string(body) {
				t.Fatal("report changed during retry")
			}
			return nil, errors.New("response lost")
		}
		return agentResponse(200, `{}`), nil
	})}
	if _, e = c.Once(context.Background()); e == nil {
		t.Fatal("expected uncertain report")
	}
	restarted, e := NewApplyState(dir)
	if e != nil {
		t.Fatal(e)
	}
	c.State = restarted
	c.HTTP = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/report") {
			t.Fatalf("restart reclaimed/reapplied instead of exact report: %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != string(first) {
			t.Fatal("durable report mismatch")
		}
		return agentResponse(200, `{}`), nil
	})}
	if done, e := c.Once(context.Background()); e != nil || !done || calls != 1 {
		t.Fatalf("done=%v applies=%d error=%v", done, calls, e)
	}
}

func TestValidationOnlyNeverBeginsOrApplies(t *testing.T) {
	j := applyJob()
	raw, _ := json.Marshal(j)
	var outcome string
	c := Client{BaseURL: "https://control.test", NodeID: "hk-01"}
	c.HTTP = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/claim") {
			return agentResponse(200, string(raw)), nil
		}
		if !strings.HasSuffix(r.URL.Path, "/report") {
			t.Fatalf("validation mode began apply: %s", r.URL.Path)
		}
		var report platform.ConfigReport
		json.NewDecoder(r.Body).Decode(&report)
		outcome = report.Outcome
		return agentResponse(200, `{}`), nil
	})}
	if done, e := c.Once(context.Background()); e != nil || !done || outcome != "validated" {
		t.Fatalf("outcome=%s done=%v error=%v", outcome, done, e)
	}
}

func TestClientCrossProcessStateLockCoversClaimAndApply(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	state, e := NewApplyState(dir)
	if e != nil {
		t.Fatal(e)
	}
	other, e := NewApplyState(dir)
	if e != nil {
		t.Fatal(e)
	}
	unlock, e := state.Lock(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	var mu sync.Mutex
	network := false
	c := Client{BaseURL: "https://control.test", NodeID: "hk-01", State: other, Applier: applyFunc(func(context.Context, platform.NodeConfig) error { t.Fatal("apply without lock"); return nil }), HTTP: &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		mu.Lock()
		network = true
		mu.Unlock()
		return agentResponse(204, ""), nil
	})}}
	if _, e = c.Once(ctx); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatalf("lock error %v", e)
	}
	if network {
		t.Fatal("claimed before obtaining process lock")
	}
}

func TestClientRecoversBegunApplicationAfterCrashAndExpiredLease(t *testing.T) {
	for _, scenario := range []string{"crash-after-begin", "lost-begin-response"} {
		t.Run(scenario, func(t *testing.T) {
			j := applyJob()
			raw, _ := json.Marshal(j)
			dir := filepath.Join(t.TempDir(), "state")
			state, e := NewApplyState(dir)
			if e != nil {
				t.Fatal(e)
			}
			firstBegin := ""
			firstApplies := 0
			c := Client{BaseURL: "https://control.test", NodeID: "hk-01", State: state, Applier: applyFunc(func(context.Context, platform.NodeConfig) error {
				firstApplies++
				panic("simulated process crash after begin")
			})}
			c.HTTP = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/claim") {
					return agentResponse(200, string(raw)), nil
				}
				if !strings.HasSuffix(r.URL.Path, "/begin") {
					t.Fatalf("unexpected first-run request %s", r.URL.Path)
				}
				b, _ := io.ReadAll(r.Body)
				if firstBegin == "" {
					firstBegin = string(b)
				} else if firstBegin != string(b) {
					t.Fatal("begin retry changed proof")
				}
				if scenario == "lost-begin-response" {
					return nil, errors.New("server accepted begin, response lost")
				}
				return agentResponse(200, `{}`), nil
			})}
			func() {
				defer func() {
					if p := recover(); p != nil && scenario != "crash-after-begin" {
						t.Fatalf("unexpected panic %v", p)
					}
				}()
				_, e = c.Once(context.Background())
				if scenario == "lost-begin-response" && e == nil {
					t.Fatal("lost begin not surfaced")
				}
			}()
			if scenario == "lost-begin-response" && firstApplies != 0 {
				t.Fatal("applied without begin 200")
			}
			restarted, e := NewApplyState(dir)
			if e != nil {
				t.Fatal(e)
			}
			c.State = restarted
			c.Now = func() time.Time { return j.LeaseUntil.Add(time.Minute) }
			var steps []string
			c.Applier = applyFunc(func(_ context.Context, cfg platform.NodeConfig) error {
				steps = append(steps, "verified-retry")
				if cfg.Generation != j.Generation {
					t.Fatal("recovered wrong generation")
				}
				return nil
			})
			c.HTTP = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/begin") {
					steps = append(steps, "begin")
					b, _ := io.ReadAll(r.Body)
					if string(b) != firstBegin {
						t.Fatal("restart lost original job/token")
					}
					return agentResponse(200, `{}`), nil
				}
				if strings.HasSuffix(r.URL.Path, "/report") {
					steps = append(steps, "applied")
					var report platform.ConfigReport
					json.NewDecoder(r.Body).Decode(&report)
					if report.LeaseToken != j.LeaseToken || report.Checksum != j.Checksum || report.Outcome != "applied" {
						t.Fatalf("recovered report %+v", report)
					}
					return agentResponse(200, `{}`), nil
				}
				t.Fatalf("restart must not reclaim: %s", r.URL.Path)
				return nil, nil
			})}
			if done, e := c.Once(context.Background()); e != nil || !done || strings.Join(steps, ",") != "begin,verified-retry,applied" {
				t.Fatalf("recovery %v done=%v error=%v", steps, done, e)
			}
		})
	}
}

func TestClientUnsafeApplyErrorsKeepJournalAndFenceAcrossRestart(t *testing.T) {
	for _, cause := range []error{dataplane.ErrRecoveryRequired, dataplane.ErrStateUncertain, shaping.ErrRecoveryRequired, errors.New("unknown apply failure"), fmt.Errorf("rollback failed: %w", dataplane.ErrRecoveryRequired), errors.Join(errors.New("tc rollback failed"), shaping.ErrRecoveryRequired), context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "state")
			state, e := NewApplyState(dir)
			if e != nil {
				t.Fatal(e)
			}
			j := applyJob()
			raw, _ := json.Marshal(j)
			claims, reports := 0, 0
			recovered := false
			c := Client{BaseURL: "https://control.test", NodeID: "hk-01", State: state, Applier: applyFunc(func(context.Context, platform.NodeConfig) error {
				if recovered {
					return nil
				}
				return cause
			})}
			c.HTTP = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/claim") {
					claims++
					return agentResponse(200, string(raw)), nil
				}
				if strings.HasSuffix(r.URL.Path, "/report") {
					reports++
					var report platform.ConfigReport
					json.NewDecoder(r.Body).Decode(&report)
					if report.Outcome != "applied" || !recovered {
						t.Fatalf("unsafe terminal report %+v", report)
					}
					return agentResponse(200, `{}`), nil
				}
				if !strings.HasSuffix(r.URL.Path, "/begin") {
					t.Fatalf("unexpected recovery path %s", r.URL.Path)
				}
				var proof platform.ConfigReport
				json.NewDecoder(r.Body).Decode(&proof)
				if proof.LeaseToken != j.LeaseToken || proof.Checksum != j.Checksum {
					t.Fatal("lost original recovery proof")
				}
				return agentResponse(200, `{}`), nil
			})}
			for attempt := 0; attempt < 2; attempt++ {
				if done, e := c.Once(context.Background()); done || !errors.Is(e, cause) {
					t.Fatalf("unsafe error done=%v error=%v", done, e)
				}
				pending, e := c.State.load(context.Background())
				if e != nil || pending == nil || !pending.BeginConfirmed || pending.Report != nil {
					t.Fatalf("unsafe failure lost journal/fence %+v %v", pending, e)
				}
				c.State, e = NewApplyState(dir)
				if e != nil {
					t.Fatal(e)
				}
				c.Now = func() time.Time { return j.LeaseUntil.Add(time.Hour) }
			}
			if claims != 1 || reports != 0 {
				t.Fatalf("unsafe restart claims=%d reports=%d", claims, reports)
			}
			recovered = true // Explicit caller recovery completed; exact verified retry can now succeed.
			if done, e := c.Once(context.Background()); !done || e != nil {
				t.Fatalf("recovered retry done=%v error=%v", done, e)
			}
			if claims != 1 || reports != 1 {
				t.Fatalf("recovered cycle claims=%d reports=%d", claims, reports)
			}
		})
	}
}

func TestClientStableFailureReceiptRetriesExactlyAfterRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	state, e := NewApplyState(dir)
	if e != nil {
		t.Fatal(e)
	}
	j := applyJob()
	raw, _ := json.Marshal(j)
	c := Client{BaseURL: "https://control.test", NodeID: "hk-01", State: state, Applier: applyFunc(func(context.Context, platform.NodeConfig) error {
		return &SafeApplyFailure{Err: errors.New("rolled back")}
	})}
	var first string
	c.HTTP = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/claim") {
			return agentResponse(200, string(raw)), nil
		}
		if strings.HasSuffix(r.URL.Path, "/report") {
			b, _ := io.ReadAll(r.Body)
			if first == "" {
				first = string(b)
			} else if first != string(b) {
				t.Fatal("failed receipt changed")
			}
			return nil, errors.New("report response lost")
		}
		return agentResponse(200, `{}`), nil
	})}
	if done, e := c.Once(context.Background()); done || e == nil {
		t.Fatalf("lost failure report done=%v error=%v", done, e)
	}
	pending, e := state.load(context.Background())
	if e != nil || pending == nil || !pending.FailureStable || pending.Report == nil || pending.Report.Outcome != "failed" {
		t.Fatalf("missing durable stable proof %+v %v", pending, e)
	}
	c.State, e = NewApplyState(dir)
	if e != nil {
		t.Fatal(e)
	}
	c.Applier = applyFunc(func(context.Context, platform.NodeConfig) error {
		t.Fatal("stable failed receipt reran adapter")
		return nil
	})
	c.HTTP = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/report") {
			t.Fatalf("stable failure receipt reclaimed %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		if string(b) != first {
			t.Fatal("failure receipt lost original proof")
		}
		return agentResponse(200, `{}`), nil
	})}
	if done, e := c.Once(context.Background()); !done || e != nil {
		t.Fatalf("failure retry done=%v error=%v", done, e)
	}
	if pending, e = state.load(context.Background()); e != nil || pending != nil {
		t.Fatalf("acknowledged stable receipt not cleared %+v %v", pending, e)
	}
}
