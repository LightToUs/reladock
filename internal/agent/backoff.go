package agent

import (
	"sync"
	"time"
)

// Backoff tracks one control activity. Instances are intentionally not shared
// between configuration, metering, heartbeat, probes, preflight, or renewal.
type Backoff struct {
	mu       sync.Mutex
	base     time.Duration
	max      time.Duration
	jitter   func(time.Duration) time.Duration
	failures uint
}

func NewBackoff(base, max time.Duration, jitter func(time.Duration) time.Duration) *Backoff {
	if base <= 0 {
		base = time.Second
	}
	if max < base {
		max = base
	}
	return &Backoff{base: base, max: max, jitter: jitter}
}

func (b *Backoff) Failure() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	delay := b.base
	for i := uint(0); i < b.failures && delay < b.max; i++ {
		if delay > b.max/2 {
			delay = b.max
			break
		}
		delay *= 2
	}
	if delay > b.max {
		delay = b.max
	}
	if b.failures < ^uint(0) {
		b.failures++
	}
	if b.jitter != nil {
		if candidate := b.jitter(delay); candidate >= 0 && candidate <= delay {
			return candidate
		}
	}
	return delay
}

func (b *Backoff) Success() {
	b.mu.Lock()
	b.failures = 0
	b.mu.Unlock()
}
