package updater

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"reladock/internal/dataplane"
)

var knownAgentUnits = []string{"reladock-agent.service", "reladock-agent-apply.service"}

type HealthMarker struct {
	TaskID        string    `json:"taskId"`
	Challenge     string    `json:"challenge"`
	TargetVersion string    `json:"targetVersion"`
	CompletedAt   time.Time `json:"completedAt"`
}

type LocalHost struct {
	InstallDir string
	StateDir   string
	ControlDir string
	RequestDir string
	HealthDir  string
	Command    func(context.Context, string, ...string) ([]byte, error)
}

func (h *LocalHost) defaults() {
	if h.InstallDir == "" {
		h.InstallDir = "/opt/reladock"
	}
	if h.StateDir == "" {
		h.StateDir = "/var/lib/reladock-updater"
	}
	if h.ControlDir == "" {
		h.ControlDir = "/var/lib/reladock-agent/control"
	}
	if h.RequestDir == "" {
		h.RequestDir = "/run/reladock-updater"
	}
	if h.HealthDir == "" {
		h.HealthDir = "/run/reladock-agent"
	}
	if h.Command == nil {
		h.Command = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).CombinedOutput()
		}
	}
}

func validUnit(unit string) bool { return unit == knownAgentUnits[0] || unit == knownAgentUnits[1] }

func (h *LocalHost) ActiveUnits(ctx context.Context) ([]string, error) {
	h.defaults()
	var active []string
	for _, unit := range knownAgentUnits {
		if _, err := h.Command(ctx, "systemctl", "is-active", "--quiet", unit); err == nil {
			active = append(active, unit)
		}
	}
	return active, nil
}

func (h *LocalHost) Stop(ctx context.Context, unit string) error {
	h.defaults()
	if !validUnit(unit) {
		return errors.New("unknown Agent unit")
	}
	_, err := h.Command(ctx, "systemctl", "stop", unit)
	return err
}

func (h *LocalHost) Start(ctx context.Context, unit string) error {
	h.defaults()
	if !validUnit(unit) {
		return errors.New("unknown Agent unit")
	}
	_, err := h.Command(ctx, "systemctl", "start", unit)
	return err
}

func (h *LocalHost) ServiceActive(ctx context.Context, unit string) bool {
	h.defaults()
	if !validUnit(unit) {
		return false
	}
	_, err := h.Command(ctx, "systemctl", "is-active", "--quiet", unit)
	return err == nil
}

