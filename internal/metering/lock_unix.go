//go:build linux || darwin

package metering

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

func openPrivate(path string, flags int) (*os.File, error) {
	return os.OpenFile(path, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
}

func singleLink(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink == 1
}

func lockDirectory(dir string) (func() error, error) {
	f, e := openPrivate(filepath.Join(dir, "wal.lock"), os.O_CREATE|os.O_RDWR)
	if e != nil {
		return nil, e
	}
	if e = privateRegular(f); e != nil {
		_ = f.Close()
		return nil, e
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		_ = f.Close()
		if errors.Is(e, syscall.EAGAIN) || errors.Is(e, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, e
	}
	var once sync.Once
	var closeErr error
	return func() error {
		once.Do(func() { closeErr = errors.Join(syscall.Flock(int(f.Fd()), syscall.LOCK_UN), f.Close()) })
		return closeErr
	}, nil
}
