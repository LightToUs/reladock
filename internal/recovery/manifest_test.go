package recovery

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestManifestCanonicalAndValidated(t *testing.T) {
	m := Manifest{Version: FormatVersion, Kind: KindNode, CreatedAt: time.Date(2026, 10, 31, 1, 2, 3, 0, time.UTC), SourceID: "node-1", Files: []FileRecord{
		{Path: "state/z", Mode: 0600, Size: 1, SHA256: strings.Repeat("a", 64)},
		{Path: "state/a", Mode: 0640, Size: 2, SHA256: strings.Repeat("b", 64)},
	}}
	b, err := EncodeManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Index(string(b), "state/a") > strings.Index(string(b), "state/z") {
		t.Fatalf("files not sorted: %s", b)
	}
	if err := ValidateManifest(m, KindNode); err != nil {
		t.Fatal(err)
	}
	m.Files[1].Path = "../escape"
	if err := ValidateManifest(m, KindNode); err == nil {
		t.Fatal("accepted traversal")
	}
}

func TestExtractBundleRejectsTraversalAndDoesNotOverwrite(t *testing.T) {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	if err := tw.WriteHeader(&tar.Header{Name: "../escape", Mode: 0600, Size: 1, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ExtractBundle(&b, filepath.Join(t.TempDir(), "stage"), KindNode); err == nil {
		t.Fatal("accepted traversal archive")
	}

	existing := filepath.Join(t.TempDir(), "existing")
	if err := os.Mkdir(existing, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := ExtractBundle(bytes.NewReader(nil), existing, KindNode); err == nil {
		t.Fatal("accepted existing staging directory")
	}
}

func TestBundleRoundTripRejectsHashMismatch(t *testing.T) {
	source := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(source, []byte("safe"), 0600); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	m := Manifest{Version: FormatVersion, Kind: KindNode, CreatedAt: time.Now().UTC(), SourceID: "node-1"}
	if _, err := WriteBundle(&b, m, []SourceFile{{Path: "state/agent.json", Source: source, Mode: 0600}}); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(t.TempDir(), "stage")
	got, err := ExtractBundle(bytes.NewReader(b.Bytes()), stage, KindNode)
	if err != nil {
		t.Fatal(err)
	}
	if got.SourceID != "node-1" {
		t.Fatalf("source = %q", got.SourceID)
	}
	data, err := os.ReadFile(filepath.Join(stage, "state", "agent.json"))
	if err != nil || string(data) != "safe" {
		t.Fatalf("data=%q err=%v", data, err)
	}

	corrupt := bytes.Replace(b.Bytes(), []byte("safe"), []byte("evil"), 1)
	if _, err := ExtractBundle(bytes.NewReader(corrupt), filepath.Join(t.TempDir(), "bad"), KindNode); err == nil {
		t.Fatal("accepted hash mismatch")
	}
}
