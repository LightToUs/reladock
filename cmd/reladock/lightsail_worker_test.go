package main

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"reladock/internal/cloud/lightsail"
	"reladock/internal/cloud/sshdeploy"
	"reladock/internal/platform"
)

type fakeLightsailJobStore struct {
	job             *platform.LightsailJob
	execution       platform.LightsailExecution
	claimErr        error
	advanced        []string
	retried         []string
	updates         []platform.LightsailInstanceUpdate
	completed       []string
	executionResult *platform.LightsailExecutionResult
}

func (f *fakeLightsailJobStore) ClaimLightsailJob(context.Context, time.Duration) (*platform.LightsailJob, error) {
	return f.job, f.claimErr
}
func (f *fakeLightsailJobStore) AdvanceLightsailJob(_ context.Context, _, _, expected, next, operation string) error {
	f.advanced = append(f.advanced, expected+"->"+next+":"+operation)
	return nil
}
func (f *fakeLightsailJobStore) RetryLightsailJob(_ context.Context, _, _, stage, code string, _ time.Duration) error {
	f.retried = append(f.retried, stage+":"+code)
	return nil
}
func (f *fakeLightsailJobStore) UpdateLightsailInstance(_ context.Context, _, _, _ string, update platform.LightsailInstanceUpdate) error {
	f.updates = append(f.updates, update)
	return nil
}
func (f *fakeLightsailJobStore) BeginLightsailExecution(context.Context, string, string) (platform.LightsailExecution, error) {
	return f.execution, nil
}
func (f *fakeLightsailJobStore) CompleteLightsailExecution(_ context.Context, _, _ string, result platform.LightsailExecutionResult) error {
	f.executionResult = &result
	return nil
}
func (f *fakeLightsailJobStore) CompleteLightsailJob(_ context.Context, _, _, state, code string) error {
	f.completed = append(f.completed, state+":"+code)
	return nil
}

type fakeLightsailProvider struct {
	instance      lightsail.Instance
	instanceFound bool
	instanceErr   error
	staticIP      lightsail.StaticIP
	staticIPFound bool
	staticIPErr   error
	operation     lightsail.Operation
	operationErr  error
	access        lightsail.AccessDetails
	accessErr     error
	accessCalls   int
	createCalls   int
	createErr     error
	createdInput  lightsail.CreateInstanceInput
	firewallCalls int
	allocateCalls int
	attachCalls   int
	detachCalls   int
	releaseCalls  int
	stopCalls     int
	deleteCalls   int
}

