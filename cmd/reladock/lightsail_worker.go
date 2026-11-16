package main

import (
	"context"
	"log"
	"net/netip"
	"time"

	"reladock/internal/cloud/lightsail"
	"reladock/internal/cloud/sshdeploy"
	"reladock/internal/platform"
)

type lightsailJobStore interface {
	ClaimLightsailJob(context.Context, time.Duration) (*platform.LightsailJob, error)
	AdvanceLightsailJob(context.Context, string, string, string, string, string) error
	RetryLightsailJob(context.Context, string, string, string, string, time.Duration) error
	UpdateLightsailInstance(context.Context, string, string, string, platform.LightsailInstanceUpdate) error
	BeginLightsailExecution(context.Context, string, string) (platform.LightsailExecution, error)
	CompleteLightsailExecution(context.Context, string, string, platform.LightsailExecutionResult) error
	CompleteLightsailJob(context.Context, string, string, string, string) error
}

type sshRunner interface {
	Run(context.Context, sshdeploy.Request) (sshdeploy.Result, error)
}

func processLightsailJob(ctx context.Context, store lightsailJobStore, provider lightsail.Provider, runner sshRunner, cfg lightsail.Config) error {
	job, err := store.ClaimLightsailJob(ctx, 2*time.Minute)
	if err != nil || job == nil {
		return err
	}
	switch job.Kind {
	case "create":
		return processLightsailCreate(ctx, store, provider, runner, cfg, job)
	case "stop":
		return processLightsailStop(ctx, store, provider, job)
	case "destroy":
		return processLightsailDestroy(ctx, store, provider, job)
	default:
		return store.CompleteLightsailJob(ctx, job.ID, job.LeaseToken, "failed", "unknown-job-kind")
	}
}

