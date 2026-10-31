package agent

import (
	"testing"
	"time"
)

func TestBackoffGrowsCapsJittersAndResets(t *testing.T) {
	seen := []time.Duration{}
	b := NewBackoff(time.Second, time.Minute, func(d time.Duration) time.Duration {
		seen = append(seen, d)
		return d - 100*time.Millisecond
	})
	want := []time.Duration{900 * time.Millisecond, 1900 * time.Millisecond, 3900 * time.Millisecond}
	for i, expected := range want {
		if got := b.Failure(); got != expected {
			t.Fatalf("failure %d delay=%s want=%s", i+1, got, expected)
		}
	}
	for i := 0; i < 10; i++ {
		_ = b.Failure()
	}
	if got := b.Failure(); got != 59900*time.Millisecond {
		t.Fatalf("capped delay=%s want=59.9s", got)
	}
	if got := seen[len(seen)-1]; got != time.Minute {
		t.Fatalf("jitter input=%s want=1m", got)
	}
	b.Success()
	if got := b.Failure(); got != 900*time.Millisecond {
		t.Fatalf("reset delay=%s want=900ms", got)
	}
}

func TestBackoffRejectsOutOfRangeJitter(t *testing.T) {
	for _, jitter := range []func(time.Duration) time.Duration{
		func(time.Duration) time.Duration { return -time.Second },
		func(d time.Duration) time.Duration { return d + time.Second },
	} {
		b := NewBackoff(time.Second, time.Minute, jitter)
		if got := b.Failure(); got != time.Second {
			t.Fatalf("unsafe jitter escaped bounds: %s", got)
		}
	}
}
