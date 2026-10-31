package updater

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

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
	TaskID          string         `json:"taskId"`
	LeaseToken      string         `json:"leaseToken"`
	TargetVersion   string         `json:"targetVersion"`
	PreviousVersion string         `json:"previousVersion"`
	SHA256          string         `json:"sha256,omitempty"`
	Arch            string         `json:"arch"`
	Unit            string         `json:"unit,omitempty"`
	Phase           string         `json:"phase"`
	Staged          StagedBinaries `json:"staged,omitempty"`
	Backup          Backup         `json:"backup,omitempty"`
	Health          HealthRequest  `json:"health,omitempty"`
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
	case "claimed", "staged", "backed-up", "publishing", "published", "verifying", "restoring", "rollback-failed":
		return true
	default:
		return false
	}
}

func validState(state *State) bool {
	return state != nil && upgradeID.MatchString(state.TaskID) && hexSHA256.MatchString(state.LeaseToken) && buildVersion(state.PreviousVersion) && buildVersion(state.TargetVersion) && (state.Arch == "amd64" || state.Arch == "arm64") && validPhase(state.Phase)
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
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > maxUpdaterStateBytes {
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
	if err = decoder.Decode(&state); err != nil || decoder.Decode(new(any)) != io.EOF || !validState(&state) {
		return nil, errors.New("invalid updater state")
	}
	return &state, nil
}

func (s *StateStore) Save(ctx context.Context, state *State) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validState(state) {
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

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
