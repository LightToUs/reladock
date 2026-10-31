package updater

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"reladock/internal/platform"
)

const DefaultHealthTimeout = 120 * time.Second

type StageError struct {
	Code string
	Err  error
}

func (e *StageError) Error() string { return e.Code + ": " + e.Err.Error() }
func (e *StageError) Unwrap() error { return e.Err }
func NewStageError(code string, err error) error {
	if err == nil || (code != "download-unavailable" && code != "archive-invalid" && code != "checksum-mismatch") {
		return errors.New("invalid stage error")
	}
	return &StageError{Code: code, Err: err}
}

type UpgradeControl interface {
	Claim(context.Context, string, string) (*platform.UpgradeClaim, error)
	Report(context.Context, platform.UpgradeReport) error
}

type Host interface {
	ActiveUnits(context.Context) ([]string, error)
	Stop(context.Context, string) error
	Start(context.Context, string) error
	LockControlIdle(context.Context) (func(), error)
	Backup(context.Context, string) (Backup, error)
	Publish(context.Context, StagedBinaries) error
	Restore(context.Context, Backup) error
	ProjectHealth(context.Context, HealthRequest) error
	WaitHealth(context.Context, HealthRequest, time.Duration) error
	ServiceActive(context.Context, string) bool
}

type Options struct {
	Store          *StateStore
	Control        UpgradeControl
	Host           Host
	CurrentVersion string
	Arch           string
	HealthTimeout  time.Duration
	Stage          func(context.Context, platform.UpgradeClaim) (StagedBinaries, error)
}

func (o Options) validate() error {
	if o.Store == nil || o.Control == nil || o.Host == nil || o.Stage == nil || !buildVersion(o.CurrentVersion) || (o.Arch != "amd64" && o.Arch != "arm64") {
		return errors.New("invalid updater options")
	}
	if o.HealthTimeout == 0 {
		o.HealthTimeout = DefaultHealthTimeout
	}
	if o.HealthTimeout <= 0 || o.HealthTimeout > DefaultHealthTimeout {
		return errors.New("invalid health timeout")
	}
	return nil
}

func newChallenge() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func Run(ctx context.Context, options Options) error {
	if err := options.validate(); err != nil {
		return err
	}
	if options.HealthTimeout == 0 {
		options.HealthTimeout = DefaultHealthTimeout
	}
	unlock, err := options.Store.Lock(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if unlock != nil {
			unlock()
		}
	}()
	state, err := options.Store.Load(ctx)
	if err != nil {
		return err
	}
	if state == nil {
		currentVersion, versionErr := options.Store.CurrentVersion(options.CurrentVersion)
		if versionErr != nil {
			return versionErr
		}
		claim, claimErr := options.Control.Claim(ctx, options.Arch, currentVersion)
		if claimErr != nil || claim == nil {
			return claimErr
		}
		state = &State{TaskID: claim.ID, LeaseToken: claim.LeaseToken, TargetVersion: claim.TargetVersion, PreviousVersion: claim.PreviousVersion, SHA256: claim.SHA256, Arch: options.Arch, Phase: "claimed"}
		if err = options.Store.Save(ctx, state); err != nil {
			return err
		}
	}
	clearPreMutation := func(reportErr error) error {
		if reportErr != nil && !errors.Is(reportErr, ErrReportRejected) {
			return reportErr
		}
		return options.Store.Clear()
	}
	if state.Phase == "publishing" || state.Phase == "restoring" || state.Phase == "rollback-failed" {
		return recoverPartialPublish(ctx, options, state)
	}
	if state.Phase == "claimed" {
		state.Staged, err = options.Stage(ctx, platform.UpgradeClaim{ID: state.TaskID, LeaseToken: state.LeaseToken, TargetVersion: state.TargetVersion, PreviousVersion: state.PreviousVersion, SHA256: state.SHA256})
		if err != nil {
			code := "download-unavailable"
			var stageErr *StageError
			if errors.As(err, &stageErr) {
				code = stageErr.Code
			}
			reportErr := options.Control.Report(ctx, reportFor(state, "failed", code))
			return errors.Join(err, clearPreMutation(reportErr))
		}
		state.Phase = "staged"
		if err = options.Store.Save(ctx, state); err != nil {
			return err
		}
	}
	if state.Phase == "staged" {
		units, unitErr := options.Host.ActiveUnits(ctx)
		if unitErr != nil || len(units) != 1 || (units[0] != "reladock-agent.service" && units[0] != "reladock-agent-apply.service") {
			reportErr := options.Control.Report(ctx, reportFor(state, "failed", "state-invalid"))
			clearErr := clearPreMutation(reportErr)
			if unitErr != nil {
				return errors.Join(unitErr, clearErr)
			}
			return errors.Join(errors.New("exactly one known Agent service must be active"), clearErr)
		}
		state.Unit = units[0]
		state.Phase = "stopping"
		if err = options.Store.Save(ctx, state); err != nil {
			return err
		}
	}
	var controlUnlock func()
	unlockControl := func() {
		if controlUnlock != nil {
			controlUnlock()
			controlUnlock = nil
		}
	}
	defer unlockControl()
	lockStoppedControl := func() error {
		if controlUnlock != nil {
			return nil
		}
		var lockErr error
		controlUnlock, lockErr = options.Host.LockControlIdle(ctx)
		return lockErr
	}
	if state.Phase == "stopping" {
		if err = lockStoppedControl(); err != nil {
			reportErr := options.Control.Report(ctx, reportFor(state, "failed", "state-invalid"))
			return errors.Join(err, clearPreMutation(reportErr))
		}
		if err = options.Control.Report(ctx, reportFor(state, "installing", "")); err != nil {
			if errors.Is(err, ErrReportRejected) {
				return options.Store.Clear()
			}
			return err
		}
		if err = options.Host.Stop(ctx, state.Unit); err != nil {
			reportErr := options.Control.Report(ctx, reportFor(state, "failed", "service-failed"))
			return errors.Join(err, clearPreMutation(reportErr))
		}
		state.Backup, err = options.Host.Backup(ctx, state.TaskID)
		if err != nil {
			_ = options.Host.Start(ctx, state.Unit)
			reportErr := options.Control.Report(ctx, reportFor(state, "failed", "install-failed"))
			return errors.Join(err, clearPreMutation(reportErr))
		}
		state.Phase = "backed-up"
		if err = options.Store.Save(ctx, state); err != nil {
			return err
		}
	}
	if state.Phase == "backed-up" {
		if err = lockStoppedControl(); err != nil {
			return err
		}
		if err = options.Host.Stop(ctx, state.Unit); err != nil {
			return err
		}
		state.Phase = "publishing"
		if err = options.Store.Save(ctx, state); err != nil {
			return err
		}
		if err = options.Host.Publish(ctx, state.Staged); err != nil {
			unlockControl()
			return rollback(ctx, options, state, "install-failed", err, false)
		}
		state.Phase = "published"
		if err = options.Store.Save(ctx, state); err != nil {
			return err
		}
	}
	if state.Phase == "published" {
		challenge, challengeErr := newChallenge()
		if challengeErr != nil {
			return rollback(ctx, options, state, "install-failed", challengeErr, false)
		}
		state.Health = HealthRequest{TaskID: state.TaskID, Challenge: challenge, TargetVersion: state.TargetVersion}
		state.Phase = "starting"
		if err = options.Store.Save(ctx, state); err != nil {
			return err
		}
	}
	if state.Phase == "starting" {
		unlockControl()
		if err = options.Host.ProjectHealth(ctx, state.Health); err != nil {
			return rollback(ctx, options, state, "install-failed", err, false)
		}
		if err = options.Host.Start(ctx, state.Unit); err != nil {
			return rollback(ctx, options, state, "service-failed", err, false)
		}
		if err = options.Control.Report(ctx, reportFor(state, "verifying", "")); err != nil {
			return err
		}
		state.Phase = "verifying"
		if err = options.Store.Save(ctx, state); err != nil {
			return err
		}
	}
	if state.Phase == "verifying" {
		if err = options.Host.WaitHealth(ctx, state.Health, options.HealthTimeout); err != nil {
			return rollback(ctx, options, state, "health-timeout", err, true)
		}
		if !options.Host.ServiceActive(ctx, state.Unit) {
			return rollback(ctx, options, state, "service-failed", errors.New("upgraded service inactive"), true)
		}
		if err = options.Control.Report(ctx, reportFor(state, "succeeded", "")); err != nil {
			return err
		}
		if err = options.Store.SetCurrentVersion(state.TargetVersion); err != nil {
			return err
		}
		return options.Store.Clear()
	}
	return errors.New("unhandled updater phase")
}

