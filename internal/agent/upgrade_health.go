package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"reladock/internal/buildinfo"
)

const maxUpgradeHealthBytes = 64 << 10

var healthChallenge = regexp.MustCompile(`^[a-f0-9]{64}$`)

type upgradeHealthRequest struct {
	TaskID        string `json:"taskId"`
	Challenge     string `json:"challenge"`
	TargetVersion string `json:"targetVersion"`
}

type upgradeHealthMarker struct {
	TaskID        string    `json:"taskId"`
	Challenge     string    `json:"challenge"`
	TargetVersion string    `json:"targetVersion"`
	CompletedAt   time.Time `json:"completedAt"`
}

type UpgradeHealth struct {
	mu          sync.Mutex
	requestPath string
	markerDir   string
	version     string
	current     upgradeHealthRequest
	heartbeat   bool
	config      bool
}

func NewUpgradeHealth(requestPath, markerDir, version string) *UpgradeHealth {
	return &UpgradeHealth{requestPath: requestPath, markerDir: markerDir, version: version}
}

func (h *UpgradeHealth) HeartbeatSucceeded() error     { return h.record(true) }
func (h *UpgradeHealth) ConfigurationSucceeded() error { return h.record(false) }

func (h *UpgradeHealth) loadRequest() (*upgradeHealthRequest, error) {
	info, err := os.Lstat(h.requestPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0640 || info.Size() > maxUpgradeHealthBytes {
		return nil, errors.New("unsafe upgrade health request")
	}
	f, err := os.Open(h.requestPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var request upgradeHealthRequest
	decoder := json.NewDecoder(io.LimitReader(f, maxUpgradeHealthBytes+1))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&request); err != nil || decoder.Decode(new(any)) != io.EOF || !jobIDPattern.MatchString(request.TaskID) || !healthChallenge.MatchString(request.Challenge) {
		return nil, errors.New("invalid upgrade health request")
	}
	if _, _, _, ok := buildinfo.ParseProduction(request.TargetVersion); !ok {
		return nil, errors.New("invalid upgrade target version")
	}
	return &request, nil
}

func (h *UpgradeHealth) record(heartbeat bool) error {
	request, err := h.loadRequest()
	h.mu.Lock()
	defer h.mu.Unlock()
	if err != nil || request == nil || request.TargetVersion != h.version {
		h.current = upgradeHealthRequest{}
		h.heartbeat = false
		h.config = false
		return err
	}
	if h.current != *request {
		h.current = *request
		h.heartbeat = false
		h.config = false
	}
	if heartbeat {
		h.heartbeat = true
	} else {
		h.config = true
	}
	if !h.heartbeat || !h.config {
		return nil
	}
	return h.publish(upgradeHealthMarker{TaskID: request.TaskID, Challenge: request.Challenge, TargetVersion: request.TargetVersion, CompletedAt: time.Now().UTC()})
}

func (h *UpgradeHealth) publish(marker upgradeHealthMarker) error {
	if err := os.MkdirAll(h.markerDir, 0700); err != nil {
		return err
	}
	raw, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(h.markerDir, ".upgrade-health-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = io.Copy(f, bytes.NewReader(raw))
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), filepath.Join(h.markerDir, "upgrade-health.json")); err != nil {
		return err
	}
	dir, err := os.Open(h.markerDir)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
