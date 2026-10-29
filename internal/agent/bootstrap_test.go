package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBootstrapProtectsExistingPrivateKeyAndRequiresHTTPS(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pki")
	os.Mkdir(dir, 0700)
	os.WriteFile(filepath.Join(dir, "agent.key"), []byte("existing"), 0600)
	err := Bootstrap(context.Background(), BootstrapOptions{Origin: "http://example.com", PKIDir: dir, Token: strings.NewReader("secret\n")})
	if err == nil {
		t.Fatal("plain HTTP allowed")
	}
	err = Bootstrap(context.Background(), BootstrapOptions{Origin: "https://example.com", PKIDir: dir, Token: strings.NewReader("secret\n")})
	if err == nil {
		t.Fatal("existing private key accepted")
	}
	b, _ := os.ReadFile(filepath.Join(dir, "agent.key"))
	if string(b) != "existing" {
		t.Fatal("existing key overwritten")
	}
}