func (f *fakeLightsailProvider) Catalog(context.Context, string) (lightsail.Catalog, error) {
	return lightsail.Catalog{}, nil
}
func (f *fakeLightsailProvider) GetInstance(context.Context, string, string) (lightsail.Instance, bool, error) {
	return f.instance, f.instanceFound, f.instanceErr
}
func (f *fakeLightsailProvider) CreateInstance(_ context.Context, input lightsail.CreateInstanceInput) (lightsail.Operation, error) {
	f.createCalls++
	f.createdInput = input
	if f.createErr != nil {
		return lightsail.Operation{}, f.createErr
	}
	return lightsail.Operation{ID: "op-create", Status: "started"}, nil
}
func (f *fakeLightsailProvider) GetOperation(context.Context, string, string) (lightsail.Operation, error) {
	return f.operation, f.operationErr
}
func (f *fakeLightsailProvider) ConfigureSSHFirewall(context.Context, string, string, string) (lightsail.Operation, error) {
	f.firewallCalls++
	return lightsail.Operation{ID: "op-firewall", Status: "started"}, nil
}
func (f *fakeLightsailProvider) GetStaticIP(context.Context, string, string) (lightsail.StaticIP, bool, error) {
	return f.staticIP, f.staticIPFound, f.staticIPErr
}
func (f *fakeLightsailProvider) AllocateStaticIP(context.Context, string, string) (lightsail.Operation, error) {
	f.allocateCalls++
	return lightsail.Operation{ID: "op-allocate", Status: "started"}, nil
}
func (f *fakeLightsailProvider) AttachStaticIP(context.Context, string, string, string) (lightsail.Operation, error) {
	f.attachCalls++
	return lightsail.Operation{ID: "op-attach", Status: "started"}, nil
}
func (f *fakeLightsailProvider) DetachStaticIP(context.Context, string, string) (lightsail.Operation, error) {
	f.detachCalls++
	return lightsail.Operation{ID: "op-detach", Status: "started"}, nil
}
func (f *fakeLightsailProvider) ReleaseStaticIP(context.Context, string, string) (lightsail.Operation, error) {
	f.releaseCalls++
	return lightsail.Operation{ID: "op-release", Status: "started"}, nil
}
func (f *fakeLightsailProvider) AccessDetails(context.Context, string, string) (lightsail.AccessDetails, error) {
	f.accessCalls++
	return f.access, f.accessErr
}
func (f *fakeLightsailProvider) StopInstance(context.Context, string, string) (lightsail.Operation, error) {
	f.stopCalls++
	return lightsail.Operation{ID: "op-stop", Status: "started"}, nil
}
func (f *fakeLightsailProvider) DeleteInstance(context.Context, string, string) (lightsail.Operation, error) {
	f.deleteCalls++
	return lightsail.Operation{ID: "op-delete", Status: "started"}, nil
}

type fakeSSHRunner struct {
	requests []sshdeploy.Request
	result   sshdeploy.Result
	err      error
}

func (f *fakeSSHRunner) Run(_ context.Context, request sshdeploy.Request) (sshdeploy.Result, error) {
	f.requests = append(f.requests, request)
	return f.result, f.err
}

func workerConfig() lightsail.Config {
	return lightsail.Config{Enabled: true, AllowedRegions: []string{"us-east-1"}, SSHSourceCIDR: "192.0.2.8/32", WorkerInterval: time.Second, AWSOperationTimeout: time.Minute, SSHConnectTimeout: time.Second, ScriptTimeout: time.Minute}
}

func createWorkerJob(stage string) *platform.LightsailJob {
	return &platform.LightsailJob{
		ID: "00000000-0000-4000-8000-000000000010", InstanceID: "00000000-0000-4000-8000-000000000011", Kind: "create", State: "leased", Stage: stage,
		LeaseToken: "00000000-0000-4000-8000-000000000012", ProviderOperationID: "stored-operation",
		Instance: platform.LightsailInstance{ID: "00000000-0000-4000-8000-000000000011", Name: "Worker", Selection: platform.LightsailCatalogSelection{Region: "us-east-1", AvailabilityZone: "us-east-1a", BlueprintID: "debian", BundleID: "small"}, AWSInstanceName: "reladock-ls-abc123abc123", StaticIPName: "reladock-ip-abc123abc123", StaticIPAddress: "203.0.113.9", ScriptSize: 10},
	}
}

func TestLightsailWorkerCreateReconcilesBeforeCreateAndTagsOwnership(t *testing.T) {
	store := &fakeLightsailJobStore{job: createWorkerJob("reconcile-instance")}
	provider := &fakeLightsailProvider{instanceFound: true, instance: lightsail.Instance{Name: store.job.Instance.AWSInstanceName, ARN: "arn:test", State: "pending", Tags: map[string]string{"reladock-managed": "true", "reladock-id": store.job.InstanceID}}}
	if err := processLightsailJob(context.Background(), store, provider, &fakeSSHRunner{}, workerConfig()); err != nil {
		t.Fatal(err)
	}
	if provider.createCalls != 0 || !reflect.DeepEqual(store.advanced, []string{"reconcile-instance->wait-instance:"}) || len(store.updates) != 1 || store.updates[0].AWSARN == nil || *store.updates[0].AWSARN != "arn:test" {
		t.Fatalf("provider=%+v store=%+v", provider, store)
	}

	store = &fakeLightsailJobStore{job: createWorkerJob("reconcile-instance")}
	provider = &fakeLightsailProvider{}
	if err := processLightsailJob(context.Background(), store, provider, &fakeSSHRunner{}, workerConfig()); err != nil {
		t.Fatal(err)
	}
	if provider.createCalls != 1 || provider.createdInput.Tags["reladock-managed"] != "true" || provider.createdInput.Tags["reladock-id"] != store.job.InstanceID || !reflect.DeepEqual(store.advanced, []string{"reconcile-instance->wait-instance:op-create"}) {
		t.Fatalf("create input=%+v advanced=%v", provider.createdInput, store.advanced)
	}
}

