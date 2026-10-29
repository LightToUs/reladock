//go:build !linux && !darwin

package shaping

import (
	"context"
	"errors"
)

type fileLock struct{ path string }

func (*fileLock) Lock(context.Context) (func(), error) {
	return nil, errors.New("shaping file lock unsupported on this OS")
}
