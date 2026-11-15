package main

import (
	"context"
	"errors"

	"reladock/internal/cloud/lightsail"
	"reladock/internal/cloud/sshdeploy"
	"reladock/internal/observability"
	"reladock/internal/web"
)

type lightsailRuntimeFactories struct {
	provider func(context.Context, lightsail.Config) (lightsail.Provider, error)
	runner   func() sshRunner
}

type lightsailRuntime struct {
	web      *web.LightsailOptions
	provider lightsail.Provider
	run      func(context.Context)
}

func defaultLightsailRuntimeFactories() lightsailRuntimeFactories {
	return lightsailRuntimeFactories{
		provider: lightsail.NewAWSProvider,
		runner:   func() sshRunner { return sshdeploy.Runner{} },
	}
}

func configureLightsailRuntime(ctx context.Context, checkOnly bool, cfg lightsail.Config, store lightsailJobStore, metrics *observability.Registry, factories lightsailRuntimeFactories) (*lightsailRuntime, error) {
	if checkOnly || !cfg.Enabled {
		return nil, nil
	}
	if store == nil || factories.provider == nil || factories.runner == nil {
		return nil, errors.New("Lightsail connector startup failed")
	}
	provider, err := factories.provider(ctx, cfg)
	if err != nil || provider == nil {
		return nil, errors.New("Lightsail connector startup failed")
	}
	runner := factories.runner()
	if runner == nil {
		return nil, errors.New("Lightsail connector startup failed")
	}
	runtime := &lightsailRuntime{provider: provider}
	runtime.web = &web.LightsailOptions{Config: cfg, Provider: provider}
	runtime.run = func(workerContext context.Context) {
		runLightsailWorker(workerContext, store, provider, runner, cfg, func(runErr error) {
			if metrics != nil {
				_ = metrics.ObserveWorker(observability.WorkerLightsailLifecycle, runErr)
			}
		})
	}
	return runtime, nil
}
