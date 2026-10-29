package shaping

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

const maxStateSize = 8 * 1024 * 1024

var ErrStoreUncertain = errors.New("shaping state replacement visible but durability uncertain")

type FileStore struct{ dir string }

// NewFileStore creates only the last component (0700). Parent directories must
// be trusted. All processes controlling one lease must use this same lock path.
func NewFileStore(dir string) (*FileStore, error) {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) == string(filepath.Separator) {
		return nil, errors.New("absolute private shaping directory required")
	}
	if e := os.Mkdir(dir, 0700); e != nil && !errors.Is(e, os.ErrExist) {
		return nil, e
	}
	info, e := os.Lstat(dir)
	if e != nil {
		return nil, e
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("private nonsymlink directory required")
	}
	return &FileStore{dir}, nil
}
func (s *FileStore) Load(ctx context.Context) (State, error) {
	var v State
	if e := ctx.Err(); e != nil {
		return v, e
	}
	path := filepath.Join(s.dir, "state.json")
	info, e := os.Lstat(path)
	if errors.Is(e, os.ErrNotExist) {
		return v, nil
	}
	if e != nil {
		return v, e
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > maxStateSize {
		return v, errors.New("unsafe shaping state file")
	}
	f, e := os.Open(path)
	if e != nil {
		return v, e
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, maxStateSize+1))
	d.DisallowUnknownFields()
	if e = d.Decode(&v); e != nil {
		return v, e
	}
	if d.Decode(new(any)) != io.EOF {
		return v, errors.New("trailing shaping state")
	}
	return v, nil
}
func (s *FileStore) Save(ctx context.Context, v State) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	if len(b) > maxStateSize {
		return errors.New("shaping state exceeds 8 MiB")
	}
	f, e := os.CreateTemp(s.dir, ".shaping-*")
	if e != nil {
		return e
	}
	defer f.Close()
	defer os.Remove(f.Name())
	if _, e = io.Copy(f, bytes.NewReader(b)); e != nil {
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
		return errors.Join(ErrStoreUncertain, e)
	}
	defer d.Close()
	if e = d.Sync(); e != nil {
		return errors.Join(ErrStoreUncertain, e)
	}
	return nil
}
func (s *FileStore) Locker() Locker { return &fileLock{filepath.Join(s.dir, "lock")} }
