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
	"syscall"
)

const (
	maxArchiveEntries = 4096
	maxFileBytes      = int64(4 << 30)
	maxArchiveBytes   = int64(8 << 30)
)

const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func modeOr(value, fallback uint32) uint32 {
	if value != 0 {
		return value
	}
	return fallback
}

func openRegularNoFollow(name string) (*os.File, os.FileInfo, error) {
	fd, err := syscall.Open(name, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, nil, fmt.Errorf("recovery source %q is not a regular file", name)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Nlink > 1 {
		_ = f.Close()
		return nil, nil, fmt.Errorf("recovery source %q is multiply linked", name)
	}
	return f, info, nil
}

type SourceFile struct {
	Path, Source string
	Mode         uint32
	Directory    bool
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
		if s.Directory && !info.IsDir() {
			return base, fmt.Errorf("recovery source %q is not a directory", s.Source)
		}
		if !s.Directory && !info.Mode().IsRegular() {
			return base, fmt.Errorf("recovery source %q is not a regular file", s.Source)
		}
		if info.Size() > maxFileBytes {
			return base, fmt.Errorf("recovery source %q exceeds size limit", s.Source)
		}
		if s.Directory {
			base.Files = append(base.Files, FileRecord{Path: s.Path, Type: "directory", Mode: modeOr(s.Mode, uint32(info.Mode().Perm())), Size: 0, SHA256: emptySHA256})
			continue
		}
		f, currentInfo, err := openRegularNoFollow(s.Source)
		if err != nil {
			return base, err
		}
		if !os.SameFile(info, currentInfo) {
			_ = f.Close()
			return base, fmt.Errorf("recovery source %q changed during collection", s.Source)
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
		base.Files = append(base.Files, FileRecord{Path: s.Path, Type: "file", Mode: mode, Size: info.Size(), SHA256: hex.EncodeToString(h.Sum(nil))})
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
		typeflag := byte(tar.TypeReg)
		if r.Type == "directory" {
			typeflag = tar.TypeDir
		}
		if err := tw.WriteHeader(&tar.Header{Name: r.Path, Mode: int64(r.Mode), Size: r.Size, Typeflag: typeflag}); err != nil {
			return base, err
		}
		if r.Type == "directory" {
			continue
		}
		f, currentInfo, err := openRegularNoFollow(s.Source)
		if err != nil {
			return base, err
		}
		if currentInfo.Size() != r.Size {
			_ = f.Close()
			return base, fmt.Errorf("recovery source %q changed during collection", s.Source)
		}
		hash := sha256.New()
		_, copyErr := io.CopyN(io.MultiWriter(tw, hash), f, r.Size)
		closeErr := f.Close()
		if copyErr != nil {
			return base, copyErr
		}
		if closeErr != nil {
			return base, closeErr
		}
		if hex.EncodeToString(hash.Sum(nil)) != r.SHA256 {
			return base, fmt.Errorf("recovery source %q changed during collection", s.Source)
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
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA && h.Typeflag != tar.TypeDir {
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
			if h.Typeflag == tar.TypeDir {
				return m, errors.New("recovery manifest is not a file")
			}
			manifestData, err = io.ReadAll(io.LimitReader(tr, maxFileBytes+1))
			if err != nil {
				return m, err
			}
			continue
		}
		destination := filepath.Join(staging, filepath.FromSlash(h.Name))
		if h.Typeflag == tar.TypeDir {
			if err = os.Mkdir(destination, os.FileMode(h.Mode)&0777); err != nil {
				return m, err
			}
			seen[h.Name] = FileRecord{Path: h.Name, Type: "directory", Mode: uint32(h.Mode) & 0777, Size: 0, SHA256: emptySHA256}
			continue
		}
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
		seen[h.Name] = FileRecord{Path: h.Name, Type: "file", Mode: uint32(h.Mode) & 0777, Size: h.Size, SHA256: hex.EncodeToString(hash.Sum(nil))}
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
