package platform

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSharedRateLimitConcurrentAndExpiry(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := s.AllowRequest(ctx, "same-ip", "auth", 10)
			if err != nil {
				t.Error(err)
			}
			if ok {
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 10 {
		t.Fatalf("accepted %d instead of 10", accepted.Load())
	}
	if _, err := s.pool.Exec(ctx, "UPDATE request_limits SET expires_at=$1", time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.AllowRequest(ctx, "same-ip", "auth", 10); err != nil || !ok {
		t.Fatalf("expired limit: %v %v", ok, err)
	}
	if ok, err := s.AllowRequest(ctx, "other-ip", "auth", 10); err != nil || !ok {
		t.Fatalf("other client: %v %v", ok, err)
	}
}
