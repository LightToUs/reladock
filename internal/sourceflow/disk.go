//go:build linux || darwin

package sourceflow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

type walDiskImage struct {
	SHA256 string `json:"sha256"`
	Data   []byte `json:"data"`
}

func strictWALJSON(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return ErrCorrupt
	}
	return nil
}

func encodeWALSnapshot(state walSnapshot) ([]byte, error) {
	data, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	return json.Marshal(walDiskImage{SHA256: hex.EncodeToString(sum[:]), Data: data})
}

func Open(dir string, options Options) (*WAL, error) {
	options, stored, err := normalizedOptions(options)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(dir) || filepath.Clean(dir) == string(filepath.Separator) {
		return nil, errors.New("absolute private source-flow WAL directory required")
	}
	created := false
	if err = os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	} else if err == nil {
		created = true
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("source-flow WAL directory must be private and not a symlink")
	}
	if created {
		if err = syncWALDirectory(filepath.Dir(dir)); err != nil {
			return nil, err
		}
	}
	_, lockErr := os.Lstat(filepath.Join(dir, "wal.lock"))
	oldLock := lockErr == nil
	if lockErr != nil && !errors.Is(lockErr, os.ErrNotExist) {
		return nil, lockErr
	}
	unlock, err := lockWALDirectory(dir)
	if err != nil {
		return nil, err
	}
	w := &WAL{dir: dir, options: options, unlock: unlock}
	ok := false
	defer func() {
		if !ok {
			_ = unlock()
		}
	}()
	w.persist = func(ctx context.Context, data []byte) error { return atomicWALSnapshot(ctx, dir, data) }
	data, err := readWALBounded(filepath.Join(dir, "wal.json"), options.MaxBytes)
	if errors.Is(err, os.ErrNotExist) {
		if oldLock {
			return nil, ErrCorrupt
		}
		if _, nextErr := os.Lstat(filepath.Join(dir, "wal.next")); nextErr == nil || !errors.Is(nextErr, os.ErrNotExist) {
			return nil, ErrCorrupt
		}
		id, randomErr := randomBytes(16)
		if randomErr != nil {
			return nil, randomErr
		}
		epoch, randomErr := randomUUID()
		if randomErr != nil {
			return nil, randomErr
		}
		state := walSnapshot{Version: sourceWALVersion, Options: stored, ID: hex.EncodeToString(id), CollectorEpoch: epoch, CollectionEnabled: options.CollectionEnabled, CleanShutdown: false, CheckpointAt: time.Now().UTC().Truncate(time.Microsecond)}
		if err = w.commit(context.Background(), state); err != nil {
			return nil, err
		}
	} else {
		if err != nil {
			return nil, err
		}
		var image walDiskImage
		if err = strictWALJSON(data, &image); err != nil {
			return nil, errors.Join(ErrCorrupt, err)
		}
		sum := sha256.Sum256(image.Data)
		if image.SHA256 != hex.EncodeToString(sum[:]) {
			return nil, ErrCorrupt
		}
		var state walSnapshot
		if err = strictWALJSON(image.Data, &state); err != nil {
			return nil, errors.Join(ErrCorrupt, err)
		}
		if err = validateSnapshotState(state, stored); err != nil {
			return nil, err
		}
		if !state.CleanShutdown && state.CollectionEnabled {
			if len(state.Gaps) >= options.MaxGaps || state.Sequence == int64(^uint64(0)>>1) {
				return nil, ErrCorrupt
			}
			state.Sequence++
			state.Gaps = append(state.Gaps, Gap{NodeID: options.NodeID, CollectorEpoch: state.CollectorEpoch, Sequence: state.Sequence, Reason: "collector-restart", OpenedAt: state.CheckpointAt})
		}
		epoch, randomErr := randomUUID()
		if randomErr != nil {
			return nil, randomErr
		}
		state.CollectorEpoch = epoch
		state.Sequence = 0
		state.CollectionEnabled = options.CollectionEnabled
		state.CleanShutdown = false
		if err = w.commit(context.Background(), state); err != nil {
			return nil, err
		}
	}
	ok = true
	return w, nil
}

func (w *WAL) commit(ctx context.Context, next walSnapshot) error {
	next.CheckpointAt = time.Now().UTC().Truncate(time.Microsecond)
	encoded, err := encodeWALSnapshot(next)
	if err != nil {
		return err
	}
	if int64(len(encoded)) > w.options.MaxBytes {
		return ErrFull
	}
	return w.commitEncoded(ctx, next, encoded)
}

func (w *WAL) commitEncoded(ctx context.Context, next walSnapshot, encoded []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	next.CheckpointAt = time.Now().UTC().Truncate(time.Microsecond)
	var err error
	encoded, err = encodeWALSnapshot(next)
	if err != nil {
		return err
	}
	if int64(len(encoded)) > w.options.MaxBytes {
		return ErrFull
	}
	if err = w.persist(ctx, encoded); err != nil {
		w.uncertain = true
		return errors.Join(ErrUncertain, err)
	}
	w.state = next
	return nil
}

func openWALPrivate(path string, flags int) (*os.File, error) {
	return os.OpenFile(path, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
}

func privateWALRegular(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || !ok || stat.Nlink != 1 {
		return errors.New("unsafe source-flow WAL file")
	}
	return nil
}

func lockWALDirectory(dir string) (func() error, error) {
	file, err := openWALPrivate(filepath.Join(dir, "wal.lock"), os.O_CREATE|os.O_RDWR)
	if err != nil {
		return nil, err
	}
	if err = privateWALRegular(file); err != nil {
		file.Close()
		return nil, err
	}
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, err
	}
	var once sync.Once
	var closeErr error
	return func() error {
		once.Do(func() { closeErr = errors.Join(syscall.Flock(int(file.Fd()), syscall.LOCK_UN), file.Close()) })
		return closeErr
	}, nil
}

func readWALBounded(path string, max int64) ([]byte, error) {
	file, err := openWALPrivate(path, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if err = privateWALRegular(file); err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || info.Size() > max {
		if err != nil {
			return nil, err
		}
		return nil, ErrCorrupt
	}
	data, err := io.ReadAll(io.LimitReader(file, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, ErrCorrupt
	}
	return data, nil
}

func syncWALDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func atomicWALSnapshot(ctx context.Context, dir string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	scratch := filepath.Join(dir, "wal.next")
	file, err := openWALPrivate(scratch, os.O_CREATE|os.O_RDWR)
	if err != nil {
		return err
	}
	defer file.Close()
	if err = privateWALRegular(file); err != nil {
		return err
	}
	if err = file.Truncate(0); err != nil {
		return err
	}
	if _, err = io.Copy(file, bytes.NewReader(data)); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = os.Rename(scratch, filepath.Join(dir, "wal.json")); err != nil {
		return err
	}
	if err = syncWALDirectory(dir); err != nil {
		return errors.Join(ErrUncertain, err)
	}
	return nil
}
