//go:build !linux && !darwin

package metering

import (
	"errors"
	"os"
)

func openPrivate(string, int) (*os.File, error) {
	return nil, errors.New("metering durable storage unsupported on this OS")
}
func lockDirectory(string) (func() error, error) {
	return nil, errors.New("metering locking unsupported on this OS")
}

func singleLink(os.FileInfo) bool { return false }
