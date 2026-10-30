package dataplane

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
)

// ExecRunner is an opt-in Linux-only runner. The binary path must be absolute
// and trusted by the operator; there is no shell and no PATH resolution.
type ExecRunner struct{ path string }

func NewExecRunner(path string) (*ExecRunner, error) {
	if runtime.GOOS != "linux" {
		return nil, errors.New("nft execution is supported only on Linux")
	}
	if !filepath.IsAbs(path) {
		return nil, errors.New("nft binary must be an absolute path")
	}
	return &ExecRunner{path}, nil
}

type boundedOutput struct {
	bytes.Buffer
	limit int
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, errors.New("nft output exceeds limit")
	}
	return b.Buffer.Write(p)
}

func (r *ExecRunner) Run(ctx context.Context, args []string, input string) ([]byte, error) {
	if runtime.GOOS != "linux" {
		return nil, errors.New("nft execution is supported only on Linux")
	}
	allowed := false
	for _, v := range [][]string{{"-c", "-f", "-"}, {"-f", "-"}, {"-j", "list", "tables"}, {"-j", "list", "ruleset"}, {"-j", "list", "table", "ip", Table}, {"-a", "list", "table", "ip", Table}} {
		if reflect.DeepEqual(args, v) {
			allowed = true
		}
	}
	if !allowed {
		return nil, errors.New("unsupported nft command")
	}
	cmd := exec.CommandContext(ctx, r.path, args...)
	cmd.Stdin = strings.NewReader(input)
	stdout, stderr := &boundedOutput{limit: 16 * 1024 * 1024}, &boundedOutput{limit: 64 * 1024}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if e := cmd.Run(); e != nil {
		return nil, fmt.Errorf("nft command failed: %w: %s", e, stderr.String())
	}
	return stdout.Bytes(), nil
}