func TestLightsailWorkerCreateUncertainResponseReconcilesWithoutDuplicate(t *testing.T) {
	store := &fakeLightsailJobStore{job: createWorkerJob("reconcile-instance")}
	provider := &fakeLightsailProvider{createErr: errors.New("provider unavailable")}
	if err := processLightsailJob(context.Background(), store, provider, &fakeSSHRunner{}, workerConfig()); err != nil {
		t.Fatal(err)
	}
	if provider.createCalls != 1 || !reflect.DeepEqual(store.retried, []string{"reconcile-instance:provider-unavailable"}) {
		t.Fatalf("first cycle provider=%+v store=%+v", provider, store)
	}
	provider.createErr = nil
	provider.instanceFound = true
	provider.instance = lightsail.Instance{Name: store.job.Instance.AWSInstanceName, ARN: "arn:recovered", Tags: map[string]string{"reladock-managed": "true", "reladock-id": store.job.InstanceID}}
	store.retried = nil
	if err := processLightsailJob(context.Background(), store, provider, &fakeSSHRunner{}, workerConfig()); err != nil {
		t.Fatal(err)
	}
	if provider.createCalls != 1 {
		t.Fatalf("duplicate create after uncertain response: %d", provider.createCalls)
	}
}

func TestLightsailWorkerRetryBudgetTerminatesBeforeDatabaseAttemptLimit(t *testing.T) {
	job := createWorkerJob("reconcile-instance")
	job.Attempts = 100
	store := &fakeLightsailJobStore{job: job}
	provider := &fakeLightsailProvider{instanceErr: errors.New("provider unavailable")}
	if err := processLightsailJob(context.Background(), store, provider, &fakeSSHRunner{}, workerConfig()); err != nil {
		t.Fatal(err)
	}
	if len(store.retried) != 0 || !reflect.DeepEqual(store.completed, []string{"failed:retry-exhausted"}) {
		t.Fatalf("retried=%v completed=%v", store.retried, store.completed)
	}
}

