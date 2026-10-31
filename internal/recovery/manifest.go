package recovery

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

const FormatVersion = 1

type Kind string

const (
	KindControl Kind = "control"
	KindNode    Kind = "node"
)

type FileRecord struct {
	Path   string `json:"path"`
	Type   string `json:"type"`
	Mode   uint32 `json:"mode"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type Manifest struct {
	Version    int               `json:"version"`
	Kind       Kind              `json:"kind"`
	CreatedAt  time.Time         `json:"createdAt"`
	SourceID   string            `json:"sourceId"`
	Invariants map[string]string `json:"invariants,omitempty"`
	Files      []FileRecord      `json:"files"`
}

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func cleanBundlePath(name string) bool {
	return name != "" && name != "." && !strings.Contains(name, "\\") && !strings.HasPrefix(name, "/") && path.Clean(name) == name && !strings.HasPrefix(name, "../")
}

func ValidateManifest(m Manifest, expected Kind) error {
	if m.Version != FormatVersion {
		return fmt.Errorf("unsupported recovery format version %d", m.Version)
	}
	if m.Kind != expected || (m.Kind != KindControl && m.Kind != KindNode) {
		return fmt.Errorf("unexpected recovery bundle kind %q", m.Kind)
	}
	if m.CreatedAt.IsZero() || m.CreatedAt.Location() != time.UTC {
		return errors.New("createdAt must be a UTC timestamp")
	}
	if strings.TrimSpace(m.SourceID) == "" || len(m.SourceID) > 256 {
		return errors.New("invalid sourceId")
	}
	seen := map[string]struct{}{}
	for _, f := range m.Files {
		if !cleanBundlePath(f.Path) || f.Path == "manifest.json" {
			return fmt.Errorf("unsafe recovery path %q", f.Path)
		}
		if _, ok := seen[f.Path]; ok {
			return fmt.Errorf("duplicate recovery path %q", f.Path)
		}
		seen[f.Path] = struct{}{}
		if (f.Type != "file" && f.Type != "directory") || f.Mode&^0777 != 0 || f.Size < 0 || !sha256Pattern.MatchString(f.SHA256) || (f.Type == "directory" && f.Size != 0) {
			return fmt.Errorf("invalid recovery metadata for %q", f.Path)
		}
	}
	return nil
}

func EncodeManifest(m Manifest) ([]byte, error) {
	m.Files = append([]FileRecord(nil), m.Files...)
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Path < m.Files[j].Path })
	if err := ValidateManifest(m, m.Kind); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(m); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func decodeManifest(data []byte) (Manifest, error) {
	var m Manifest
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&m); err != nil {
		return m, fmt.Errorf("decode recovery manifest: %w", err)
	}
	return m, nil
}
