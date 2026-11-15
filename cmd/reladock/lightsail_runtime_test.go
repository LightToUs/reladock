package main

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"reladock/internal/cloud/lightsail"
	"reladock/internal/cloud/sshdeploy"
	"reladock/internal/observability"
	"reladock/internal/platform"
)

type idleLightsailStore struct {
	claims  atomic.Int64
	claimed chan struct{}
}

func (s *idleLightsailStore) ClaimLightsailJob(context.Context, time.Duration) (*platform.LightsailJob, error) {
	if s.claims.Add(1) == 1 && s.claimed != nil {
		close(s.claimed)
	}
	return nil, nil
}
func (*idleLightsailStore) AdvanceLightsailJob(context.Context, string, string, string, string, string) error {
	return nil
}
func (*idleLightsailStore) RetryLightsailJob(context.Context, string, string, string, string, time.Duration) error {
	return nil
}
func (*idleLightsailStore) UpdateLightsailInstance(context.Context, string, string, string, platform.LightsailInstanceUpdate) error {
	return nil
}
func (*idleLightsailStore) BeginLightsailExecution(context.Context, string, string) (platform.LightsailExecution, error) {
	return platform.LightsailExecution{}, nil
}
func (*idleLightsailStore) CompleteLightsailExecution(context.Context, string, string, platform.LightsailExecutionResult) error {
	return nil
}
func (*idleLightsailStore) CompleteLightsailJob(context.Context, string, string, string, string) error {
	return nil
}

type idleLightsailProvider struct{ lightsail.Provider }

func runtimeLightsailConfig(enabled bool) lightsail.Config {
	return lightsail.Config{Enabled: enabled, AllowedRegions: []string{"us-east-1"}, SSHSourceCIDR: "192.0.2.10/32", WorkerInterval: 5 * time.Millisecond, AWSOperationTimeout: time.Minute, SSHConnectTimeout: time.Second, ScriptTimeout: time.Minute}
}

func TestLightsailRuntimeCheckOnlyAndDisabledConstructNothing(t *testing.T) {
	for _, tc := range []struct {
		name      string
		checkOnly bool
		enabled   bool
	}{{"check config", true, true}, {"disabled", false, false}} {
		t.Run(tc.name, func(t *testing.T) {
			var providers, runners atomic.Int64
			runtime, err := configureLightsailRuntime(context.Background(), tc.checkOnly, runtimeLightsailConfig(tc.enabled), &idleLightsailStore{}, nil, lightsailRuntimeFactories{
				provider: func(context.Context, lightsail.Config) (lightsail.Provider, error) {
					providers.Add(1)
					return &idleLightsailProvider{}, nil
				},
				runner: func() sshRunner { runners.Add(1); return sshdeploy.Runner{} },
			})
			if err != nil || runtime != nil || providers.Load() != 0 || runners.Load() != 0 {
				t.Fatalf("runtime=%v err=%v providers=%d runners=%d", runtime, err, providers.Load(), runners.Load())
			}
		})
	}
}

func TestLightsailRuntimeSharesProviderAndStopsOnCancellation(t *testing.T) {
	provider := &idleLightsailProvider{}
	store := &idleLightsailStore{claimed: make(chan struct{})}
	registry := observability.New("test", func(context.Context) (platform.OperationalMetrics, error) { return platform.OperationalMetrics{}, nil })
	var providers, runners atomic.Int64
	runtime, err := configureLightsailRuntime(context.Background(), false, runtimeLightsailConfig(true), store, registry, lightsailRuntimeFactories{
		provider: func(context.Context, lightsail.Config) (lightsail.Provider, error) {
			providers.Add(1)
			return provider, nil
		},
		runner: func() sshRunner { runners.Add(1); return sshdeploy.Runner{} },
	})
	if err != nil {
		t.Fatal(err)
	}
	if runtime == nil || runtime.web == nil || runtime.web.Provider != provider || runtime.provider != provider || providers.Load() != 1 || runners.Load() != 1 {
		t.Fatalf("runtime=%+v providers=%d runners=%d", runtime, providers.Load(), runners.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); runtime.run(ctx) }()
	select {
	case <-store.claimed:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop after cancellation")
	}
}

func TestLightsailRuntimeSanitizesProviderConstructionFailure(t *testing.T) {
	runtime, err := configureLightsailRuntime(context.Background(), false, runtimeLightsailConfig(true), &idleLightsailStore{}, nil, lightsailRuntimeFactories{
		provider: func(context.Context, lightsail.Config) (lightsail.Provider, error) {
			return nil, errors.New("AWS secret details")
		},
		runner: func() sshRunner { return sshdeploy.Runner{} },
	})
	if runtime != nil || err == nil || err.Error() != "Lightsail connector startup failed" || strings.Contains(err.Error(), "secret") {
		t.Fatalf("runtime=%v err=%v", runtime, err)
	}
}
