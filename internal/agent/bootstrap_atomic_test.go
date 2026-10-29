//go:build darwin || linux

package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func TestBootstrapAtomicPrivateWriteNeverPublishesTruncatedCredential(t *testing.T) {
	if path := os.Getenv("RELADOCK_ATOMIC_WRITE_CHILD"); path != "" {
		if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: 1, Max: 1}); err != nil {
			os.Exit(3)
		}
		if err := privateWrite(path, []byte("credential-longer-than-limit")); err == nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
	path := filepath.Join(t.TempDir(), "agent.crt")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestBootstrapAtomicPrivateWriteNeverPublishesTruncatedCredential$")
	cmd.Env = append(os.Environ(), "RELADOCK_ATOMIC_WRITE_CHILD="+path)
	_ = cmd.Run()
	if _, err = os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("interrupted private write published a truncated credential")
	}
	if err = privateWrite(path, []byte("complete")); err != nil {
		t.Fatal(err)
	}
	if err = privateWrite(path, []byte("replacement")); err == nil {
		t.Fatal("existing credential replaced")
	}
	b, _ := os.ReadFile(path)
	if string(b) != "complete" {
		t.Fatal("existing credential changed")
	}
}
