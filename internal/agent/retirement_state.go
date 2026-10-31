package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"reladock/internal/platform"
)

const maxRetirementJournal = 128 * 1024

type retirementRecord struct {
	Origin         string                    `json:"origin"`
	NodeID         string                    `json:"nodeId"`
	Claim          platform.RetirementClaim  `json:"claim"`
	Sequence       int64                     `json:"sequence"`
	BeginAttempted bool                      `json:"beginAttempted,omitempty"`
	BeginConfirmed bool                      `json:"beginConfirmed,omitempty"`
	Proof          *platform.RetirementProof `json:"proof,omitempty"`
}

type RetirementState struct{ dir string }

func NewRetirementState(root string) (*RetirementState, error) {
	state, err := NewApplyState(root)
	if err != nil {
		return nil, err
	}
	return &RetirementState{dir: state.dir}, nil
}

func (s *RetirementState) path() string { return filepath.Join(s.dir, "retirement.json") }

func (s *RetirementState) NextSequence(ctx context.Context, floor int64) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	path := filepath.Join(s.dir, "retirement-sequence")
	current := int64(0)
	info, statErr := os.Lstat(path)
	if statErr == nil && (!info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 64) {
		return 0, errors.New("unsafe retirement sequence")
	}
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return 0, statErr
	}
	if raw, err := os.ReadFile(path); err == nil {
		current, err = strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
		if err != nil || current < 0 {
			return 0, errors.New("invalid retirement sequence")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	next := current + 1
	if floor > next {
		next = floor
	}
	file, err := os.CreateTemp(s.dir, ".retirement-sequence-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err = file.Chmod(0600); err != nil {
		return 0, err
	}
	if _, err = file.WriteString(strconv.FormatInt(next, 10) + "\n"); err != nil {
		return 0, err
	}
	if err = file.Sync(); err != nil {
		return 0, err
	}
	if err = file.Close(); err != nil {
		return 0, err
	}
	if err = os.Rename(file.Name(), path); err != nil {
		return 0, err
	}
	if err = syncRetirementDirectory(s.dir); err != nil {
		return 0, err
	}
	return next, nil
}

func (s *RetirementState) CurrentSequence() (int64, error) {
	path := filepath.Join(s.dir, "retirement-sequence")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 64 {
		return 0, errors.New("unsafe retirement sequence")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	value, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil || value < 0 {
		return 0, errors.New("invalid retirement sequence")
	}
	return value, nil
}

func (s *RetirementState) Load(ctx context.Context) (*retirementRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := os.Lstat(s.path())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > maxRetirementJournal {
		return nil, errors.New("unsafe retirement journal")
	}
	raw, err := os.ReadFile(s.path())
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var record retirementRecord
	if err = decoder.Decode(&record); err != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("invalid retirement journal")
	}
	return &record, nil
}

func (s *RetirementState) Save(ctx context.Context, record retirementRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	raw, err := json.Marshal(record)
	if err != nil || len(raw) > maxRetirementJournal {
		return errors.New("retirement journal exceeds bound")
	}
	file, err := os.CreateTemp(s.dir, ".retirement-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err = file.Write(raw); err != nil || file.Sync() != nil || file.Close() != nil {
		return errors.New("persist retirement journal")
	}
	if err = os.Rename(file.Name(), s.path()); err != nil {
		return err
	}
	return syncRetirementDirectory(s.dir)
}

func (s *RetirementState) Clear() error {
	if err := os.Remove(s.path()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncRetirementDirectory(s.dir)
}

func syncRetirementDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
