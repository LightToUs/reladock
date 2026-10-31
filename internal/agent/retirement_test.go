package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"reladock/internal/dataplane"
	"reladock/internal/platform"
)

type fakeRetirementAdapter struct {
	purges, confirms, recovers int
	outcome                    dataplane.PurgeOutcome
	purgeErr                   error
}

func (a *fakeRetirementAdapter) Recover(context.Context) error { a.recovers++; return nil }
func (a *fakeRetirementAdapter) PurgeAllocation(context.Context, platform.RetirementClaim) (dataplane.PurgeOutcome, error) {
	a.purges++
	return a.outcome, a.purgeErr
}

func TestRetirementBeginConflictClearsOnlyUnconfirmedJournal(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	state, _ := NewRetirementState(root)
	claim := platform.RetirementClaim{ID: "11111111-1111-4111-8111-111111111111", AllocationID: "22222222-2222-4222-8222-222222222222", NodeID: "hk-01", RuleID: "rule-a", Address: "192.0.2.1", Protocol: "tcp", Port: 20001, QuarantineGeneration: 1, LeaseToken: strings.Repeat("a", 64), LeaseUntil: time.Now().Add(time.Minute)}
	if err := state.Save(context.Background(), retirementRecord{Origin: "https://control.test", NodeID: "hk-01", Claim: claim, Sequence: 1, BeginAttempted: true}); err != nil {
		t.Fatal(err)
	}
	adapter := &fakeRetirementAdapter{}
	worker := &RetirementWorker{State: state, Adapter: adapter, BootID: "33333333-3333-4333-8333-333333333333"}
	client := &Client{BaseURL: "https://control.test", NodeID: "hk-01", HTTP: &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) { return retirementHTTP(http.StatusConflict, `{}`), nil })}}
	if done, err := worker.Once(context.Background(), client); err == nil || done || adapter.purges != 0 {
		t.Fatalf("conflict result done=%v err=%v", done, err)
	}
	if record, _ := state.Load(context.Background()); record != nil {
		t.Fatal("explicit begin conflict retained unconfirmed journal")
	}
}

func TestRetirementPurgeFailureReportsSafeCodeAndRetainsJournal(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	state, _ := NewRetirementState(root)
	claim := platform.RetirementClaim{ID: "11111111-1111-4111-8111-111111111111", AllocationID: "22222222-2222-4222-8222-222222222222", NodeID: "hk-01", RuleID: "rule-a", Address: "192.0.2.1", Protocol: "tcp", Port: 20001, QuarantineGeneration: 1, LeaseToken: strings.Repeat("a", 64), LeaseUntil: time.Now().Add(time.Minute)}
	if err := state.Save(context.Background(), retirementRecord{Origin: "https://control.test", NodeID: "hk-01", Claim: claim, Sequence: 1, BeginAttempted: true, BeginConfirmed: true}); err != nil {
		t.Fatal(err)
	}
	adapter := &fakeRetirementAdapter{purgeErr: errors.New("private kernel diagnostic")}
	reported := ""
	worker := &RetirementWorker{State: state, Adapter: adapter, BootID: "33333333-3333-4333-8333-333333333333"}
	client := &Client{BaseURL: "https://control.test", NodeID: "hk-01", HTTP: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		reported = string(raw)
		return retirementHTTP(200, `{}`), nil
	})}}
	if done, err := worker.Once(context.Background(), client); err == nil || done {
		t.Fatalf("failure reported success: %v %v", done, err)
	}
	if !strings.Contains(reported, `"errorCode":"internal-error"`) || strings.Contains(reported, "private kernel") {
		t.Fatalf("unsafe failure report: %s", reported)
	}
	if record, _ := state.Load(context.Background()); record == nil || record.Proof != nil {
		t.Fatal("failed purge journal cleared")
	}
}
func (a *fakeRetirementAdapter) ConfirmRetirementACK(context.Context, string, string) error {
	a.confirms++
	return nil
}

