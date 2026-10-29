package agent

import (
	"context"
	"testing"
	"time"
)

func TestMeterOffloadFeaturesMustAllBeExplicitlyDisabled(t *testing.T) {
	good := "Features for eth0:\ngeneric-receive-offload: off\ngeneric-segmentation-offload: off\ntcp-segmentation-offload: off [fixed]\nlarge-receive-offload: off [fixed]\n"
	if e := validateOffloadFeatures([]byte(good)); e != nil {
		t.Fatal(e)
	}
	for _, bad := range []string{"", "generic-receive-offload: on\n", good + "generic-receive-offload: on\n"} {
		if e := validateOffloadFeatures([]byte(bad)); e == nil {
			t.Fatal("accepted missing, enabled or duplicate feature report")
		}
	}
}

type stopperFunc func(context.Context) error

func (f stopperFunc) StopOwned(ctx context.Context) error { return f(ctx) }
func TestEnvironmentTimeoutStillGetsIndependentBoundedStopBudget(t *testing.T) {
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	if err := stopAfterEnvironmentFailure(expired, stopperFunc(func(ctx context.Context) error {
		called = true
		if ctx.Err() != nil {
			t.Fatal("expired preflight prevented stop from acquiring lock")
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 10*time.Second {
			t.Fatal("unbounded stop")
		}
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("never attempted stop")
	}
}
