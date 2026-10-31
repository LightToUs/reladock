package recovery

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

const (
	maxArchiveEntries = 4096
	maxFileBytes      = int64(4 << 30)
	maxArchiveBytes   = int64(8 << 30)
)

type SourceFile struct {
	Path, Source string
	Mode         uint32
}

func WriteBundle(w io.Writer, base Manifest, sources []SourceFile) (Manifest, error) {
	if base.Version == 0 {
		base.Version = FormatVersion
	}
	base.Files = nil
	sources = append([]SourceFile(nil), sources...)
	sort.Slice(sources, func(i, j int) bool { return sources[i].Path < sources[j].Path })
	for _, s := range sources {
		if !cleanBundlePath(s.Path) || s.Path == "manifest.json" {
			return base, fmt.Errorf("unsafe recovery path %q", s.Path)
		}
		info, err := os.Lstat(s.Source)
		if err != nil {
			return base, err
		}
		if !info.Mode().IsRegular() {
			return base, fmt.Errorf("recovery source %q is not a regular file", s.Source)
		}
		if info.Size() > maxFileBytes {
			return base, fmt.Errorf("recovery source %q exceeds size limit", s.Source)
		}
		f, err := os.Open(s.Source)
		if err != nil {
			return base, err
		}
		h := sha256.New()
		_, copyErr := io.Copy(h, io.LimitReader(f, maxFileBytes+1))
		closeErr := f.Close()
		if copyErr != nil {
			return base, copyErr
		}
		if closeErr != nil {
			return base, closeErr
		}
		mode := s.Mode
		if mode == 0 {
			mode = uint32(info.Mode().Perm())
		}
		base.Files = append(base.Files, FileRecord{Path: s.Path, Mode: mode, Size: info.Size(), SHA256: hex.EncodeToString(h.Sum(nil))})
	}
	manifest, err := EncodeManifest(base)
	if err != nil {
		return base, err
	}
	tw := tar.NewWriter(w)
	if err := tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0600, Size: int64(len(manifest)), Typeflag: tar.TypeReg}); err != nil {
		return base, err
	}
	if _, err := tw.Write(manifest); err != nil {
		return base, err
	}
	for i, s := range sources {
		r := base.Files[i]
		if err := tw.WriteHeader(&tar.Header{Name: r.Path, Mode: int64(r.Mode), Size: r.Size, Typeflag: tar.TypeReg}); err != nil {
			return base, err
		}
		f, err := os.Open(s.Source)
		if err != nil {
			return base, err
		}
		_, copyErr := io.CopyN(tw, f, r.Size)
		closeErr := f.Close()
		if copyErr != nil {
			return base, copyErr
		}
		if closeErr != nil {
			return base, closeErr
		}
	}
	if err := tw.Close(); err != nil {
		return base, err
	}
	return base, nil
}

func ExtractBundle(r io.Reader, staging string, expected Kind) (m Manifest, err error) {
	if !filepath.IsAbs(staging) {
		return m, errors.New("staging path must be absolute")
	}
	if _, statErr := os.Lstat(staging); !os.IsNotExist(statErr) {
		return m, errors.New("staging path already exists")
	}
	if err = os.Mkdir(staging, 0700); err != nil {
		return m, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(staging)
		}
	}()
	tr := tar.NewReader(io.LimitReader(r, maxArchiveBytes+1))
	seen := map[string]FileRecord{}
	total := int64(0)
	var manifestData []byte
	for count := 0; ; count++ {
		if count >= maxArchiveEntries {
			return m, errors.New("recovery archive has too many entries")
		}
		h, nextErr := tr.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			return m, nextErr
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA {
			return m, fmt.Errorf("unsupported recovery entry type for %q", h.Name)
		}
		if !cleanBundlePath(h.Name) || h.Size < 0 || h.Size > maxFileBytes {
			return m, fmt.Errorf("unsafe recovery entry %q", h.Name)
		}
		if _, exists := seen[h.Name]; exists || (h.Name == "manifest.json" && manifestData != nil) {
			return m, fmt.Errorf("duplicate recovery entry %q", h.Name)
		}
		total += h.Size
		if total > maxArchiveBytes {
			return m, errors.New("recovery archive exceeds size limit")
		}
		if h.Name == "manifest.json" {
			manifestData, err = io.ReadAll(io.LimitReader(tr, maxFileBytes+1))
			if err != nil {
				return m, err
			}
			continue
		}
		destination := filepath.Join(staging, filepath.FromSlash(h.Name))
		if err = os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
			return m, err
		}
		f, createErr := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(h.Mode)&0777)
		if createErr != nil {
			return m, createErr
		}
		hash := sha256.New()
		_, copyErr := io.CopyN(io.MultiWriter(f, hash), tr, h.Size)
		syncErr := f.Sync()
		closeErr := f.Close()
		if copyErr != nil {
			return m, copyErr
		}
		if syncErr != nil {
			return m, syncErr
		}
		if closeErr != nil {
			return m, closeErr
		}
		seen[h.Name] = FileRecord{Path: h.Name, Mode: uint32(h.Mode) & 0777, Size: h.Size, SHA256: hex.EncodeToString(hash.Sum(nil))}
	}
	if manifestData == nil {
		return m, errors.New("recovery manifest is missing")
	}
	m, err = decodeManifest(manifestData)
	if err != nil {
		return m, err
	}
	if err = ValidateManifest(m, expected); err != nil {
		return m, err
	}
	if len(seen) != len(m.Files) {
		return m, errors.New("recovery archive file set does not match manifest")
	}
	for _, want := range m.Files {
		if got, exists := seen[want.Path]; !exists || got != want {
			return m, fmt.Errorf("recovery entry %q does not match manifest", want.Path)
		}
	}
	ok = true
	return m, nil
}