func (h *LocalHost) EnsureControlIdle(ctx context.Context) error {
	h.defaults()
	store, err := dataplane.NewFileStore(h.ControlDir)
	if err != nil {
		return err
	}
	unlock, err := store.Locker().Lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err = os.Lstat(filepath.Join(h.ControlDir, "delivery.json")); err == nil {
		return errors.New("persistent delivery journal present")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxReleaseFileBytes {
		return "", errors.New("unsafe binary")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	hash := sha256.New()
	if _, err = io.Copy(hash, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func copyDurable(source, destination string, mode os.FileMode) error {
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxReleaseFileBytes {
		return errors.New("unsafe binary source")
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	dir := filepath.Dir(destination)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	out, err := os.CreateTemp(dir, ".binary-*")
	if err != nil {
		return err
	}
	defer os.Remove(out.Name())
	if err = out.Chmod(mode); err == nil {
		_, err = io.Copy(out, io.LimitReader(in, maxReleaseFileBytes+1))
	}
	if err == nil {
		err = out.Sync()
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(out.Name(), destination); err != nil {
		return err
	}
	return syncDirectory(dir)
}

func (h *LocalHost) Backup(ctx context.Context, taskID string) (Backup, error) {
	h.defaults()
	if err := ctx.Err(); err != nil || !upgradeID.MatchString(taskID) {
		return Backup{}, errors.New("invalid backup request")
	}
	dir := filepath.Join(h.StateDir, "tasks", taskID, "backup")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return Backup{}, err
	}
	backup := Backup{Agent: filepath.Join(dir, "reladock-agent"), Dataplane: filepath.Join(dir, "reladock-dataplane")}
	if err := copyDurable(filepath.Join(h.InstallDir, "reladock-agent"), backup.Agent, 0755); err != nil {
		return Backup{}, err
	}
	if err := copyDurable(filepath.Join(h.InstallDir, "reladock-dataplane"), backup.Dataplane, 0755); err != nil {
		return Backup{}, err
	}
	backup.AgentSHA256, _ = fileSHA256(backup.Agent)
	backup.DataplaneSHA256, _ = fileSHA256(backup.Dataplane)
	return backup, nil
}

func (h *LocalHost) Publish(ctx context.Context, staged StagedBinaries) error {
	h.defaults()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := copyDurable(staged.Agent, filepath.Join(h.InstallDir, "reladock-agent"), 0755); err != nil {
		return err
	}
	return copyDurable(staged.Dataplane, filepath.Join(h.InstallDir, "reladock-dataplane"), 0755)
}

func (h *LocalHost) Restore(ctx context.Context, backup Backup) error {
	h.defaults()
	if err := ctx.Err(); err != nil {
		return err
	}
	agentHash, err := fileSHA256(backup.Agent)
	if err != nil || agentHash != backup.AgentSHA256 {
		return errors.New("Agent backup checksum mismatch")
	}
	dataplaneHash, err := fileSHA256(backup.Dataplane)
	if err != nil || dataplaneHash != backup.DataplaneSHA256 {
		return errors.New("Dataplane backup checksum mismatch")
	}
	if err = copyDurable(backup.Agent, filepath.Join(h.InstallDir, "reladock-agent"), 0755); err != nil {
		return err
	}
	return copyDurable(backup.Dataplane, filepath.Join(h.InstallDir, "reladock-dataplane"), 0755)
}

func writeJSONAtomic(dir, name string, value any, mode os.FileMode) error {
	if err := os.MkdirAll(dir, 0750); err != nil {
		return err
	}
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > maxUpdaterStateBytes {
		return errors.New("invalid health state")
	}
	f, err := os.CreateTemp(dir, ".health-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(mode); err == nil {
		_, err = io.Copy(f, bytes.NewReader(raw))
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), filepath.Join(dir, name)); err != nil {
		return err
	}
	return syncDirectory(dir)
}

func (h *LocalHost) ProjectHealth(_ context.Context, request HealthRequest) error {
	h.defaults()
	if !upgradeID.MatchString(request.TaskID) || !hexSHA256.MatchString(request.Challenge) || !buildVersion(request.TargetVersion) {
		return errors.New("invalid health request")
	}
	return writeJSONAtomic(h.RequestDir, "health-request.json", request, 0640)
}

func readHealthMarker(path string) (HealthMarker, error) {
	var marker HealthMarker
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxUpdaterStateBytes {
		return marker, errors.New("health marker unavailable")
	}
	f, err := os.Open(path)
	if err != nil {
		return marker, err
	}
	defer f.Close()
	decoder := json.NewDecoder(io.LimitReader(f, maxUpdaterStateBytes+1))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&marker); err != nil || decoder.Decode(new(any)) != io.EOF || marker.CompletedAt.IsZero() {
		return marker, errors.New("invalid health marker")
	}
	return marker, nil
}

func (h *LocalHost) WaitHealth(ctx context.Context, request HealthRequest, timeout time.Duration) error {
	h.defaults()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		marker, err := readHealthMarker(filepath.Join(h.HealthDir, "upgrade-health.json"))
		if err == nil && marker.TaskID == request.TaskID && marker.Challenge == request.Challenge && marker.TargetVersion == request.TargetVersion {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return context.DeadlineExceeded
		case <-ticker.C:
		}
	}
}
