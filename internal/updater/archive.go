package updater

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	maxReleaseArchiveBytes = 128 << 20
	maxReleaseFileBytes    = 64 << 20
	maxReleaseTotalBytes   = 256 << 20
)

var ErrChecksumMismatch = errors.New("release checksum mismatch")

var releaseAllowlist = map[string]bool{
	"DEPLOYMENT.md": true, "RELEASE.json": true, "SHA256SUMS": true,
	"agent.env.example": true, "bootstrap-agent.sh": true, "control.env.example": true, "install-linux.sh": true,
	"reladock": true, "reladock-admin": true, "reladock-agent": true, "reladock-agent-apply.service": true,
	"reladock-agent.service": true, "reladock-dataplane": true, "reladock-updater": true,
	"reladock-updater.service": true, "reladock-updater.timer": true, "reladock.service": true,
}

type StagedBinaries struct {
	Agent     string
	Dataplane string
}

func safePrivateDirectory(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) == string(filepath.Separator) {
		return errors.New("unsafe staging directory")
	}
	if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("unsafe staging directory")
	}
	return nil
}

func verifyOuterArchive(path, expected string) error {
	if !hexSHA256.MatchString(expected) {
		return errors.New("invalid expected archive checksum")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > maxReleaseArchiveBytes {
		return errors.New("unsafe release archive")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	hash := sha256.New()
	if _, err = io.Copy(hash, io.LimitReader(f, maxReleaseArchiveBytes+1)); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != expected {
		return ErrChecksumMismatch
	}
	return nil
}

func VerifyArchive(path, expectedSHA256, destination string) (StagedBinaries, error) {
	result := StagedBinaries{}
	if err := verifyOuterArchive(path, expectedSHA256); err != nil {
		return result, err
	}
	if err := safePrivateDirectory(destination); err != nil {
		return result, err
	}
	f, err := os.Open(path)
	if err != nil {
		return result, err
	}
	defer f.Close()
	compressed := bufio.NewReader(f)
	gz, err := gzip.NewReader(compressed)
	if err != nil {
		return result, errors.New("invalid release compression")
	}
	defer gz.Close()
	gz.Multistream(false)
	reader := tar.NewReader(gz)
	seen := make(map[string]bool, len(releaseAllowlist))
	calculated := make(map[string]string, len(releaseAllowlist))
	var total int64
	for {
		header, nextErr := reader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return result, errors.New("invalid release archive")
		}
		name := header.Name
		if !releaseAllowlist[name] || seen[name] || filepath.Base(name) != name || strings.ContainsAny(name, `/\\`) || (header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA) || header.Size < 0 || header.Size > maxReleaseFileBytes {
			return result, errors.New("unsafe release archive entry")
		}
		total += header.Size
		if total > maxReleaseTotalBytes {
			return result, errors.New("release archive too large")
		}
		seen[name] = true
		mode := os.FileMode(0600)
		if name == "reladock-agent" || name == "reladock-dataplane" {
			mode = 0755
		}
		outputPath := filepath.Join(destination, name)
		out, openErr := os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if openErr != nil {
			return result, errors.New("unsafe staging destination")
		}
		hash := sha256.New()
		_, copyErr := io.CopyN(io.MultiWriter(out, hash), reader, header.Size)
		syncErr := out.Sync()
		closeErr := out.Close()
		if copyErr != nil || syncErr != nil || closeErr != nil {
			return result, errors.New("release extraction failed")
		}
		calculated[name] = hex.EncodeToString(hash.Sum(nil))
	}
	padding, readErr := io.ReadAll(io.LimitReader(gz, 1<<20))
	if readErr != nil || len(padding) >= 1<<20 {
		return result, errors.New("trailing release archive data")
	}
	for _, value := range padding {
		if value != 0 {
			return result, errors.New("trailing release archive data")
		}
	}
	if err = gz.Close(); err != nil {
		return result, errors.New("invalid release compression")
	}
	if _, err = compressed.Peek(1); !errors.Is(err, io.EOF) {
		return result, errors.New("trailing release compression data")
	}
	if len(seen) != len(releaseAllowlist) {
		return result, errors.New("incomplete release archive")
	}
	if err = verifyInnerChecksums(filepath.Join(destination, "SHA256SUMS"), calculated); err != nil {
		return result, err
	}
	result.Agent = filepath.Join(destination, "reladock-agent")
	result.Dataplane = filepath.Join(destination, "reladock-dataplane")
	return result, nil
}

func verifyInnerChecksums(path string, calculated map[string]string) error {
	f, err := os.Open(path)
	if err != nil {
		return errors.New("package checksums unavailable")
	}
	defer f.Close()
	seen := make(map[string]bool, len(releaseAllowlist)-1)
	scanner := bufio.NewScanner(io.LimitReader(f, maxReleaseFileBytes+1))
	scanner.Buffer(make([]byte, 1024), 4096)
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) < 67 || line[64:66] != "  " {
			return errors.New("invalid package checksums")
		}
		digest, name := line[:64], line[66:]
		if name == "SHA256SUMS" || !releaseAllowlist[name] || seen[name] || !hexSHA256.MatchString(digest) {
			return errors.New("invalid package checksums")
		}
		if calculated[name] != digest {
			return ErrChecksumMismatch
		}
		seen[name] = true
	}
	if scanner.Err() != nil || len(seen) != len(releaseAllowlist)-1 {
		return errors.New("incomplete package checksums")
	}
	return nil
}
