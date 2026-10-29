package dataplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const maxStateBytes = 8 * 1024 * 1024

// ErrStateUncertain marks an error after the state file has been renamed:
// callers must not assume the old state remains visible or durable.
var ErrStateUncertain = errors.New("state replacement visible but durability uncertain")

type FileStore struct{ dir string }

// NewFileStore requires an absolute path to a private directory (0700), with
// trusted parent directories. It creates only that final directory if absent.
// Every process controlling this network namespace must use this same path.
func NewFileStore(dir string) (*FileStore, error) {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) == string(filepath.Separator) {
		return nil, errors.New("state directory must be an absolute private path")
	}
	if e := os.Mkdir(dir, 0700); e != nil && !errors.Is(e, os.ErrExist) {
		return nil, e
	}
	info, e := os.Lstat(dir)
	if e != nil {
		return nil, e
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("state directory must be private and not a symlink")
	}
	return &FileStore{dir}, nil
}

func (s *FileStore) Load(ctx context.Context) (State, error) {
	var state State
	if e := ctx.Err(); e != nil {
		return state, e
	}
	path := filepath.Join(s.dir, "state.json")
	info, e := os.Lstat(path)
	if errors.Is(e, os.ErrNotExist) {
		return state, nil
	}
	if e != nil {
		return state, e
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > maxStateBytes {
		return state, errors.New("unsafe state file")
	}
	f, e := os.Open(path)
	if e != nil {
		return state, e
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, maxStateBytes+1))
	d.DisallowUnknownFields()
	if e = d.Decode(&state); e != nil {
		return state, e
	}
	if e = d.Decode(new(any)); e != io.EOF {
		return state, errors.New("trailing state JSON")
	}
	return state, nil
}

func (s *FileStore) Save(ctx context.Context, state State) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	data, e := json.Marshal(state)
	if e != nil {
		return e
	}
	if len(data) > maxStateBytes {
		return errors.New("state exceeds size limit")
	}
	f, e := os.CreateTemp(s.dir, ".state-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, e = io.Copy(f, bytes.NewReader(data)); e != nil {
		return e
	}
	if e = f.Sync(); e != nil {
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	if e = ctx.Err(); e != nil {
		return e
	}
	if e = os.Rename(f.Name(), filepath.Join(s.dir, "state.json")); e != nil {
		return e
	}
	d, e := os.Open(s.dir)
	if e != nil {
		return errors.Join(ErrStateUncertain, e)
	}
	defer d.Close()
	if e = d.Sync(); e != nil {
		return errors.Join(ErrStateUncertain, fmt.Errorf("sync state directory: %w", e))
	}
	return nil
}

func (s *FileStore) Locker() Locker { return &fileLock{path: filepath.Join(s.dir, "lock")} }
