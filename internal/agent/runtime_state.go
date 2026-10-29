package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"reladock/internal/dataplane"
	"reladock/internal/platform"
)

type applyRecord struct {
	Origin         string                 `json:"origin"`
	NodeID         string                 `json:"nodeId"`
	Job            platform.ConfigJob     `json:"job"`
	BeginAttempted bool                   `json:"beginAttempted,omitempty"`
	BeginConfirmed bool                   `json:"beginConfirmed,omitempty"`
	FailureStable  bool                   `json:"failureStable,omitempty"`
	Report         *platform.ConfigReport `json:"report,omitempty"`
	// RawMessage is compacted by json.Marshal. Keep the original checksum-bound
	// payload as base64 bytes instead, including its exact whitespace.
	PayloadBytes []byte `json:"payloadBytes"`
}

// ApplyState owns a private control journal and cross-process cycle lock.
// The adapter uses a separate dataplane subdirectory and its own kernel lock.
// All processes for a node/network namespace must use the same state root.
type ApplyState struct {
	dir    string
	locker dataplane.Locker
}

func NewApplyState(root string) (*ApplyState, error) {
	if _, err := dataplane.NewFileStore(root); err != nil {
		return nil, err
	}
	dir := filepath.Join(root, "control")
	store, err := dataplane.NewFileStore(dir)
	if err != nil {
		return nil, err
	}
	return &ApplyState{dir: dir, locker: store.Locker()}, nil
}
func (s *ApplyState) Lock(ctx context.Context) (func(), error) { return s.locker.Lock(ctx) }
func (s *ApplyState) load(ctx context.Context) (*applyRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := filepath.Join(s.dir, "delivery.json")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 4<<20 {
		return nil, errors.New("unsafe agent delivery journal")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var record applyRecord
	decoder := json.NewDecoder(io.LimitReader(f, (4<<20)+1))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&record); err != nil {
		return nil, err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("trailing agent journal data")
	}
	record.Job.Payload = record.PayloadBytes
	record.PayloadBytes = nil
	return &record, nil
}
func (s *ApplyState) save(ctx context.Context, record *applyRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	disk := *record
	disk.PayloadBytes = []byte(record.Job.Payload)
	disk.Job.Payload = nil
	data, err := json.Marshal(disk)
	if err != nil {
		return err
	}
	if len(data) > 4<<20 {
		return errors.New("agent journal exceeds size bound")
	}
	f, err := os.CreateTemp(s.dir, ".delivery-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err = io.Copy(f, bytes.NewReader(data)); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), filepath.Join(s.dir, "delivery.json")); err != nil {
		return err
	}
	return s.sync()
}
func (s *ApplyState) clear() error {
	if err := os.Remove(filepath.Join(s.dir, "delivery.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return s.sync()
}
func (s *ApplyState) sync() error {
	d, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