func retirementHTTP(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func TestRetirementWorkerPersistsAmbiguousBeginAndExactProofRetry(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	state, err := NewRetirementState(root)
	if err != nil {
		t.Fatal(err)
	}
	claim := `{"id":"11111111-1111-4111-8111-111111111111","allocationId":"22222222-2222-4222-8222-222222222222","nodeId":"hk-01","ruleId":"rule-a","address":"192.0.2.1","protocol":"tcp","port":20001,"quarantineGeneration":1,"leaseToken":"` + strings.Repeat("a", 64) + `","leaseUntil":"2030-01-01T00:00:00Z"}`
	adapter := &fakeRetirementAdapter{outcome: dataplane.PurgeOutcome{ConntrackVersion: "conntrack v1.4.7 (conntrack-tools)", GuardBeforeDigest: strings.Repeat("b", 64), GuardAfterDigest: strings.Repeat("c", 64), GoodDigest: strings.Repeat("d", 64)}}
	worker := &RetirementWorker{State: state, Adapter: adapter, BootID: "33333333-3333-4333-8333-333333333333", Now: func() time.Time { return time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC) }}
	beginAttempts, reports := 0, 0
	client := &Client{BaseURL: "https://control.test", NodeID: "hk-01", HTTP: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/agent/retirements/claim":
			return retirementHTTP(200, claim), nil
		case "/agent/retirements/begin":
			beginAttempts++
			if beginAttempts == 1 {
				return nil, errors.New("lost response")
			}
			return retirementHTTP(200, `{"ok":true}`), nil
		case "/agent/retirements/report":
			reports++
			if reports == 1 {
				return retirementHTTP(503, `{}`), nil
			}
			return retirementHTTP(200, `{"ok":true}`), nil
		default:
			t.Fatalf("unexpected request %s", r.URL.Path)
			return nil, nil
		}
	})}}
	if done, err := worker.Once(context.Background(), client); err == nil || done || adapter.purges != 0 {
		t.Fatalf("ambiguous begin executed purge: done=%v err=%v", done, err)
	}
	if done, err := worker.Once(context.Background(), client); err == nil || done || adapter.purges != 1 || adapter.confirms != 0 {
		t.Fatalf("lost report state: done=%v err=%v adapter=%+v", done, err, adapter)
	}
	journal, err := state.Load(context.Background())
	if err != nil || journal == nil || journal.Proof == nil {
		t.Fatalf("proof not durable: %+v %v", journal, err)
	}
	proof := *journal.Proof
	if done, err := worker.Once(context.Background(), client); err != nil || !done || adapter.purges != 1 || adapter.confirms != 1 {
		t.Fatalf("retry failed: done=%v err=%v adapter=%+v", done, err, adapter)
	}
	if proof.TaskID != "11111111-1111-4111-8111-111111111111" || reports != 2 {
		t.Fatal("proof/report changed")
	}
	if journal, _ = state.Load(context.Background()); journal != nil {
		t.Fatal("acknowledged journal retained")
	}
}

func TestValidationOnlyClientNeverClaimsRetirement(t *testing.T) {
	paths := []string{}
	client := Client{BaseURL: "https://control.test", NodeID: "hk-01", Retirement: &RetirementWorker{}, HTTP: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		return retirementHTTP(204, ""), nil
	})}}
	if done, err := client.Once(context.Background()); err != nil || done {
		t.Fatalf("validation cycle: %v %v", done, err)
	}
	if len(paths) != 1 || paths[0] != "/agent/config/claim" {
		t.Fatalf("validation claimed retirement: %v", paths)
	}
}

func TestRetirementSequenceIsDurableAndMonotonic(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	state, err := NewRetirementState(root)
	if err != nil {
		t.Fatal(err)
	}
	first, err := state.NextSequence(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := NewRetirementState(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := restarted.NextSequence(context.Background(), 1)
	if err != nil || first != 10 || second != 11 {
		t.Fatalf("sequence %d %d err=%v", first, second, err)
	}
}
