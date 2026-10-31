package agent

import (
	"context"
	"crypto/rand"
	"errors"
	"math/big"
	"sync"
	"time"
)

type Activity struct {
	Name     string
	Interval time.Duration
	Run      func(context.Context) error
	backoff  *Backoff
}

func fullJitter(limit time.Duration) time.Duration {
	if limit <= 0 {
		return 0
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(limit)+1))
	if err != nil {
		return limit
	}
	return time.Duration(n.Int64())
}

func RunSupervisor(ctx context.Context, activities []Activity, report func(string, error)) error {
	if len(activities) == 0 {
		return errors.New("at least one Agent activity is required")
	}
	names := make(map[string]struct{}, len(activities))
	for i := range activities {
		a := &activities[i]
		if a.Name == "" || a.Interval <= 0 || a.Run == nil {
			return errors.New("invalid Agent activity")
		}
		if _, exists := names[a.Name]; exists {
			return errors.New("duplicate Agent activity")
		}
		names[a.Name] = struct{}{}
		if a.backoff == nil {
			a.backoff = NewBackoff(time.Second, time.Minute, fullJitter)
		}
	}
	var workers sync.WaitGroup
	workers.Add(len(activities))
	for i := range activities {
		a := activities[i]
		go func() {
			defer workers.Done()
			for {
				err := a.Run(ctx)
				if ctx.Err() != nil {
					return
				}
				delay := a.Interval
				if err != nil {
					delay = a.backoff.Failure()
					if report != nil {
						report(a.Name, err)
					}
				} else {
					a.backoff.Success()
				}
				timer := time.NewTimer(delay)
				select {
				case <-ctx.Done():
					if !timer.Stop() {
						<-timer.C
					}
					return
				case <-timer.C:
				}
			}
		}()
	}
	workers.Wait()
	return nil
}
