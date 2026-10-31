package updater

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"reladock/internal/buildinfo"
	"reladock/internal/dataplane"
)

const maxUpdaterStateBytes = 64 << 10

type Backup struct {
	Agent           string `json:"agent"`
	Dataplane       string `json:"dataplane"`
	AgentSHA256     string `json:"agentSha256"`
	DataplaneSHA256 string `json:"dataplaneSha256"`
}

type HealthRequest struct {
	TaskID        string `json:"taskId"`
	Challenge     string `json:"challenge"`
	TargetVersion string `json:"targetVersion"`
}

type State struct {
	TaskID               string         `json:"taskId"`
	LeaseToken           string         `json:"leaseToken"`
	TargetVersion        string         `json:"targetVersion"`
	PreviousVersion      string         `json:"previousVersion"`
	SHA256               string         `json:"sha256,omitempty"`
	Arch                 string         `json:"arch"`
	Unit                 string         `json:"unit,omitempty"`
	Phase                string         `json:"phase"`
	Staged               StagedBinaries `json:"staged,omitempty"`
	Backup               Backup         `json:"backup,omitempty"`
	Health               HealthRequest  `json:"health,omitempty"`
	RollbackCode         string         `json:"rollbackCode,omitempty"`
	RollbackWasVerifying bool           `json:"rollbackWasVerifying,omitempty"`
}

type StateStore struct {
	dir    string
	locker dataplane.Locker
}

func OpenStateStore(dir string) (*StateStore, error) {
	store, err := dataplane.NewFileStore(dir)
	if err != nil {
		return nil, err
	}
	return &StateStore{dir: dir, locker: store.Locker()}, nil
}

func (s *StateStore) Lock(ctx context.Context) (func(), error) { return s.locker.Lock(ctx) }

func validPhase(phase string) bool {
	switch phase {
	case "claimed", "staged", "stopping", "backed-up", "publishing", "published", "starting", "verifying", "restoring", "rollback-failed":
		return true
	default:
		return false
	}
}

func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Geteuid()
}

func within(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != "." && relative != "" && relative != ".." && !filepath.IsAbs(relative) && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func validState(state *State, dir string) bool {
	if state == nil || !upgradeID.MatchString(state.TaskID) || !hexSHA256.MatchString(state.LeaseToken) || !hexSHA256.MatchString(state.SHA256) || !buildVersion(state.PreviousVersion) || !buildVersion(state.TargetVersion) || (state.Arch != "amd64" && state.Arch != "arm64") || !validPhase(state.Phase) {
		return false
	}
	taskRoot := filepath.Join(dir, "tasks", state.TaskID)
	if state.Phase == "claimed" {
		return true
	}
	if !within(taskRoot, state.Staged.Agent) || !within(taskRoot, state.Staged.Dataplane) {
		return false
	}
	if state.Phase == "staged" {
		return true
	}
	if !validUnit(state.Unit) {
		return false
	}
	if state.Phase == "stopping" {
		return true
	}
	backupRoot := filepath.Join(taskRoot, "backup")
	if state.Backup.Agent != filepath.Join(backupRoot, "reladock-agent") || state.Backup.Dataplane != filepath.Join(backupRoot, "reladock-dataplane") || !hexSHA256.MatchString(state.Backup.AgentSHA256) || !hexSHA256.MatchString(state.Backup.DataplaneSHA256) {
		return false
	}
	if state.Phase == "starting" || state.Phase == "verifying" {
		return state.Health.TaskID == state.TaskID && state.Health.TargetVersion == state.TargetVersion && hexSHA256.MatchString(state.Health.Challenge)
	}
	if state.Phase == "restoring" || state.Phase == "rollback-failed" {
		return validUpgradeErrorCode(state.RollbackCode)
	}
	return true
}

func validUpgradeErrorCode(code string) bool {
	switch code {
	case "download-unavailable", "archive-invalid", "checksum-mismatch", "install-failed", "service-failed", "health-timeout", "rollback-failed", "state-invalid":
		return true
	default:
		return false
	}
}

func buildVersion(value string) bool {
	_, _, _, ok := buildinfo.ParseProduction(value)
	return ok
}

func (s *StateStore) Load(ctx context.Context) (*State, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := filepath.Join(s.dir, "state.json")
	dirInfo, dirErr := os.Lstat(s.dir)
	if dirErr != nil || !dirInfo.IsDir() || !ownedByCurrentUser(dirInfo) {
		return nil, errors.New("unsafe updater state directory")
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > maxUpdaterStateBytes || !ownedByCurrentUser(info) {
		return nil, errors.New("unsafe updater state")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var state State
	decoder := json.NewDecoder(io.LimitReader(f, maxUpdaterStateBytes+1))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&state); err != nil || decoder.Decode(new(any)) != io.EOF || !validState(&state, s.dir) {
		return nil, errors.New("invalid updater state")
	}
	return &state, nil
}

func (s *StateStore) Save(ctx context.Context, state *State) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validState(state, s.dir) {
		return errors.New("invalid updater state")
	}
	raw, err := json.Marshal(state)
	if err != nil || len(raw) > maxUpdaterStateBytes {
		return errors.New("invalid updater state")
	}
	f, err := os.CreateTemp(s.dir, ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
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
	if err = os.Rename(f.Name(), filepath.Join(s.dir, "state.json")); err != nil {
		return err
	}
	return syncDirectory(s.dir)
}

func (s *StateStore) Clear() error {
	if err := os.Remove(filepath.Join(s.dir, "state.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirectory(s.dir)
}

func (s *StateStore) CurrentVersion(fallback string) (string, error) {
	path := filepath.Join(s.dir, "agent-version")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if !buildVersion(fallback) {
			return "", errors.New("invalid fallback Agent version")
		}
		return fallback, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 64 || !ownedByCurrentUser(info) {
		return "", errors.New("unsafe Agent version marker")
	}
	raw, err := os.ReadFile(path)
	value := strings.TrimSpace(string(raw))
	if err != nil || !buildVersion(value) {
		return "", errors.New("invalid Agent version marker")
	}
	markerMajor, markerMinor, markerPatch, _ := buildinfo.ParseProduction(value)
	fallbackMajor, fallbackMinor, fallbackPatch, fallbackOK := buildinfo.ParseProduction(fallback)
	if !fallbackOK {
		return "", errors.New("invalid fallback Agent version")
	}
	if markerMajor != fallbackMajor || fallbackMinor > markerMinor || (fallbackMinor == markerMinor && fallbackPatch > markerPatch) {
		return fallback, nil
	}
	return value, nil
}

func (s *StateStore) SetCurrentVersion(version string) error {
	if !buildVersion(version) {
		return errors.New("invalid Agent version")
	}
	f, err := os.CreateTemp(s.dir, ".version-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = io.WriteString(f, version+"\n")
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(s.dir, "agent-version"))
	}
	if err != nil {
		return err
	}
	return syncDirectory(s.dir)
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
