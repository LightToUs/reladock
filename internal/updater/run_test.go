package updater

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"reladock/internal/platform"
)

type fakeUpgradeControl struct {
	claim   *platform.UpgradeClaim
	reports []platform.UpgradeReport
}

func (f *fakeUpgradeControl) Claim(context.Context, string, string) (*platform.UpgradeClaim, error) {
	return f.claim, nil
}
func (f *fakeUpgradeControl) Report(_ context.Context, report platform.UpgradeReport) error {
	f.reports = append(f.reports, report)
	return nil
}

type fakeUpgradeHost struct {
	units      []string
	stopped    bool
	stopErr    error
	started    int
	startErrAt int
	idleErr    error
	publishErr error
	healthErr  error
	restoreErr error
	active     bool
	backedUp   bool
	published  bool
	restored   bool
	projected  HealthRequest
}

func (f *fakeUpgradeHost) ActiveUnits(context.Context) ([]string, error) { return f.units, nil }
func (f *fakeUpgradeHost) Stop(context.Context, string) error            { f.stopped = true; return f.stopErr }
func (f *fakeUpgradeHost) Start(context.Context, string) error {
	f.started++
	if f.started == f.startErrAt {
		return errors.New("start failed")
	}
	return nil
}
func (f *fakeUpgradeHost) EnsureControlIdle(context.Context) error { return f.idleErr }
func (f *fakeUpgradeHost) Backup(context.Context, string) (Backup, error) {
	f.backedUp = true
	return Backup{Agent: "old-agent", Dataplane: "old-dataplane", AgentSHA256: strings.Repeat("1", 64), DataplaneSHA256: strings.Repeat("2", 64)}, nil
}
func (f *fakeUpgradeHost) Publish(context.Context, StagedBinaries) error {
	f.published = true
	return f.publishErr
}
func (f *fakeUpgradeHost) Restore(context.Context, Backup) error {
	f.restored = true
	return f.restoreErr
}
func (f *fakeUpgradeHost) ProjectHealth(_ context.Context, request HealthRequest) error {
	f.projected = request
	return nil
}
func (f *fakeUpgradeHost) WaitHealth(context.Context, HealthRequest, time.Duration) error {
	return f.healthErr
}
func (f *fakeUpgradeHost) ServiceActive(context.Context, string) bool { return f.active }

func testUpgradeClaim() *platform.UpgradeClaim {
	return &platform.UpgradeClaim{ID: "11111111-1111-4111-8111-111111111111", TargetVersion: "v0.9.0", PreviousVersion: "v0.8.0", SHA256: strings.Repeat("a", 64), LeaseToken: strings.Repeat("b", 64)}
}

func runOptions(t *testing.T, control *fakeUpgradeControl, host *fakeUpgradeHost) Options {
	t.Helper()
	store, err := OpenStateStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	return Options{Store: store, Control: control, Host: host, CurrentVersion: "v0.8.0", Arch: "amd64", HealthTimeout: 20 * time.Millisecond, Stage: func(context.Context, platform.UpgradeClaim) (StagedBinaries, error) {
		return StagedBinaries{Agent: "new-agent", Dataplane: "new-dataplane"}, nil
	}}
}

func TestRunExitsWithoutTaskAndRejectsAmbiguousServices(t *testing.T) {
	control := &fakeUpgradeControl{}
	host := &fakeUpgradeHost{units: []string{"reladock-agent.service"}}
	if err := Run(context.Background(), runOptions(t, control, host)); err != nil || host.stopped {
		t.Fatalf("no-task run: %v stopped=%v", err, host.stopped)
	}
	for _, units := range [][]string{{}, {"reladock-agent.service", "reladock-agent-apply.service"}} {
		control = &fakeUpgradeControl{claim: testUpgradeClaim()}
		host = &fakeUpgradeHost{units: units}
		if err := Run(context.Background(), runOptions(t, control, host)); err == nil || host.published {
			t.Fatalf("ambiguous units accepted: %v err=%v", units, err)
		}
	}
}

