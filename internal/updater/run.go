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

type UpgradeControl interface {
	Claim(context.Context, string, string) (*platform.UpgradeClaim, error)
	Report(context.Context, platform.UpgradeReport) error
}

type Host interface {
	ActiveUnits(context.Context) ([]string, error)
	Stop(context.Context, string) error
	Start(context.Context, string) error
	EnsureControlIdle(context.Context) error
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
	defer unlock()
	state, err := options.Store.Load(ctx)
	if err != nil {
		return err
	}
	if state == nil {
		claim, claimErr := options.Control.Claim(ctx, options.Arch, options.CurrentVersion)
		if claimErr != nil || claim == nil {
			return claimErr
		}
		state = &State{TaskID: claim.ID, LeaseToken: claim.LeaseToken, TargetVersion: claim.TargetVersion, PreviousVersion: claim.PreviousVersion, SHA256: claim.SHA256, Arch: options.Arch, Phase: "claimed"}
		if err = options.Store.Save(ctx, state); err != nil {
			return err
		}
	}
	if state.Phase == "publishing" || state.Phase == "restoring" || state.Phase == "rollback-failed" {
		return recoverPartialPublish(ctx, options, state)
	}
	if state.Phase == "claimed" {
		state.Staged, err = options.Stage(ctx, platform.UpgradeClaim{ID: state.TaskID, LeaseToken: state.LeaseToken, TargetVersion: state.TargetVersion, PreviousVersion: state.PreviousVersion, SHA256: state.SHA256})
		if err != nil {
			_ = options.Control.Report(ctx, reportFor(state, "failed", "download-unavailable"))
			_ = options.Store.Clear()
			return err
		}
		state.Phase = "staged"
		if err = options.Store.Save(ctx, state); err != nil {
			return err
		}
	}
	if state.Phase == "staged" {
		units, unitErr := options.Host.ActiveUnits(ctx)
		if unitErr != nil || len(units) != 1 || (units[0] != "reladock-agent.service" && units[0] != "reladock-agent-apply.service") {
			_ = options.Control.Report(ctx, reportFor(state, "failed", "state-invalid"))
			_ = options.Store.Clear()
			if unitErr != nil {
				return unitErr
			}
			return errors.New("exactly one known Agent service must be active")
		}
		state.Unit = units[0]
		if err = options.Control.Report(ctx, reportFor(state, "installing", "")); err != nil {
			return err
		}
		if err = options.Host.Stop(ctx, state.Unit); err != nil {
			reportErr := options.Control.Report(ctx, reportFor(state, "failed", "service-failed"))
			if reportErr == nil {
				_ = options.Store.Clear()
			}
			return errors.Join(err, reportErr)
		}
		if err = options.Host.EnsureControlIdle(ctx); err != nil {
			_ = options.Host.Start(ctx, state.Unit)
			_ = options.Control.Report(ctx, reportFor(state, "failed", "state-invalid"))
			_ = options.Store.Clear()
			return err
		}
		state.Backup, err = options.Host.Backup(ctx, state.TaskID)
		if err != nil {
			_ = options.Host.Start(ctx, state.Unit)
			reportErr := options.Control.Report(ctx, reportFor(state, "failed", "install-failed"))
			if reportErr == nil {
				_ = options.Store.Clear()
			}
			return errors.Join(err, reportErr)
		}
		state.Phase = "backed-up"
		if err = options.Store.Save(ctx, state); err != nil {
			return err
		}
	}
	if state.Phase == "backed-up" {
		state.Phase = "publishing"
		if err = options.Store.Save(ctx, state); err != nil {
			return err
		}
		if err = options.Host.Publish(ctx, state.Staged); err != nil {
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
		if err = options.Host.ProjectHealth(ctx, state.Health); err != nil {
			return rollback(ctx, options, state, "install-failed", err, false)
		}
		state.Phase = "verifying"
		if err = options.Store.Save(ctx, state); err != nil {
			return err
		}
		if err = options.Host.Start(ctx, state.Unit); err != nil {
			return rollback(ctx, options, state, "service-failed", err, false)
		}
		if err = options.Control.Report(ctx, reportFor(state, "verifying", "")); err != nil {
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
	return rollback(ctx, options, state, "install-failed", errors.New("recovering interrupted binary publication"), false)
}

func rollback(ctx context.Context, options Options, state *State, code string, cause error, wasVerifying bool) error {
	state.Phase = "restoring"
	if err := options.Store.Save(ctx, state); err != nil {
		return errors.Join(cause, err)
	}
	if err := options.Host.Restore(ctx, state.Backup); err != nil {
		state.Phase = "rollback-failed"
		_ = options.Store.Save(ctx, state)
		_ = options.Control.Report(ctx, reportFor(state, "failed", "rollback-failed"))
		return errors.Join(cause, err)
	}
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
