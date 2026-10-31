package updater

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

var testReleaseFiles = []string{
	"DEPLOYMENT.md", "RELEASE.json", "agent.env.example", "bootstrap-agent.sh", "control.env.example", "install-linux.sh",
	"reladock", "reladock-admin", "reladock-agent", "reladock-agent-apply.service", "reladock-agent.service", "reladock-dataplane",
	"reladock-updater", "reladock-updater.service", "reladock-updater.timer", "reladock.service",
}

type archiveEntry struct {
	name     string
	typeflag byte
	content  string
	size     int64
	linkname string
}

func validArchiveEntries() []archiveEntry {
	entries := make([]archiveEntry, 0, len(testReleaseFiles)+1)
	hashes := make(map[string]string, len(testReleaseFiles))
	for _, name := range testReleaseFiles {
		content := "contents-of-" + name
		sum := sha256.Sum256([]byte(content))
		hashes[name] = hex.EncodeToString(sum[:])
		entries = append(entries, archiveEntry{name: name, typeflag: tar.TypeReg, content: content})
	}
	sort.Strings(testReleaseFiles)
	var sums strings.Builder
	for _, name := range testReleaseFiles {
		fmt.Fprintf(&sums, "%s  %s\n", hashes[name], name)
	}
	entries = append(entries, archiveEntry{name: "SHA256SUMS", typeflag: tar.TypeReg, content: sums.String()})
	return entries
}

func writeArchive(t *testing.T, entries []archiveEntry) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "release.tar.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, entry := range entries {
		size := entry.size
		if size == 0 && entry.typeflag == tar.TypeReg {
			size = int64(len(entry.content))
		}
		header := &tar.Header{Name: entry.name, Mode: 0600, Typeflag: entry.typeflag, Size: size, Linkname: entry.linkname}
		if err = tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if entry.content != "" {
			if _, err = tw.Write([]byte(entry.content)); err != nil {
				t.Fatal(err)
			}
		} else if size > 0 {
			zero := make([]byte, 32<<10)
			for remaining := size; remaining > 0; {
				chunk := int64(len(zero))
				if chunk > remaining {
					chunk = remaining
				}
				if _, err = tw.Write(zero[:int(chunk)]); err != nil {
					t.Fatal(err)
				}
				remaining -= chunk
			}
		}
	}
	if err = tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err = gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	sum := sha256.Sum256(raw)
	return path, hex.EncodeToString(sum[:])
}

func TestVerifyArchiveStagesOnlyVerifiedBinaries(t *testing.T) {
	path, digest := writeArchive(t, validArchiveEntries())
	destination := filepath.Join(t.TempDir(), "staged")
	staged, err := VerifyArchive(path, digest, destination)
	if err != nil {
		t.Fatal(err)
	}
	for _, binary := range []string{staged.Agent, staged.Dataplane} {
		info, statErr := os.Lstat(binary)
		if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0755 {
			t.Fatalf("staged binary %q: %v %+v", binary, statErr, info)
		}
	}
	if raw, _ := os.ReadFile(staged.Agent); string(raw) != "contents-of-reladock-agent" {
		t.Fatalf("wrong staged Agent: %q", raw)
	}
}

func TestVerifyArchiveRejectsHostileEntriesAndChecksums(t *testing.T) {
	tests := []struct {
		name   string
		mutate func([]archiveEntry) []archiveEntry
	}{
		{name: "traversal", mutate: func(v []archiveEntry) []archiveEntry { v[0].name = "../escape"; return v }},
		{name: "nested", mutate: func(v []archiveEntry) []archiveEntry { v[0].name = "dir/file"; return v }},
		{name: "duplicate", mutate: func(v []archiveEntry) []archiveEntry { return append(v, v[0]) }},
		{name: "extra", mutate: func(v []archiveEntry) []archiveEntry {
			return append(v, archiveEntry{name: "extra", typeflag: tar.TypeReg, content: "x"})
		}},
		{name: "missing", mutate: func(v []archiveEntry) []archiveEntry { return v[1:] }},
		{name: "symlink", mutate: func(v []archiveEntry) []archiveEntry {
			v[0].typeflag = tar.TypeSymlink
			v[0].linkname = "reladock-agent"
			v[0].content = ""
			return v
		}},
		{name: "hardlink", mutate: func(v []archiveEntry) []archiveEntry {
			v[0].typeflag = tar.TypeLink
			v[0].linkname = "reladock-agent"
			v[0].content = ""
			return v
		}},
		{name: "device", mutate: func(v []archiveEntry) []archiveEntry { v[0].typeflag = tar.TypeChar; v[0].content = ""; return v }},
		{name: "directory", mutate: func(v []archiveEntry) []archiveEntry { v[0].typeflag = tar.TypeDir; v[0].content = ""; return v }},
		{name: "oversized", mutate: func(v []archiveEntry) []archiveEntry {
			v[0].size = maxReleaseFileBytes + 1
			v[0].content = ""
			return v
		}},
		{name: "bad inner checksum", mutate: func(v []archiveEntry) []archiveEntry {
			v[len(v)-1].content = strings.Repeat("0", 64) + "  reladock-agent\n"
			return v
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path, digest := writeArchive(t, test.mutate(validArchiveEntries()))
			if _, err := VerifyArchive(path, digest, filepath.Join(t.TempDir(), "staged")); err == nil {
				t.Fatal("hostile archive accepted")
			}
		})
	}
	path, digest := writeArchive(t, validArchiveEntries())
	if _, err := VerifyArchive(path, strings.Repeat("0", 64), filepath.Join(t.TempDir(), "outer")); err == nil || digest == strings.Repeat("0", 64) {
		t.Fatal("outer checksum mismatch accepted")
	}
}

func TestVerifyArchiveRejectsUnsafeDestinationEntry(t *testing.T) {
	path, digest := writeArchive(t, validArchiveEntries())
	destination := filepath.Join(t.TempDir(), "staged")
	if err := os.Mkdir(destination, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/tmp", filepath.Join(destination, "reladock-agent")); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyArchive(path, digest, destination); err == nil {
		t.Fatal("pre-existing destination symlink accepted")
	}
}