func processLightsailCreate(ctx context.Context, store lightsailJobStore, provider lightsail.Provider, runner sshRunner, cfg lightsail.Config, job *platform.LightsailJob) error {
	instance := job.Instance
	switch job.Stage {
	case "reconcile-instance":
		remote, found, err := provider.GetInstance(ctx, instance.Selection.Region, instance.AWSInstanceName)
		if err != nil {
			return retryLightsailProvider(ctx, store, job, "provider-unavailable")
		}
		if found {
			if !ownedLightsailInstance(remote, instance) {
				return store.CompleteLightsailJob(ctx, job.ID, job.LeaseToken, "manual-review", "instance-ownership-mismatch")
			}
			if err = recordRemoteInstance(ctx, store, job, remote); err != nil {
				return err
			}
			return store.AdvanceLightsailJob(ctx, job.ID, job.LeaseToken, job.Stage, "wait-instance", "")
		}
		op, err := provider.CreateInstance(ctx, lightsail.CreateInstanceInput{
			Region: instance.Selection.Region, AvailabilityZone: instance.Selection.AvailabilityZone, BlueprintID: instance.Selection.BlueprintID, BundleID: instance.Selection.BundleID, Name: instance.AWSInstanceName,
			Tags: map[string]string{"reladock-managed": "true", "reladock-id": instance.ID},
		})
		if err != nil {
			return retryLightsailProvider(ctx, store, job, "provider-unavailable")
		}
		if err = store.UpdateLightsailInstance(ctx, job.ID, job.LeaseToken, job.Stage, platform.LightsailInstanceUpdate{CreateOperationID: stringPointer(op.ID), LifecycleState: stringPointer("provisioning")}); err != nil {
			return err
		}
		return store.AdvanceLightsailJob(ctx, job.ID, job.LeaseToken, job.Stage, "wait-instance", op.ID)

	case "wait-instance":
		if ready, err := operationReady(ctx, store, provider, job); err != nil || !ready {
			return err
		}
		remote, found, err := provider.GetInstance(ctx, instance.Selection.Region, instance.AWSInstanceName)
		if err != nil || !found {
			return retryLightsailProvider(ctx, store, job, "instance-not-ready")
		}
		if !ownedLightsailInstance(remote, instance) {
			return store.CompleteLightsailJob(ctx, job.ID, job.LeaseToken, "manual-review", "instance-ownership-mismatch")
		}
		if remote.State != "running" {
			return retryLightsailProvider(ctx, store, job, "instance-not-running")
		}
		if err = recordRemoteInstance(ctx, store, job, remote); err != nil {
			return err
		}
		if instance.ScriptSize == 0 {
			return store.AdvanceLightsailJob(ctx, job.ID, job.LeaseToken, job.Stage, "reconcile-static-ip", "")
		}
		return store.AdvanceLightsailJob(ctx, job.ID, job.LeaseToken, job.Stage, "configure-ssh-firewall", "")

	case "configure-ssh-firewall":
		op, err := provider.ConfigureSSHFirewall(ctx, instance.Selection.Region, instance.AWSInstanceName, cfg.SSHSourceCIDR)
		if err != nil {
			return retryLightsailProvider(ctx, store, job, "provider-unavailable")
		}
		if err = store.UpdateLightsailInstance(ctx, job.ID, job.LeaseToken, job.Stage, platform.LightsailInstanceUpdate{FirewallOperationID: stringPointer(op.ID)}); err != nil {
			return err
		}
		return store.AdvanceLightsailJob(ctx, job.ID, job.LeaseToken, job.Stage, "wait-firewall", op.ID)

	case "wait-firewall":
		if ready, err := operationReady(ctx, store, provider, job); err != nil || !ready {
			return err
		}
		return store.AdvanceLightsailJob(ctx, job.ID, job.LeaseToken, job.Stage, "reconcile-static-ip", "")

	case "reconcile-static-ip":
		staticIP, found, err := provider.GetStaticIP(ctx, instance.Selection.Region, instance.StaticIPName)
		if err != nil {
			return retryLightsailProvider(ctx, store, job, "provider-unavailable")
		}
		if !found {
			op, allocateErr := provider.AllocateStaticIP(ctx, instance.Selection.Region, instance.StaticIPName)
			if allocateErr != nil {
				return retryLightsailProvider(ctx, store, job, "provider-unavailable")
			}
			if err = store.UpdateLightsailInstance(ctx, job.ID, job.LeaseToken, job.Stage, platform.LightsailInstanceUpdate{StaticIPOperationID: stringPointer(op.ID)}); err != nil {
				return err
			}
			return store.AdvanceLightsailJob(ctx, job.ID, job.LeaseToken, job.Stage, "wait-static-ip", op.ID)
		}
		return attachOrAdvanceStaticIP(ctx, store, provider, job, staticIP, "reconcile-static-ip")

	case "wait-static-ip":
		if ready, err := operationReady(ctx, store, provider, job); err != nil || !ready {
			return err
		}
		staticIP, found, err := provider.GetStaticIP(ctx, instance.Selection.Region, instance.StaticIPName)
		if err != nil || !found {
			return retryLightsailProvider(ctx, store, job, "static-ip-not-ready")
		}
		return attachOrAdvanceStaticIP(ctx, store, provider, job, staticIP, "wait-static-ip")

	case "wait-attach":
		if ready, err := operationReady(ctx, store, provider, job); err != nil || !ready {
			return err
		}
		staticIP, found, err := provider.GetStaticIP(ctx, instance.Selection.Region, instance.StaticIPName)
		if err != nil || !found || !staticIP.IsAttached {
			return retryLightsailProvider(ctx, store, job, "static-ip-not-attached")
		}
		if staticIP.AttachedTo != instance.AWSInstanceName {
			return store.CompleteLightsailJob(ctx, job.ID, job.LeaseToken, "manual-review", "static-ip-ownership-mismatch")
		}
		if err = store.UpdateLightsailInstance(ctx, job.ID, job.LeaseToken, job.Stage, platform.LightsailInstanceUpdate{StaticIPAddress: stringPointer(staticIP.IPAddress)}); err != nil {
			return err
		}
		if instance.ScriptSize == 0 {
			return store.CompleteLightsailJob(ctx, job.ID, job.LeaseToken, "succeeded", "")
		}
		return store.AdvanceLightsailJob(ctx, job.ID, job.LeaseToken, job.Stage, "wait-ssh-details", "")

	case "wait-ssh-details":
		access, err := provider.AccessDetails(ctx, instance.Selection.Region, instance.AWSInstanceName)
		if err != nil || !validAccessDetails(access, instance.StaticIPAddress) {
			return retryLightsailProvider(ctx, store, job, "ssh-details-not-ready")
		}
		return store.AdvanceLightsailJob(ctx, job.ID, job.LeaseToken, job.Stage, "execute-script", "")

	case "execute-script":
		access, err := provider.AccessDetails(ctx, instance.Selection.Region, instance.AWSInstanceName)
		if err != nil || !validAccessDetails(access, instance.StaticIPAddress) {
			return retryLightsailProvider(ctx, store, job, "ssh-details-not-ready")
		}
		execution, err := store.BeginLightsailExecution(ctx, job.ID, job.LeaseToken)
		if err != nil {
			return err
		}
		hostKeys := make([]string, 0, len(access.HostKeys))
		for _, key := range access.HostKeys {
			hostKeys = append(hostKeys, key.PublicKey)
		}
		runResult, runErr := runner.Run(ctx, sshdeploy.Request{Address: execution.Instance.StaticIPAddress, Username: access.Username, PrivateKeyPEM: access.PrivateKey, Certificate: access.Certificate, HostPublicKeys: hostKeys, Script: execution.Script, ConnectTimeout: cfg.SSHConnectTimeout, RunTimeout: cfg.ScriptTimeout})
		result := platform.LightsailExecutionResult{State: "succeeded", Output: runResult.Output}
		if runResult.HostKeySHA256 != "" {
			result.HostKeySHA256 = []string{runResult.HostKeySHA256}
		}
		if runErr != nil {
			result.State = "unknown"
			result.ErrorCode = "execution-outcome-unknown"
		} else {
			exitCode := runResult.ExitCode
			result.ExitCode = &exitCode
			if exitCode != 0 {
				result.State = "failed"
				result.ErrorCode = "script-nonzero-exit"
			}
		}
		return store.CompleteLightsailExecution(ctx, job.ID, job.LeaseToken, result)
	default:
		return store.CompleteLightsailJob(ctx, job.ID, job.LeaseToken, "failed", "unknown-create-stage")
	}
}

