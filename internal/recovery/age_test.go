package recovery

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeAge(t *testing.T, fail bool) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "age")
	body := "#!/bin/sh\ncat\n"
	if fail {
		body = "#!/bin/sh\necho failed >&2\nexit 7\n"
	}
	if err := os.WriteFile(p, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEncryptDecryptNoOverwrite(t *testing.T) {
	dir := t.TempDir()
	age := fakeAge(t, false)
	path, err := Encrypt(context.Background(), age, dir, KindNode, "age1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqp5e5z", func(w io.Writer) error { _, e := w.Write([]byte("secret")); return e })
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
	identity := filepath.Join(t.TempDir(), "identity")
	if err := os.WriteFile(identity, []byte("AGE-SECRET-KEY-test"), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Decrypt(context.Background(), age, path, identity, func(r io.Reader) error { _, e := out.ReadFrom(r); return e }); err != nil {
		t.Fatal(err)
	}
	if out.String() != "secret" {
		t.Fatalf("output=%q", out.String())
	}
}

func TestEncryptFailureCleansPartialOutput(t *testing.T) {
	dir := t.TempDir()
	_, err := Encrypt(context.Background(), fakeAge(t, true), dir, KindControl, "age1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqp5e5z", func(w io.Writer) error { _, e := w.Write([]byte("secret")); return e })
	if err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("err=%v", err)
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("partial outputs remain: %v", entries)
	}
}

func TestAgeInputsFailClosed(t *testing.T) {
	dir := t.TempDir()
	if _, err := Encrypt(context.Background(), "age", dir, KindNode, "age1bad", func(io.Writer) error { return nil }); err == nil {
		t.Fatal("accepted relative age path")
	}
	link := filepath.Join(t.TempDir(), "destination")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Encrypt(context.Background(), fakeAge(t, false), link, KindNode, "age1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqp5e5z", func(io.Writer) error { return nil }); err == nil {
		t.Fatal("accepted destination symlink")
	}
}
