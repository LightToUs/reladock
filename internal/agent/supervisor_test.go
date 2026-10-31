package agent

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunSupervisorKeepsActivitiesIndependent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var failedRuns atomic.Int32
	var healthyRuns atomic.Int32
	healthyTwice := make(chan struct{})
	var once sync.Once
	activities := []Activity{
		{Name: "failing", Interval: time.Millisecond, Run: func(context.Context) error {
			failedRuns.Add(1)
			return errors.New("offline")
		}, backoff: NewBackoff(50*time.Millisecond, 50*time.Millisecond, nil)},
		{Name: "healthy", Interval: 5 * time.Millisecond, Run: func(context.Context) error {
			if healthyRuns.Add(1) >= 2 {
				once.Do(func() { close(healthyTwice) })
			}
			return nil
		}},
	}
	done := make(chan error, 1)
	go func() { done <- RunSupervisor(ctx, activities, nil) }()
	select {
	case <-healthyTwice:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("healthy activity was delayed by unrelated failure")
	}
	if got := failedRuns.Load(); got != 1 {
		t.Fatalf("failing activity ignored its own backoff: runs=%d", got)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("supervisor cancellation: %v", err)
	}
}

func TestRunSupervisorResetsOnlySuccessfulActivity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	delays := make(chan time.Duration, 4)
	b := NewBackoff(5*time.Millisecond, 20*time.Millisecond, func(d time.Duration) time.Duration {
		delays <- d
		return d
	})
	var runs atomic.Int32
	activity := Activity{Name: "recovering", Interval: time.Millisecond, backoff: b, Run: func(context.Context) error {
		switch runs.Add(1) {
		case 1, 2, 4:
			return errors.New("temporary")
		default:
			return nil
		}
	}}
	done := make(chan error, 1)
	go func() { done <- RunSupervisor(ctx, []Activity{activity}, nil) }()
	want := []time.Duration{5 * time.Millisecond, 10 * time.Millisecond, 5 * time.Millisecond}
	for i, expected := range want {
		select {
		case got := <-delays:
			if got != expected {
				t.Fatalf("failure %d delay=%s want=%s", i+1, got, expected)
			}
		case <-time.After(250 * time.Millisecond):
			t.Fatalf("missing failure delay %d", i+1)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("supervisor cancellation: %v", err)
	}
}

func TestRunSupervisorCancellationInterruptsWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- RunSupervisor(ctx, []Activity{{Name: "idle", Interval: time.Hour, Run: func(context.Context) error {
			close(started)
			return nil
		}}}, nil)
	}()
	<-started
	begin := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cancel result: %v", err)
		}
		if elapsed := time.Since(begin); elapsed > 100*time.Millisecond {
			t.Fatalf("cancellation took %s", elapsed)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("cancellation did not interrupt wait")
	}
}

func TestRunSupervisorRejectsInvalidActivity(t *testing.T) {
	for _, activity := range []Activity{
		{},
		{Name: "missing-run", Interval: time.Second},
		{Name: "bad-interval", Run: func(context.Context) error { return nil }},
	} {
		if err := RunSupervisor(context.Background(), []Activity{activity}, nil); err == nil {
			t.Fatalf("accepted invalid activity: %+v", activity)
		}
	}
}