func ownedLightsailInstance(remote lightsail.Instance, local platform.LightsailInstance) bool {
	return remote.Name == local.AWSInstanceName && remote.Tags["reladock-managed"] == "true" && remote.Tags["reladock-id"] == local.ID
}

func recordRemoteInstance(ctx context.Context, store lightsailJobStore, job *platform.LightsailJob, remote lightsail.Instance) error {
	return store.UpdateLightsailInstance(ctx, job.ID, job.LeaseToken, job.Stage, platform.LightsailInstanceUpdate{AWSARN: stringPointer(remote.ARN), AWSState: stringPointer(remote.State), LifecycleState: stringPointer("provisioning")})
}

func attachOrAdvanceStaticIP(ctx context.Context, store lightsailJobStore, provider lightsail.Provider, job *platform.LightsailJob, staticIP lightsail.StaticIP, expectedStage string) error {
	instance := job.Instance
	if staticIP.Name != instance.StaticIPName {
		return store.CompleteLightsailJob(ctx, job.ID, job.LeaseToken, "manual-review", "static-ip-ownership-mismatch")
	}
	if staticIP.IsAttached {
		if staticIP.AttachedTo != instance.AWSInstanceName {
			return store.CompleteLightsailJob(ctx, job.ID, job.LeaseToken, "manual-review", "static-ip-ownership-mismatch")
		}
		if err := store.UpdateLightsailInstance(ctx, job.ID, job.LeaseToken, expectedStage, platform.LightsailInstanceUpdate{StaticIPAddress: stringPointer(staticIP.IPAddress)}); err != nil {
			return err
		}
		if instance.ScriptSize == 0 {
			return store.CompleteLightsailJob(ctx, job.ID, job.LeaseToken, "succeeded", "")
		}
		return store.AdvanceLightsailJob(ctx, job.ID, job.LeaseToken, expectedStage, "wait-ssh-details", "")
	}
	op, err := provider.AttachStaticIP(ctx, instance.Selection.Region, instance.StaticIPName, instance.AWSInstanceName)
	if err != nil {
		return retryLightsailProvider(ctx, store, job, "provider-unavailable")
	}
	return store.AdvanceLightsailJob(ctx, job.ID, job.LeaseToken, expectedStage, "wait-attach", op.ID)
}