func TestRunPublishesOnlyWhenControlIsIdleAndCompletesHealth(t *testing.T) {
	control := &fakeUpgradeControl{claim: testUpgradeClaim()}
	host := &fakeUpgradeHost{units: []string{"reladock-agent.service"}, active: true}
	options := runOptions(t, control, host)
	if err := Run(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	if !host.stopped || !host.backedUp || !host.published || host.restored || host.started != 1 || host.projected.TaskID != control.claim.ID {
		t.Fatalf("lifecycle: %+v", host)
	}
	want := []string{"installing", "verifying", "succeeded"}
	if len(control.reports) != len(want) {
		t.Fatalf("reports: %+v", control.reports)
	}
	for i := range want {
		if control.reports[i].State != want[i] {
			t.Fatalf("report %d = %+v", i, control.reports[i])
		}
	}
	if state, err := options.Store.Load(context.Background()); err != nil || state != nil {
		t.Fatalf("completed state retained: %+v %v", state, err)
	}
}

func TestRunRefusesPersistentApplicationAndRollsBackHealthFailure(t *testing.T) {
	control := &fakeUpgradeControl{claim: testUpgradeClaim()}
	host := &fakeUpgradeHost{units: []string{"reladock-agent-apply.service"}, idleErr: errors.New("delivery journal present")}
	if err := Run(context.Background(), runOptions(t, control, host)); err == nil || host.published || host.started != 1 {
		t.Fatalf("busy control changed binaries: %+v %v", host, err)
	}
	if len(control.reports) != 2 || control.reports[1].State != "failed" || control.reports[1].ErrorCode != "state-invalid" {
		t.Fatalf("busy report: %+v", control.reports)
	}

	control = &fakeUpgradeControl{claim: testUpgradeClaim()}
	host = &fakeUpgradeHost{units: []string{"reladock-agent.service"}, healthErr: context.DeadlineExceeded}
	options := runOptions(t, control, host)
	if err := Run(context.Background(), options); err == nil || !host.restored || host.started != 2 {
		t.Fatalf("health failure not rolled back: %+v %v", host, err)
	}
	last := control.reports[len(control.reports)-1]
	if last.State != "rolled-back" || last.ErrorCode != "health-timeout" {
		t.Fatalf("rollback report: %+v", last)
	}
}

func TestRunRecoversPublishingCrashByRestoringBothBinaries(t *testing.T) {
	control := &fakeUpgradeControl{claim: testUpgradeClaim()}
	host := &fakeUpgradeHost{units: []string{"reladock-agent.service"}, restoreErr: errors.New("restore failed")}
	options := runOptions(t, control, host)
	state := &State{TaskID: control.claim.ID, LeaseToken: control.claim.LeaseToken, TargetVersion: "v0.9.0", PreviousVersion: "v0.8.0", Arch: "amd64", Unit: "reladock-agent.service", Phase: "publishing", Backup: Backup{Agent: "old-agent", Dataplane: "old-dataplane"}}
	if err := options.Store.Save(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), options); err == nil || !host.restored || host.published {
		t.Fatalf("publishing crash guessed success: %+v %v", host, err)
	}
	loaded, err := options.Store.Load(context.Background())
	if err != nil || loaded == nil || loaded.Phase != "rollback-failed" {
		t.Fatalf("rollback failure state lost: %+v %v", loaded, err)
	}
}

func TestRunClearsTerminalPrePublishFailure(t *testing.T) {
	control := &fakeUpgradeControl{claim: testUpgradeClaim()}
	host := &fakeUpgradeHost{units: []string{"reladock-agent.service"}, stopErr: errors.New("stop failed")}
	options := runOptions(t, control, host)
	if err := Run(context.Background(), options); err == nil {
		t.Fatal("stop failure accepted")
	}
	if state, err := options.Store.Load(context.Background()); err != nil || state != nil {
		t.Fatalf("terminal pre-publish state retained: %+v %v", state, err)
	}
	last := control.reports[len(control.reports)-1]
	if last.State != "failed" || last.ErrorCode != "service-failed" {
		t.Fatalf("stop failure report: %+v", last)
	}
}

func TestRunMarksRollbackFailedWhenOldServiceCannotRestart(t *testing.T) {
	control := &fakeUpgradeControl{claim: testUpgradeClaim()}
	host := &fakeUpgradeHost{units: []string{"reladock-agent.service"}, healthErr: context.DeadlineExceeded, startErrAt: 2}
	options := runOptions(t, control, host)
	if err := Run(context.Background(), options); err == nil {
		t.Fatal("rollback restart failure accepted")
	}
	last := control.reports[len(control.reports)-1]
	if last.State != "failed" || last.ErrorCode != "rollback-failed" {
		t.Fatalf("rollback restart failure report: %+v", last)
	}
	state, err := options.Store.Load(context.Background())
	if err != nil || state == nil || state.Phase != "rollback-failed" {
		t.Fatalf("rollback failure state missing: %+v %v", state, err)
	}
}
