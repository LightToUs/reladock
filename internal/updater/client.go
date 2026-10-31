package updater

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"reladock/internal/buildinfo"
	"reladock/internal/platform"
)

const maxControlResponseBytes = 64 << 10

var ErrReportRejected = errors.New("upgrade report rejected")

var (
	upgradeID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	hexSHA256 = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

type Client struct {
	HTTP    *http.Client
	BaseURL string
}

func (c Client) endpoint(path string) (string, error) {
	if c.HTTP == nil {
		return "", errors.New("mTLS client required")
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("invalid control origin")
	}
	return c.BaseURL + path, nil
}

func (c Client) post(ctx context.Context, path string, value any) (int, []byte, error) {
	endpoint, err := c.endpoint(path)
	if err != nil {
		return 0, nil, err
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := *c.HTTP
	if client.Timeout <= 0 || client.Timeout > 20*time.Second {
		client.Timeout = 20 * time.Second
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		return 0, nil, errors.New("control request unavailable")
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxControlResponseBytes+1))
	if err != nil || len(raw) > maxControlResponseBytes {
		return 0, nil, errors.New("control response invalid")
	}
	return response.StatusCode, raw, nil
}

func (c Client) Claim(ctx context.Context, arch, current string) (*platform.UpgradeClaim, error) {
	if arch != "amd64" && arch != "arm64" {
		return nil, errors.New("unsupported architecture")
	}
	if _, _, _, ok := buildinfo.ParseProduction(current); !ok {
		return nil, errors.New("invalid current version")
	}
	status, raw, err := c.post(ctx, "/agent/upgrade/claim", struct {
		Arch           string `json:"arch"`
		CurrentVersion string `json:"currentVersion"`
	}{arch, current})
	if err != nil {
		return nil, err
	}
	if status == http.StatusNoContent {
		return nil, nil
	}
	if status != http.StatusOK {
		return nil, errors.New("upgrade claim rejected")
	}
	var claim platform.UpgradeClaim
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&claim); err != nil || decoder.Decode(new(any)) != io.EOF || !upgradeID.MatchString(claim.ID) || !buildinfo.CanUpgrade(current, claim.TargetVersion) || claim.PreviousVersion != current || !hexSHA256.MatchString(claim.SHA256) || !hexSHA256.MatchString(claim.LeaseToken) {
		return nil, errors.New("upgrade claim response invalid")
	}
	return &claim, nil
}

func (c Client) Report(ctx context.Context, report platform.UpgradeReport) error {
	if !upgradeID.MatchString(report.ID) || !hexSHA256.MatchString(report.LeaseToken) {
		return errors.New("invalid upgrade report")
	}
	status, _, err := c.post(ctx, "/agent/upgrade/report", report)
	if err != nil {
		return err
	}
	if status == http.StatusUnauthorized || status == http.StatusConflict || status == http.StatusGone {
		return ErrReportRejected
	}
	if status != http.StatusOK {
		return errors.New("upgrade report rejected")
	}
	return nil
}

func ReleaseAssetURL(version, arch string) (string, error) {
	if _, _, _, ok := buildinfo.ParseProduction(version); !ok || (arch != "amd64" && arch != "arm64") {
		return "", errors.New("invalid release asset")
	}
	return fmt.Sprintf("https://github.com/LightToUs/reladock/releases/download/%s/reladock-%s-linux-%s.tar.gz", version, version, arch), nil
}

func DownloadRelease(ctx context.Context, client *http.Client, version, arch, destination string) error {
	asset, err := ReleaseAssetURL(version, arch)
	if err != nil {
		return err
	}
	return downloadReleaseFromURL(ctx, client, asset, destination)
}

func downloadReleaseFromURL(ctx context.Context, source *http.Client, asset, destination string) error {
	if source == nil || !filepath.IsAbs(destination) {
		return errors.New("invalid release download")
	}
	client := *source
	if client.Timeout <= 0 || client.Timeout > 5*time.Minute {
		client.Timeout = 5 * time.Minute
	}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		host := req.URL.Hostname()
		if len(via) >= 5 || req.URL.Scheme != "https" || (host != "github.com" && !strings.HasSuffix(host, ".githubusercontent.com")) {
			return errors.New("unsafe release redirect")
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, asset, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(req)
	if err != nil {
		return errors.New("release download unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.ContentLength > maxReleaseArchiveBytes {
		return errors.New("release download rejected")
	}
	dir := filepath.Dir(destination)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".download-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		var written int64
		written, err = io.Copy(f, io.LimitReader(response.Body, maxReleaseArchiveBytes+1))
		if err == nil && written > maxReleaseArchiveBytes {
			err = errors.New("release download too large")
		}
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
	if err = os.Rename(f.Name(), destination); err != nil {
		return err
	}
	return syncDirectory(dir)
}