func validAccessDetails(access lightsail.AccessDetails, expectedIP string) bool {
	if access.Username == "" || access.PrivateKey == "" || len(access.HostKeys) == 0 {
		return false
	}
	actual, actualErr := netip.ParseAddr(access.IPAddress)
	expected, expectedErr := netip.ParseAddr(expectedIP)
	if actualErr != nil || expectedErr != nil || actual != expected {
		return false
	}
	for _, key := range access.HostKeys {
		if key.PublicKey == "" {
			return false
		}
	}
	return true
}

func operationReady(ctx context.Context, store lightsailJobStore, provider lightsail.Provider, job *platform.LightsailJob) (bool, error) {
	if job.ProviderOperationID == "" {
		return true, nil
	}
	op, err := provider.GetOperation(ctx, job.Instance.Selection.Region, job.ProviderOperationID)
	if err != nil {
		return false, retryLightsailProvider(ctx, store, job, "provider-unavailable")
	}
	switch op.Status {
	case "succeeded":
		return true, nil
	case "failed":
		return false, store.CompleteLightsailJob(ctx, job.ID, job.LeaseToken, "failed", "provider-operation-failed")
	default:
		return false, retryLightsailProvider(ctx, store, job, "provider-operation-pending")
	}
}

func retryLightsailProvider(ctx context.Context, store lightsailJobStore, job *platform.LightsailJob, code string) error {
	if job.Attempts >= 100 {
		return store.CompleteLightsailJob(ctx, job.ID, job.LeaseToken, "failed", "retry-exhausted")
	}
	delay := time.Duration(job.Attempts+1) * 5 * time.Second
	if delay > 2*time.Minute {
		delay = 2 * time.Minute
	}
	return store.RetryLightsailJob(ctx, job.ID, job.LeaseToken, job.Stage, code, delay)
}

func stringPointer(value string) *string { return &value }

func processLightsailStop(ctx context.Context, store lightsailJobStore, provider lightsail.Provider, job *platform.LightsailJob) error {
	instance := job.Instance
	switch job.Stage {
	case "reconcile-stop":
		remote, found, err := provider.GetInstance(ctx, instance.Selection.Region, instance.AWSInstanceName)
		if err != nil {
			return retryLightsailProvider(ctx, store, job, "provider-unavailable")
		}
		if !found || !ownedLightsailInstance(remote, instance) {
			return store.CompleteLightsailJob(ctx, job.ID, job.LeaseToken, "manual-review", "stop-ownership-mismatch")
		}
		if remote.State == "stopped" {
			return store.CompleteLightsailJob(ctx, job.ID, job.LeaseToken, "succeeded", "")
		}
		op, err := provider.StopInstance(ctx, instance.Selection.Region, instance.AWSInstanceName)
		if err != nil {
			return retryLightsailProvider(ctx, store, job, "provider-unavailable")
		}
		if err = store.UpdateLightsailInstance(ctx, job.ID, job.LeaseToken, job.Stage, platform.LightsailInstanceUpdate{StopOperationID: stringPointer(op.ID)}); err != nil {
			return err
		}
		return store.AdvanceLightsailJob(ctx, job.ID, job.LeaseToken, job.Stage, "wait-stop", op.ID)
	case "wait-stop":
		if ready, err := operationReady(ctx, store, provider, job); err != nil || !ready {
			return err
		}
		remote, found, err := provider.GetInstance(ctx, instance.Selection.Region, instance.AWSInstanceName)
		if err != nil {
			return retryLightsailProvider(ctx, store, job, "provider-unavailable")
		}
		if !found || !ownedLightsailInstance(remote, instance) {
			return store.CompleteLightsailJob(ctx, job.ID, job.LeaseToken, "manual-review", "stop-ownership-mismatch")
		}
		if remote.State != "stopped" {
			return retryLightsailProvider(ctx, store, job, "instance-not-stopped")
		}
		return store.CompleteLightsailJob(ctx, job.ID, job.LeaseToken, "succeeded", "")
	default:
		return store.CompleteLightsailJob(ctx, job.ID, job.LeaseToken, "failed", "unknown-stop-stage")
	}
}

