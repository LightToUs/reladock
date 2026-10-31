package recovery

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const maxCommandErrorBytes = 8192

var ageRecipientPattern = regexp.MustCompile(`^age1[a-z0-9]{20,100}$`)

type limitBuffer struct{ b []byte }

func (w *limitBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remain := maxCommandErrorBytes - len(w.b)
	if remain > 0 {
		if len(p) > remain {
			p = p[:remain]
		}
		w.b = append(w.b, p...)
	}
	return n, nil
}
func (w *limitBuffer) String() string { return strings.TrimSpace(string(w.b)) }

func regularAbsolute(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("path must be absolute")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("path must name a regular file")
	}
	return nil
}

func safeDirectory(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("directory must be absolute")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("destination must be a real directory")
	}
	return nil
}

func createBundleFile(directory string, kind Kind) (*os.File, string, error) {
	for i := 0; i < 20; i++ {
		var random [8]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, "", err
		}
		name := fmt.Sprintf("reladock-%s-%s-%s.age", kind, time.Now().UTC().Format("20060102T150405Z"), hex.EncodeToString(random[:]))
		path := filepath.Join(directory, name)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err == nil {
			return f, path, nil
		}
		if !os.IsExist(err) {
			return nil, "", err
		}
	}
	return nil, "", errors.New("could not allocate unique recovery bundle name")
}

func Encrypt(ctx context.Context, ageBinary, destination string, kind Kind, recipient string, produce func(io.Writer) error) (output string, err error) {
	if err = regularAbsolute(ageBinary); err != nil {
		return "", fmt.Errorf("invalid age binary: %w", err)
	}
	if err = safeDirectory(destination); err != nil {
		return "", err
	}
	if !ageRecipientPattern.MatchString(recipient) {
		return "", errors.New("unsupported age recipient")
	}
	f, output, err := createBundleFile(destination, kind)
	if err != nil {
		return "", err
	}
	cleanupPath := output
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(cleanupPath)
		}
	}()
	pr, pw := io.Pipe()
	cmd := exec.CommandContext(ctx, ageBinary, "--encrypt", "--recipient", recipient)
	cmd.Stdin = pr
	cmd.Stdout = f
	var stderr limitBuffer
	cmd.Stderr = &stderr
	if err = cmd.Start(); err != nil {
		_ = pr.Close()
		_ = pw.Close()
		_ = f.Close()
		return "", fmt.Errorf("start age: %w", err)
	}
	produceResult := make(chan error, 1)
	go func() { produceErr := produce(pw); _ = pw.CloseWithError(produceErr); produceResult <- produceErr }()
	waitErr := cmd.Wait()
	_ = pr.Close()
	produceErr := <-produceResult
	if produceErr != nil {
		_ = f.Close()
		return "", fmt.Errorf("build recovery bundle: %w", produceErr)
	}
	if waitErr != nil {
		_ = f.Close()
		return "", fmt.Errorf("age encryption failed: %s", stderr.String())
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	d, openErr := os.Open(destination)
	if openErr != nil {
		return "", openErr
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	if syncErr != nil {
		return "", syncErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	keep = true
	return output, nil
}

func Decrypt(ctx context.Context, ageBinary, bundle, identity string, consume func(io.Reader) error) error {
	if err := regularAbsolute(ageBinary); err != nil {
		return fmt.Errorf("invalid age binary: %w", err)
	}
	if err := regularAbsolute(bundle); err != nil {
		return fmt.Errorf("invalid recovery bundle: %w", err)
	}
	if err := regularAbsolute(identity); err != nil {
		return fmt.Errorf("invalid age identity: %w", err)
	}
	f, err := os.Open(bundle)
	if err != nil {
		return err
	}
	defer f.Close()
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(childCtx, ageBinary, "--decrypt", "--identity", identity)
	cmd.Stdin = f
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr limitBuffer
	cmd.Stderr = &stderr
	if err = cmd.Start(); err != nil {
		return fmt.Errorf("start age: %w", err)
	}
	consumeErr := consume(stdout)
	if consumeErr != nil {
		cancel()
	}
	waitErr := cmd.Wait()
	if consumeErr != nil {
		return fmt.Errorf("read recovery bundle: %w", consumeErr)
	}
	if waitErr != nil {
		return fmt.Errorf("age decryption failed: %s", stderr.String())
	}
	return nil
}