func reportFor(state *State, status, code string) platform.UpgradeReport {
	return platform.UpgradeReport{ID: state.TaskID, LeaseToken: state.LeaseToken, State: status, ErrorCode: code}
}

func recoverPartialPublish(ctx context.Context, options Options, state *State) error {
	if state.Phase == "rollback-failed" {
		return errors.New("prior rollback failed; manual recovery required")
	}
	if state.Phase == "restoring" {
		return rollback(ctx, options, state, state.RollbackCode, errors.New("recovering interrupted rollback"), state.RollbackWasVerifying)
	}
	return rollback(ctx, options, state, "install-failed", errors.New("recovering interrupted binary publication"), false)
}

func rollback(ctx context.Context, options Options, state *State, code string, cause error, wasVerifying bool) error {
	state.Phase = "restoring"
	state.RollbackCode = code
	state.RollbackWasVerifying = wasVerifying
	if err := options.Store.Save(ctx, state); err != nil {
		return errors.Join(cause, err)
	}
	unlock, lockErr := options.Host.LockControlIdle(ctx)
	if lockErr != nil {
		state.Phase = "rollback-failed"
		_ = options.Store.Save(ctx, state)
		_ = options.Control.Report(ctx, reportFor(state, "failed", "rollback-failed"))
		return errors.Join(cause, lockErr)
	}
	defer unlock()
	if err := options.Host.Stop(ctx, state.Unit); err != nil {
		state.Phase = "rollback-failed"
		_ = options.Store.Save(ctx, state)
		_ = options.Control.Report(ctx, reportFor(state, "failed", "rollback-failed"))
		return errors.Join(cause, err)
	}
	if err := options.Host.Restore(ctx, state.Backup); err != nil {
		state.Phase = "rollback-failed"
		_ = options.Store.Save(ctx, state)
		_ = options.Control.Report(ctx, reportFor(state, "failed", "rollback-failed"))
		return errors.Join(cause, err)
	}
	unlock()
	unlock = nil
	startErr := options.Host.Start(ctx, state.Unit)
	if startErr != nil {
		state.Phase = "rollback-failed"
		_ = options.Store.Save(ctx, state)
		reportErr := options.Control.Report(ctx, reportFor(state, "failed", "rollback-failed"))
		return errors.Join(cause, startErr, reportErr)
	}
	status := "failed"
	if wasVerifying {
		status = "rolled-back"
	}
	if reportErr := options.Control.Report(ctx, reportFor(state, status, code)); reportErr != nil {
		return errors.Join(cause, startErr, reportErr)
	}
	if clearErr := options.Store.Clear(); clearErr != nil {
		return errors.Join(cause, clearErr)
	}
	return cause
}