func processLightsailDestroy(ctx context.Context, store lightsailJobStore, provider lightsail.Provider, job *platform.LightsailJob) error {
	instance := job.Instance
	switch job.Stage {
	case "reconcile-ownership":
		remote, found, err := provider.GetInstance(ctx, instance.Selection.Region, instance.AWSInstanceName)
		if err != nil {
			return retryLightsailProvider(ctx, store, job, "provider-unavailable")
		}
		if !found || !ownedLightsailInstance(remote, instance) || remote.State != "stopped" {
			return destroyManualReview(ctx, store, job)
		}
		staticIP, ipFound, err := provider.GetStaticIP(ctx, instance.Selection.Region, instance.StaticIPName)
		if err != nil {
			return retryLightsailProvider(ctx, store, job, "provider-unavailable")
		}
		if !ipFound || staticIP.Name != instance.StaticIPName || staticIP.IsAttached && staticIP.AttachedTo != instance.AWSInstanceName {
			return destroyManualReview(ctx, store, job)
		}
		next := "delete-instance"
		if staticIP.IsAttached {
			next = "detach-static-ip"
		}
		return store.AdvanceLightsailJob(ctx, job.ID, job.LeaseToken, job.Stage, next, "")

	case "detach-static-ip":
		staticIP, found, err := provider.GetStaticIP(ctx, instance.Selection.Region, instance.StaticIPName)
		if err != nil {
			return retryLightsailProvider(ctx, store, job, "provider-unavailable")
		}
		if !found || staticIP.Name != instance.StaticIPName || staticIP.IsAttached && staticIP.AttachedTo != instance.AWSInstanceName {
			return destroyManualReview(ctx, store, job)
		}
		if !staticIP.IsAttached {
			return store.AdvanceLightsailJob(ctx, job.ID, job.LeaseToken, job.Stage, "delete-instance", "")
		}
		op, err := provider.DetachStaticIP(ctx, instance.Selection.Region, instance.StaticIPName)
		if err != nil {
			return retryLightsailProvider(ctx, store, job, "provider-unavailable")
		}
		return store.AdvanceLightsailJob(ctx, job.ID, job.LeaseToken, job.Stage, "wait-detach", op.ID)

	case "wait-detach":
		if ready, err := operationReady(ctx, store, provider, job); err != nil || !ready {
			return err
		}
		staticIP, found, err := provider.GetStaticIP(ctx, instance.Selection.Region, instance.StaticIPName)
		if err != nil {
			return retryLightsailProvider(ctx, store, job, "provider-unavailable")
		}
		if !found || staticIP.Name != instance.StaticIPName {
			return destroyManualReview(ctx, store, job)
		}
		if staticIP.IsAttached {
			if staticIP.AttachedTo != instance.AWSInstanceName {
				return destroyManualReview(ctx, store, job)
			}
			return retryLightsailProvider(ctx, store, job, "static-ip-still-attached")
		}
		return store.AdvanceLightsailJob(ctx, job.ID, job.LeaseToken, job.Stage, "delete-instance", "")

	case "delete-instance":
		remote, found, err := provider.GetInstance(ctx, instance.Selection.Region, instance.AWSInstanceName)
		if err != nil {
			return retryLightsailProvider(ctx, store, job, "provider-unavailable")
		}
		if !found {
			return store.AdvanceLightsailJob(ctx, job.ID, job.LeaseToken, job.Stage, "release-static-ip", "")
		}
		if !ownedLightsailInstance(remote, instance) || remote.State != "stopped" {
			return destroyManualReview(ctx, store, job)
		}
		op, err := provider.DeleteInstance(ctx, instance.Selection.Region, instance.AWSInstanceName)
		if err != nil {
			return retryLightsailProvider(ctx, store, job, "provider-unavailable")
		}
		if err = store.UpdateLightsailInstance(ctx, job.ID, job.LeaseToken, job.Stage, platform.LightsailInstanceUpdate{DeleteOperationID: stringPointer(op.ID)}); err != nil {
			return err
		}
		return store.AdvanceLightsailJob(ctx, job.ID, job.LeaseToken, job.Stage, "wait-delete", op.ID)

	case "wait-delete":
		if ready, err := operationReady(ctx, store, provider, job); err != nil || !ready {
			return err
		}
		remote, found, err := provider.GetInstance(ctx, instance.Selection.Region, instance.AWSInstanceName)
		if err != nil {
			return retryLightsailProvider(ctx, store, job, "provider-unavailable")
		}
		if found {
			if !ownedLightsailInstance(remote, instance) {
				return destroyManualReview(ctx, store, job)
			}
			return retryLightsailProvider(ctx, store, job, "instance-still-present")
		}
		return store.AdvanceLightsailJob(ctx, job.ID, job.LeaseToken, job.Stage, "release-static-ip", "")

	case "release-static-ip":
		staticIP, found, err := provider.GetStaticIP(ctx, instance.Selection.Region, instance.StaticIPName)
		if err != nil {
			return retryLightsailProvider(ctx, store, job, "provider-unavailable")
		}
		if !found {
			return store.CompleteLightsailJob(ctx, job.ID, job.LeaseToken, "succeeded", "")
		}
		if staticIP.Name != instance.StaticIPName || staticIP.IsAttached {
			return destroyManualReview(ctx, store, job)
		}
		op, err := provider.ReleaseStaticIP(ctx, instance.Selection.Region, instance.StaticIPName)
		if err != nil {
			return retryLightsailProvider(ctx, store, job, "provider-unavailable")
		}
		return store.AdvanceLightsailJob(ctx, job.ID, job.LeaseToken, job.Stage, "wait-release", op.ID)

	case "wait-release":
		if ready, err := operationReady(ctx, store, provider, job); err != nil || !ready {
			return err
		}
		_, found, err := provider.GetStaticIP(ctx, instance.Selection.Region, instance.StaticIPName)
		if err != nil {
			return retryLightsailProvider(ctx, store, job, "provider-unavailable")
		}
		if found {
			return retryLightsailProvider(ctx, store, job, "static-ip-still-present")
		}
		return store.CompleteLightsailJob(ctx, job.ID, job.LeaseToken, "succeeded", "")
	default:
		return store.CompleteLightsailJob(ctx, job.ID, job.LeaseToken, "failed", "unknown-destroy-stage")
	}
}

func destroyManualReview(ctx context.Context, store lightsailJobStore, job *platform.LightsailJob) error {
	return store.CompleteLightsailJob(ctx, job.ID, job.LeaseToken, "manual-review", "destroy-ownership-mismatch")
}

func runLightsailWorker(ctx context.Context, store lightsailJobStore, provider lightsail.Provider, runner sshRunner, cfg lightsail.Config, observe func(error)) {
	ticker := time.NewTicker(cfg.WorkerInterval)
	defer ticker.Stop()
	for {
		workerCtx, cancel := context.WithTimeout(ctx, cfg.AWSOperationTimeout+cfg.ScriptTimeout)
		err := processLightsailJob(workerCtx, store, provider, runner, cfg)
		cancel()
		if observe != nil {
			observe(err)
		}
		if err != nil && ctx.Err() == nil {
			log.Print("Lightsail lifecycle worker failed")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