func TestLightsailWorkerDeployStagesFirewallBeforeStaticIPAndSSH(t *testing.T) {
	tests := []struct {
		stage       string
		provider    *fakeLightsailProvider
		wantAdvance string
		check       func(*testing.T, *fakeLightsailProvider, *fakeLightsailJobStore)
	}{
		{
			stage: "wait-instance",
			provider: &fakeLightsailProvider{
				operation:     lightsail.Operation{Status: "succeeded"},
				instanceFound: true,
				instance: lightsail.Instance{
					Name: "reladock-ls-abc123abc123", ARN: "arn:test", State: "running",
					Tags: map[string]string{"reladock-managed": "true", "reladock-id": "00000000-0000-4000-8000-000000000011"},
				},
			},
			wantAdvance: "wait-instance->configure-ssh-firewall:",
		},
		{stage: "configure-ssh-firewall", provider: &fakeLightsailProvider{}, wantAdvance: "configure-ssh-firewall->wait-firewall:op-firewall", check: func(t *testing.T, p *fakeLightsailProvider, _ *fakeLightsailJobStore) {
			if p.firewallCalls != 1 || p.allocateCalls != 0 {
				t.Fatalf("firewall=%d allocate=%d", p.firewallCalls, p.allocateCalls)
			}
		}},
		{stage: "wait-firewall", provider: &fakeLightsailProvider{operation: lightsail.Operation{Status: "succeeded"}}, wantAdvance: "wait-firewall->reconcile-static-ip:"},
		{stage: "reconcile-static-ip", provider: &fakeLightsailProvider{}, wantAdvance: "reconcile-static-ip->wait-static-ip:op-allocate", check: func(t *testing.T, p *fakeLightsailProvider, _ *fakeLightsailJobStore) {
			if p.allocateCalls != 1 {
				t.Fatalf("allocate calls=%d", p.allocateCalls)
			}
		}},
		{stage: "wait-static-ip", provider: &fakeLightsailProvider{operation: lightsail.Operation{Status: "succeeded"}, staticIPFound: true, staticIP: lightsail.StaticIP{Name: "reladock-ip-abc123abc123", IPAddress: "203.0.113.9"}}, wantAdvance: "wait-static-ip->wait-attach:op-attach", check: func(t *testing.T, p *fakeLightsailProvider, _ *fakeLightsailJobStore) {
			if p.attachCalls != 1 {
				t.Fatalf("attach calls=%d", p.attachCalls)
			}
		}},
		{stage: "wait-attach", provider: &fakeLightsailProvider{operation: lightsail.Operation{Status: "succeeded"}, staticIPFound: true, staticIP: lightsail.StaticIP{Name: "reladock-ip-abc123abc123", IPAddress: "203.0.113.9", IsAttached: true, AttachedTo: "reladock-ls-abc123abc123"}}, wantAdvance: "wait-attach->wait-ssh-details:"},
		{stage: "wait-ssh-details", provider: &fakeLightsailProvider{access: lightsail.AccessDetails{IPAddress: "203.0.113.9", Username: "admin", PrivateKey: "key", HostKeys: []lightsail.HostKey{{PublicKey: "ssh-ed25519 AAAA"}}}}, wantAdvance: "wait-ssh-details->execute-script:"},
	}
	for _, tc := range tests {
		t.Run(tc.stage, func(t *testing.T) {
			store := &fakeLightsailJobStore{job: createWorkerJob(tc.stage)}
			if err := processLightsailJob(context.Background(), store, tc.provider, &fakeSSHRunner{}, workerConfig()); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(store.advanced, []string{tc.wantAdvance}) {
				t.Fatalf("advanced=%v", store.advanced)
			}
			if tc.check != nil {
				tc.check(t, tc.provider, store)
			}
		})
	}
}

