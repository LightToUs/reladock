package recovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

var nodeRoots = []struct {
	path     string
	optional bool
}{{"etc/reladock", false}, {"var/lib/reladock-agent", false}, {"var/lib/reladock-updater", true}}
var recoveryUnits = []string{"reladock-agent.service", "reladock-agent-apply.service", "reladock-updater.service", "reladock-updater.timer"}

type NodeCreateConfig struct{ Destination, Recipient, AgeBinary, SystemctlBinary, ControlLock string }
type NodeVerifyConfig struct{ Bundle, Identity, AgeBinary, Staging string }

func nodePreflight(goos string, euid int) error {
	if goos != "linux" {
		return errors.New("node recovery creation requires Linux")
	}
	if euid != 0 {
		return errors.New("node recovery creation requires root")
	}
	return nil
}

type controlLock struct{ *os.File }

func (l *controlLock) Close() error {
	_ = syscall.Flock(int(l.Fd()), syscall.LOCK_UN)
	return l.File.Close()
}
func lockControl(path string) (*controlLock, error) {
	if err := regularAbsolute(path); err != nil {
		return nil, fmt.Errorf("invalid control lock: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, errors.New("node control lock is busy")
	}
	return &controlLock{f}, nil
}

func requireInactiveUnits(ctx context.Context, systemctl string) error {
	if err := regularAbsolute(systemctl); err != nil {
		return fmt.Errorf("invalid systemctl binary: %w", err)
	}
	for _, unit := range recoveryUnits {
		cmd := exec.CommandContext(ctx, systemctl, "is-active", "--quiet", unit)
		var stderr limitBuffer
		cmd.Stderr = &stderr
		err := cmd.Run()
		if err == nil {
			return fmt.Errorf("%s is active", unit)
		}
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 3 {
			return commandError("systemctl is-active "+unit, &stderr, err)
		}
	}
	return nil
}

func collectNodeSources(root string) ([]SourceFile, error) {
	var result []SourceFile
	for _, allowed := range nodeRoots {
		base := filepath.Join(root, filepath.FromSlash(allowed.path))
		info, err := os.Lstat(base)
		if os.IsNotExist(err) && allowed.optional {
			continue
		}
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, fmt.Errorf("node state root %q is not a real directory", base)
		}
		err = filepath.WalkDir(base, func(current string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			info, err := os.Lstat(current)
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("node state contains symlink %q", current)
			}
			relative, err := filepath.Rel(root, current)
			if err != nil {
				return err
			}
			archive := filepath.ToSlash(relative)
			if entry.IsDir() {
				result = append(result, SourceFile{Path: archive, Source: current, Mode: uint32(info.Mode().Perm()), Directory: true})
				return nil
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("node state contains unsupported file %q", current)
			}
			if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Nlink > 1 {
				return fmt.Errorf("node state contains multiply-linked file %q", current)
			}
			result = append(result, SourceFile{Path: archive, Source: current, Mode: uint32(info.Mode().Perm())})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

func nodeSourceID(root string) (string, error) {
	certificate, err := os.ReadFile(filepath.Join(root, "etc/reladock/node-pki/agent.crt"))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(certificate)
	return hex.EncodeToString(sum[:16]), nil
}

func safeNodeStaging(staging string) error {
	if !filepath.IsAbs(staging) || filepath.Clean(staging) != staging {
		return errors.New("node staging path must be a clean absolute path")
	}
	for _, live := range []string{"/etc", "/var"} {
		if staging == live || strings.HasPrefix(staging, live+string(filepath.Separator)) {
			return errors.New("node staging path cannot be inside a live system root")
		}
	}
	if _, err := os.Lstat(staging); !os.IsNotExist(err) {
		return errors.New("node staging path already exists")
	}
	return nil
}

func CreateNode(ctx context.Context, c NodeCreateConfig) (string, error) {
	if err := nodePreflight(runtime.GOOS, os.Geteuid()); err != nil {
		return "", err
	}
	if err := requireInactiveUnits(ctx, c.SystemctlBinary); err != nil {
		return "", err
	}
	lock, err := lockControl(c.ControlLock)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	sources, err := collectNodeSources(string(filepath.Separator))
	if err != nil {
		return "", err
	}
	source, err := nodeSourceID(string(filepath.Separator))
	if err != nil {
		return "", err
	}
	m := Manifest{Version: FormatVersion, Kind: KindNode, CreatedAt: time.Now().UTC(), SourceID: source, Invariants: map[string]string{"services": "inactive", "controlLock": "held"}}
	return Encrypt(ctx, c.AgeBinary, c.Destination, KindNode, c.Recipient, func(w io.Writer) error { _, writeErr := WriteBundle(w, m, sources); return writeErr })
}

func VerifyNode(ctx context.Context, c NodeVerifyConfig) (Manifest, error) {
	var m Manifest
	if err := safeNodeStaging(c.Staging); err != nil {
		return m, err
	}
	err := Decrypt(ctx, c.AgeBinary, c.Bundle, c.Identity, func(r io.Reader) error {
		var extractErr error
		m, extractErr = ExtractBundle(r, c.Staging, KindNode)
		return extractErr
	})
	return m, err
}
