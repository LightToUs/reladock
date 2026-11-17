package main

import (
	"context"
	"time"
)

func runSourceFlowRetention(ctx context.Context, interval, timeout time.Duration, prune func(context.Context, int) (int, error), observe func(error), logFailure func(string)) {
	if interval <= 0 || timeout <= 0 || prune == nil || observe == nil || logFailure == nil {
		panic("invalid source-flow retention worker configuration")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		runCtx, cancel := context.WithTimeout(ctx, timeout)
		_, err := prune(runCtx, 1000)
		cancel()
		if ctx.Err() != nil {
			return
		}
		observe(err)
		if err != nil {
			logFailure("source-flow retention cleanup failed")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