func TestLightsailWorkerEmptyScriptSkipsFirewallAndSSH(t *testing.T) {
	waitInstance := createWorkerJob("wait-instance")
	waitInstance.Instance.ScriptSize = 0
	store := &fakeLightsailJobStore{job: waitInstance}
	provider := &fakeLightsailProvider{
		operation:     lightsail.Operation{Status: "succeeded"},
		instanceFound: true,
		instance:      ownedRemoteInstance(waitInstance, "running"),
	}
	runner := &fakeSSHRunner{}
	if err := processLightsailJob(context.Background(), store, provider, runner, workerConfig()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(store.advanced, []string{"wait-instance->reconcile-static-ip:"}) || provider.firewallCalls != 0 || provider.accessCalls != 0 || len(runner.requests) != 0 {
		t.Fatalf("empty script wait-instance provider=%+v store=%+v runner=%+v", provider, store, runner)
	}

	waitAttach := createWorkerJob("wait-attach")
	waitAttach.Instance.ScriptSize = 0
	store = &fakeLightsailJobStore{job: waitAttach}
	provider = &fakeLightsailProvider{
		operation:     lightsail.Operation{Status: "succeeded"},
		staticIPFound: true,
		staticIP:      lightsail.StaticIP{Name: waitAttach.Instance.StaticIPName, IPAddress: waitAttach.Instance.StaticIPAddress, IsAttached: true, AttachedTo: waitAttach.Instance.AWSInstanceName},
	}
	runner = &fakeSSHRunner{}
	if err := processLightsailJob(context.Background(), store, provider, runner, workerConfig()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(store.completed, []string{"succeeded:"}) || len(store.advanced) != 0 || provider.firewallCalls != 0 || provider.accessCalls != 0 || len(runner.requests) != 0 {
		t.Fatalf("empty script completion provider=%+v store=%+v runner=%+v", provider, store, runner)
	}
}

func TestLightsailWorkerWaitsForAccessDetailsToMatchStaticIP(t *testing.T) {
	job := createWorkerJob("execute-script")
	store := &fakeLightsailJobStore{job: job, execution: platform.LightsailExecution{Instance: job.Instance, Script: "echo once\n"}}
	provider := &fakeLightsailProvider{access: lightsail.AccessDetails{IPAddress: "203.0.113.99", Username: "admin", PrivateKey: "private", HostKeys: []lightsail.HostKey{{PublicKey: "ssh-ed25519 AAAA"}}}}
	runner := &fakeSSHRunner{}
	if err := processLightsailJob(context.Background(), store, provider, runner, workerConfig()); err != nil {
		t.Fatal(err)
	}
	if len(store.retried) != 1 || len(runner.requests) != 0 || store.executionResult != nil {
		t.Fatalf("retried=%v runner=%v execution=%+v", store.retried, runner.requests, store.executionResult)
	}
}

func TestLightsailWorkerExecutionHandsOffWitnessedKeysAndCompletesOnce(t *testing.T) {
	job := createWorkerJob("execute-script")
	store := &fakeLightsailJobStore{job: job, execution: platform.LightsailExecution{Instance: job.Instance, Script: "echo once\n"}}
	provider := &fakeLightsailProvider{access: lightsail.AccessDetails{IPAddress: job.Instance.StaticIPAddress, Username: "admin", PrivateKey: "private", Certificate: "cert", HostKeys: []lightsail.HostKey{{PublicKey: "ssh-ed25519 AAAA"}}}}
	runner := &fakeSSHRunner{result: sshdeploy.Result{Started: true, ExitCode: 0, Output: "ok\n", HostKeySHA256: "SHA256:host"}}
	if err := processLightsailJob(context.Background(), store, provider, runner, workerConfig()); err != nil {
		t.Fatal(err)
	}
	if len(runner.requests) != 1 || runner.requests[0].Address != job.Instance.StaticIPAddress || !reflect.DeepEqual(runner.requests[0].HostPublicKeys, []string{"ssh-ed25519 AAAA"}) || runner.requests[0].Script != "echo once\n" {
		t.Fatalf("runner requests=%+v", runner.requests)
	}
	if store.executionResult == nil || store.executionResult.State != "succeeded" || store.executionResult.Output != "ok\n" || !reflect.DeepEqual(store.executionResult.HostKeySHA256, []string{"SHA256:host"}) {
		t.Fatalf("execution result=%+v", store.executionResult)
	}
	if len(runner.requests) != 1 {
		t.Fatal("runner called more than once")
	}
}

func TestLightsailWorkerExecutionFailureAfterBoundaryIsUnknown(t *testing.T) {
	for name, started := range map[string]bool{"before-start": false, "after-start": true} {
		t.Run(name, func(t *testing.T) {
			job := createWorkerJob("execute-script")
			store := &fakeLightsailJobStore{job: job, execution: platform.LightsailExecution{Instance: job.Instance, Script: "echo once\n"}}
			provider := &fakeLightsailProvider{access: lightsail.AccessDetails{IPAddress: job.Instance.StaticIPAddress, Username: "admin", PrivateKey: "private", HostKeys: []lightsail.HostKey{{PublicKey: "ssh-ed25519 AAAA"}}}}
			runner := &fakeSSHRunner{result: sshdeploy.Result{Started: started}, err: context.DeadlineExceeded}
			if err := processLightsailJob(context.Background(), store, provider, runner, workerConfig()); err != nil {
				t.Fatal(err)
			}
			if store.executionResult == nil || store.executionResult.State != "unknown" || len(store.retried) != 0 {
				t.Fatalf("store=%+v", store)
			}
		})
	}
}

func lifecycleWorkerJob(kind, stage string) *platform.LightsailJob {
	job := createWorkerJob(stage)
	job.Kind = kind
	job.Instance.AWSARN = "arn:test"
	job.Instance.AWSState = "running"
	return job
}

func ownedRemoteInstance(job *platform.LightsailJob, state string) lightsail.Instance {
	return lightsail.Instance{Name: job.Instance.AWSInstanceName, ARN: job.Instance.AWSARN, State: state, Tags: map[string]string{"reladock-managed": "true", "reladock-id": job.InstanceID}}
}

func TestLightsailWorkerStopReconcilesAndPreservesStaticIP(t *testing.T) {
	job := lifecycleWorkerJob("stop", "reconcile-stop")
	store := &fakeLightsailJobStore{job: job}
	provider := &fakeLightsailProvider{instanceFound: true, instance: ownedRemoteInstance(job, "stopped")}
	if err := processLightsailJob(context.Background(), store, provider, &fakeSSHRunner{}, workerConfig()); err != nil {
		t.Fatal(err)
	}
	if provider.stopCalls != 0 || !reflect.DeepEqual(store.completed, []string{"succeeded:"}) || provider.detachCalls != 0 || provider.releaseCalls != 0 {
		t.Fatalf("already stopped provider=%+v completed=%v", provider, store.completed)
	}

	store = &fakeLightsailJobStore{job: lifecycleWorkerJob("stop", "reconcile-stop")}
	provider = &fakeLightsailProvider{instanceFound: true, instance: ownedRemoteInstance(store.job, "running")}
	if err := processLightsailJob(context.Background(), store, provider, &fakeSSHRunner{}, workerConfig()); err != nil {
		t.Fatal(err)
	}
	if provider.stopCalls != 1 || !reflect.DeepEqual(store.advanced, []string{"reconcile-stop->wait-stop:op-stop"}) || provider.detachCalls != 0 {
		t.Fatalf("stop provider=%+v advanced=%v", provider, store.advanced)
	}

	store = &fakeLightsailJobStore{job: lifecycleWorkerJob("stop", "wait-stop")}
	provider = &fakeLightsailProvider{operation: lightsail.Operation{Status: "succeeded"}, instanceFound: true, instance: ownedRemoteInstance(store.job, "stopped")}
	if err := processLightsailJob(context.Background(), store, provider, &fakeSSHRunner{}, workerConfig()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(store.completed, []string{"succeeded:"}) {
		t.Fatalf("wait stop completed=%v", store.completed)
	}
}

func TestLightsailWorkerDestroyOwnershipMismatchRequiresManualReview(t *testing.T) {
	job := lifecycleWorkerJob("destroy", "reconcile-ownership")
	job.Instance.AWSState = "stopped"
	for name, provider := range map[string]*fakeLightsailProvider{
		"missing instance":    {},
		"wrong tag":           {instanceFound: true, instance: lightsail.Instance{Name: job.Instance.AWSInstanceName, State: "stopped", Tags: map[string]string{"reladock-managed": "true", "reladock-id": "other"}}},
		"wrong ip attachment": {instanceFound: true, instance: ownedRemoteInstance(job, "stopped"), staticIPFound: true, staticIP: lightsail.StaticIP{Name: job.Instance.StaticIPName, IsAttached: true, AttachedTo: "other-instance"}},
	} {
		t.Run(name, func(t *testing.T) {
			store := &fakeLightsailJobStore{job: job}
			if err := processLightsailJob(context.Background(), store, provider, &fakeSSHRunner{}, workerConfig()); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(store.completed, []string{"manual-review:destroy-ownership-mismatch"}) || provider.deleteCalls != 0 || provider.releaseCalls != 0 {
				t.Fatalf("store=%+v provider=%+v", store, provider)
			}
		})
	}
}

func TestLightsailWorkerDestroyStagesAreRestartSafe(t *testing.T) {
	base := lifecycleWorkerJob("destroy", "reconcile-ownership")
	base.Instance.AWSState = "stopped"
	ownedIPAttached := lightsail.StaticIP{Name: base.Instance.StaticIPName, IPAddress: base.Instance.StaticIPAddress, IsAttached: true, AttachedTo: base.Instance.AWSInstanceName}
	ownedIPDetached := ownedIPAttached
	ownedIPDetached.IsAttached = false
	ownedIPDetached.AttachedTo = ""
	tests := []struct {
		stage       string
		provider    *fakeLightsailProvider
		wantAdvance string
		wantDone    string
		check       func(*testing.T, *fakeLightsailProvider)
	}{
		{stage: "reconcile-ownership", provider: &fakeLightsailProvider{instanceFound: true, instance: ownedRemoteInstance(base, "stopped"), staticIPFound: true, staticIP: ownedIPAttached}, wantAdvance: "reconcile-ownership->detach-static-ip:"},
		{stage: "detach-static-ip", provider: &fakeLightsailProvider{staticIPFound: true, staticIP: ownedIPAttached}, wantAdvance: "detach-static-ip->wait-detach:op-detach", check: func(t *testing.T, p *fakeLightsailProvider) {
			if p.detachCalls != 1 {
				t.Fatalf("detach calls=%d", p.detachCalls)
			}
		}},
		{stage: "wait-detach", provider: &fakeLightsailProvider{operation: lightsail.Operation{Status: "succeeded"}, staticIPFound: true, staticIP: ownedIPDetached}, wantAdvance: "wait-detach->delete-instance:"},
		{stage: "delete-instance", provider: &fakeLightsailProvider{instanceFound: true, instance: ownedRemoteInstance(base, "stopped")}, wantAdvance: "delete-instance->wait-delete:op-delete", check: func(t *testing.T, p *fakeLightsailProvider) {
			if p.deleteCalls != 1 {
				t.Fatalf("delete calls=%d", p.deleteCalls)
			}
		}},
		{stage: "wait-delete", provider: &fakeLightsailProvider{operation: lightsail.Operation{Status: "succeeded"}}, wantAdvance: "wait-delete->release-static-ip:"},
		{stage: "release-static-ip", provider: &fakeLightsailProvider{staticIPFound: true, staticIP: ownedIPDetached}, wantAdvance: "release-static-ip->wait-release:op-release", check: func(t *testing.T, p *fakeLightsailProvider) {
			if p.releaseCalls != 1 {
				t.Fatalf("release calls=%d", p.releaseCalls)
			}
		}},
		{stage: "wait-release", provider: &fakeLightsailProvider{operation: lightsail.Operation{Status: "succeeded"}}, wantDone: "succeeded:"},
	}
	for _, tc := range tests {
		t.Run(tc.stage, func(t *testing.T) {
			job := lifecycleWorkerJob("destroy", tc.stage)
			job.Instance.AWSState = "stopped"
			store := &fakeLightsailJobStore{job: job}
			if err := processLightsailJob(context.Background(), store, tc.provider, &fakeSSHRunner{}, workerConfig()); err != nil {
				t.Fatal(err)
			}
			if tc.wantAdvance != "" && !reflect.DeepEqual(store.advanced, []string{tc.wantAdvance}) {
				t.Fatalf("advanced=%v", store.advanced)
			}
			if tc.wantDone != "" && !reflect.DeepEqual(store.completed, []string{tc.wantDone}) {
				t.Fatalf("completed=%v", store.completed)
			}
			if tc.check != nil {
				tc.check(t, tc.provider)
			}
		})
	}
}
