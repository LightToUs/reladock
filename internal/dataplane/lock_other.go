//go:build !linux && !darwin

package dataplane

import (
	"context"
	"errors"
)

type fileLock struct{ path string }

func (*fileLock) Lock(context.Context) (func(), error) {
	return nil, errors.New("file locking unsupported on this OS")
}
