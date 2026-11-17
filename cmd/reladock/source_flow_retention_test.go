package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestRunSourceFlowRetentionRunsImmediatelyRecursAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	calls := 0
	observed := make(chan error, 4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runSourceFlowRetention(ctx, 10*time.Millisecond, 100*time.Millisecond, func(context.Context, int) (int, error) {
			mu.Lock()
			defer mu.Unlock()
			calls++
			if calls == 1 {
				return 0, errors.New("temporary failure")
			}
			return 1, nil
		}, func(err error) { observed <- err }, func(string) {})
	}()
	if first := <-observed; first == nil {
		t.Fatal("immediate run did not report its failure")
	}
	if second := <-observed; second != nil {
		t.Fatalf("recurring run error: %v", second)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker ignored cancellation")
	}
	mu.Lock()
	before := calls
	mu.Unlock()
	time.Sleep(30 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if calls != before {
		t.Fatalf("worker ran after cancellation: before=%d after=%d", before, calls)
	}
}
